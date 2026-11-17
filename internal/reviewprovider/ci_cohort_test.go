package reviewprovider

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func cohortFixture(t *testing.T, mode string) (*ciObservationFixture, []CITarget) {
	t.Helper()
	f := &ciObservationFixture{t: t, mode: mode, counts: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/owner/repo/pulls/8" {
			r = r.Clone(r.Context())
			u := *r.URL
			r.URL = &u
			r.URL.Path = candidatePullPath
			rec := httptest.NewRecorder()
			f.serve(rec, r)
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			_, _ = w.Write(bytes.ReplaceAll(rec.Body.Bytes(), []byte(`"number":7`), []byte(`"number":8`)))
			return
		}
		f.serve(w, r)
	}))
	t.Cleanup(server.Close)
	f.g = testGitHub(server, "synthetic-ci-observation-token")
	a := ciObservationTarget()
	b := a
	b.PullNumber = 8
	return f, []CITarget{a, b}
}

func TestCICohortInitialAuthoritySavingsAndIndependentBrackets(t *testing.T) {
	ordinary, targets := cohortFixture(t, "")
	for _, target := range targets {
		o, e := ordinary.g.ObserveCIGate(t.Context(), target, "main")
		if e != nil || o.State != "satisfied" {
			t.Fatal(o.State, e)
		}
	}
	before, bcounts := ordinary.snapshot()
	f, targets := cohortFixture(t, "")
	c, e := f.g.NewCIGateCohort(t.Context(), targets, "main")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	var start time.Time
	for _, target := range targets {
		o, e := c.ObserveCIGate(t.Context(), target, "main")
		if e != nil || o.State != "satisfied" {
			t.Fatal(o.State, e)
		}
		if start.IsZero() {
			start = o.StartedAt
		} else if !start.Equal(o.StartedAt) {
			t.Fatal("enclosing start relabeled")
		}
		o.Authority.Checks[0].Context = "caller mutation"
	}
	after, counts := f.snapshot()
	for path, n := range bcounts {
		want := n
		if path == "/repos/owner/repo/branches/main/protection" || path == "/repos/owner/repo/rules/branches/main" {
			want--
		}
		if counts[path] != want {
			t.Fatalf("%s count %d want %d", path, counts[path], want)
		}
	}
	if len(before)-len(after) != 2 {
		t.Fatal(len(before), len(after))
	}
	t.Logf("two green targets ordinary=%d cohort=%d; initial authority only", len(before), len(after))
	if _, e := c.ObserveCIGate(t.Context(), targets[0], "main"); e == nil {
		t.Fatal("reused member")
	}
}

func TestCICohortEnrollmentAndCancellation(t *testing.T) {
	f, targets := cohortFixture(t, "")
	for _, bad := range [][]CITarget{nil, {targets[0], targets[0]}, make([]CITarget, 9)} {
		if c, e := f.g.NewCIGateCohort(t.Context(), bad, "main"); e == nil {
			c.Close()
			t.Fatal("invalid enrollment accepted")
		}
	}
	c, e := f.g.NewCIGateCohort(t.Context(), targets, "main")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	changed := targets[0]
	changed.HeadSHA = candidateBase
	if _, e = c.ObserveCIGate(t.Context(), changed, "main"); e == nil {
		t.Fatal("late enrollment")
	}
	if _, e = c.ObserveCIGate(t.Context(), targets[0], "other"); e == nil {
		t.Fatal("gate changed")
	}
	c.Close()
	if _, e = c.ObserveCIGate(t.Context(), targets[0], "main"); e == nil {
		t.Fatal("closed cohort")
	}
	paths, _ := f.snapshot()
	if len(paths) != 0 {
		t.Fatal("invalid cohort issued requests")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d, e := f.g.NewCIGateCohort(ctx, targets, "main")
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	if _, e = d.ObserveCIGate(t.Context(), targets[1], "main"); e == nil {
		t.Fatal("parent cancellation ignored")
	}
}

func TestCICohortPreservesMovementRefusals(t *testing.T) {
	for _, mode := range []string{"move_authority", "move_candidate", "move_results", "move_empty_merge"} {
		t.Run(mode, func(t *testing.T) {
			f, targets := cohortFixture(t, mode)
			c, e := f.g.NewCIGateCohort(t.Context(), targets, "main")
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			o, _ := c.ObserveCIGate(t.Context(), targets[0], "main")
			if o.State == "satisfied" {
				t.Fatal("movement credited", mode)
			}
		})
	}
}

func TestCICohortIsolationBudgetAndConcurrentRefusal(t *testing.T) {
	f, targets := cohortFixture(t, "")
	mixed := append([]CITarget(nil), targets...)
	mixed[1].RepositoryID++
	if _, e := f.g.NewCIGateCohort(t.Context(), mixed, "main"); e == nil {
		t.Fatal("mixed repository")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c, e := f.g.NewCIGateCohort(ctx, targets, "main")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	deadline, ok := c.ctx.Deadline()
	if !ok || time.Until(deadline) > CIObservationTimeout {
		t.Fatal("unbounded cohort")
	}
	c.mu.Lock()
	_, e = c.ObserveCIGate(t.Context(), targets[0], "main")
	c.mu.Unlock()
	if e == nil {
		t.Fatal("concurrent call admitted")
	}
	if o, e := c.ObserveCIGate(t.Context(), targets[0], "main"); e != nil || o.State != "satisfied" {
		t.Fatal(o.State, e)
	}
	before, _ := f.snapshot()
	cancel()
	if _, e := c.ObserveCIGate(t.Context(), targets[1], "main"); e == nil {
		t.Fatal("cancelled second member")
	}
	after, _ := f.snapshot()
	if len(before) != len(after) {
		t.Fatal("cancelled member issued provider requests")
	}
	next, e := f.g.NewCIGateCohort(t.Context(), targets, "main")
	if e != nil {
		t.Fatal(e)
	}
	defer next.Close()
	if o, e := next.ObserveCIGate(t.Context(), targets[0], "main"); e != nil || o.State != "satisfied" {
		t.Fatal(o.State, e)
	}
	_, counts := f.snapshot()
	if counts["/repos/owner/repo/branches/main/protection"] != 4 {
		t.Fatal("new cohort reused old authority", counts)
	}
}

func TestCICohortMalformedInitialAuthorityNeverReachesCandidate(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/repos/owner/repo/branches/main/protection" {
			t.Errorf("unexpected request after malformed authority: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"required_status_checks":{"strict":"invalid","contexts":[]}}`))
	}))
	defer server.Close()
	g := testGitHub(server, "synthetic-malformed-token")
	target := ciObservationTarget()
	other := target
	other.PullNumber++
	cohort, err := g.NewCIGateCohort(t.Context(), []CITarget{target, other}, "main")
	if err != nil {
		t.Fatal(err)
	}
	defer cohort.Close()
	for _, member := range []CITarget{target, other} {
		observation, err := cohort.ObserveCIGate(t.Context(), member, "main")
		if err == nil || observation.State == "satisfied" || observation.State == "not_required" || observation.Candidate != nil {
			t.Fatalf("malformed authority admitted target: %+v %v", observation, err)
		}
	}
	if calls != 1 {
		t.Fatalf("malformed initial authority reread or advanced: %d", calls)
	}
}

func TestCICohortSeparateCredentialAdaptersNeverShareAuthority(t *testing.T) {
	fixtures := []*ciObservationFixture{{t: t, counts: map[string]int{}}, {t: t, counts: map[string]int{}}}
	tokens := []string{"synthetic-credential-a", "synthetic-credential-b"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i, token := range tokens {
			if r.Header.Get("Authorization") == "Bearer "+token {
				synthetic := r.Clone(r.Context())
				synthetic.Header = r.Header.Clone()
				synthetic.Header.Set("Authorization", "Bearer synthetic-ci-observation-token")
				fixtures[i].serve(w, synthetic)
				return
			}
		}
		t.Error("unexpected credential identity")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	for i, token := range tokens {
		g := testGitHub(server, token)
		c, err := g.NewCIGateCohort(t.Context(), []CITarget{ciObservationTarget()}, "main")
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
		c.Close()
		if err != nil || got.State != "satisfied" {
			t.Fatal(got.State, err)
		}
		_, counts := fixtures[i].snapshot()
		if counts["/repos/owner/repo/branches/main/protection"] != 2 || counts["/repos/owner/repo/rules/branches/main"] != 2 {
			t.Fatalf("adapter %d skipped its own authority: %v", i, counts)
		}
	}
}
