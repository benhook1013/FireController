package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func laneDiagnosticFrame(t *testing.T, f *attemptFixture, channel string, plans []SourcePlan) {
	t.Helper()
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		c, rev, err := readConfiguration(ctx, tx, attemptProject)
		if err != nil {
			return err
		}
		return writeFrontFrame(ctx, tx, Run{ProjectID: attemptProject, Channel: channel, ConfigurationRevision: rev}, 801, c, runChoice{prefix: plans})
	})
	if err != nil {
		t.Fatal(err)
	}
}
func laneDiagnosticPlan(n int64) SourcePlan {
	p := schedulingPull(n)
	return SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "acme/repo", DefaultBranch: "main"}, Pull: p, ParentSHA: p.BaseSHA, ObservedAt: stamp()}
}
func TestLaneDiagnosticIndependentAndStackedReviewProgress(t *testing.T) {
	for _, dependent := range []bool{false, true} {
		t.Run(map[bool]string{false: "independent", true: "dependent"}[dependent], func(t *testing.T) {
			f := schedulingFixture(t)
			// A genuine completed native CLI capture still awaiting adjudication holds
			// its own PR, even after a later head or a closed allowance.
			roundCapture(t, f, 1, false)
			one := int64(1)
			if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "diagnostic-stack", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
				t.Fatal(err)
			}
			p2, p4 := laneDiagnosticPlan(2), laneDiagnosticPlan(4)
			if dependent {
				p4.ParentPullNumber = 2
				p4.ParentSHA = p2.Pull.HeadSHA
				p4.Pull.BaseSHA = p2.Pull.HeadSHA
				p4.Pull.BaseRef = "branch-two"
			}
			laneDiagnosticFrame(t, f, "cli", []SourcePlan{p2, p4})
			before := selectionCounts(t, f)
			out, err := f.e.ReadLaneDiagnostic(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), "cli")
			if err != nil || !out.SourceComplete || len(out.Candidates) != 2 {
				t.Fatal(out, err)
			}
			if out.Candidates[0].LocalReason != "review_evidence_pending" {
				t.Fatal(out)
			}
			later := out.Candidates[1]
			if later.LocalState != "locally_ready" || later.BlockingPullNumber != 0 {
				t.Fatal(later)
			}
			// The exact shared selector agrees, rather than a duplicated approximation.
			err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				got, reason, err := f.e.runFront(ctx, tx, Run{ProjectID: attemptProject, Channel: "cli"}, 801, []SourcePlan{p2, p4})
				if err != nil {
					return err
				}
				if got != 4 || reason != "" {
					t.Fatal(got, reason)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if selectionCounts(t, f) != before {
				t.Fatal("read mutated selection, provider, event or receipt state")
			}
		})
	}
}
func TestLaneDiagnosticSourceBoundariesAndHTTP(t *testing.T) {
	f := schedulingFixture(t)
	scope := selectionScope(t, f, attemptProject, f.reader, "read")
	before := selectionCounts(t, f)
	out, err := f.e.ReadLaneDiagnostic(t.Context(), scope, "cli")
	if err != nil || out.SourceComplete || out.SourceReason != "source_unavailable" || out.Candidates[1].LocalState != "unknown" {
		t.Fatal(out, err)
	}
	laneDiagnosticFrame(t, f, "cli", []SourcePlan{laneDiagnosticPlan(2)})
	out, err = f.e.ReadLaneDiagnostic(t.Context(), scope, "cli")
	if err != nil || out.SourceComplete || out.Candidates[1].LocalReason != "source_not_observed" {
		t.Fatal(out, err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	w := runHTTPCall(t, f, mux, "GET", "/api/v1/projects/"+attemptProject+"/reviews/lanes/cli/diagnostics", "", "", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Candidates) != 2 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, suffix := range []string{"manual/diagnostics", "cli/diagnostics?refresh=true"} {
		if w = runHTTPCall(t, f, mux, "GET", "/api/v1/projects/"+attemptProject+"/reviews/lanes/"+suffix, "", "", ""); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if selectionCounts(t, f) != before {
		t.Fatal("diagnostic changed state")
	}
	one := int64(1)
	if _, err = f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "changed-queue", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{4, 2}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	out, err = f.e.ReadLaneDiagnostic(t.Context(), scope, "cli")
	if err != nil || out.SourceReason != "configuration_changed" || out.SourceComplete || out.Candidates[0].HeadSHA != "" {
		t.Fatal(out, err)
	}
	// Source decode damage remains an error, never a healthy empty lane.
	admissionSQL(t, f, `UPDATE review_front_frames SET prefix_json=?`, strings.Repeat("x", 128*1024+1))
	if _, err = f.e.ReadLaneDiagnostic(t.Context(), scope, "cli"); err == nil {
		t.Fatal("damaged frame accepted")
	}
}

// Many ready siblings share one long completed ancestry. This exercises the
// advertised 100-row bound inside the real two-second Store.Read deadline.
func TestLaneDiagnosticMaximumSharedCompletedAncestry(t *testing.T) {
	queue := make([]int64, 100)
	for i := range queue {
		queue[i] = int64(i + 1)
	}
	f := schedulingFixtureWithQueue(t, queue)
	one := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "maximum-stack", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: "stacked", Queue: queue, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	plans := make([]SourcePlan, len(queue))
	zero := int64(0)
	for i, n := range queue {
		plans[i] = laneDiagnosticPlan(n)
		if i > 0 {
			parent := int64(i)
			if i >= 50 {
				parent = 50
			}
			plans[i].ParentPullNumber = parent
			plans[i].ParentSHA = strings.Repeat("a", 40)
			plans[i].Pull.BaseSHA = plans[i].ParentSHA
			plans[i].Pull.BaseRef = "ancestor"
		}
		if i < 50 {
			if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "complete-"+strconv.FormatInt(n, 10), n, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	laneDiagnosticFrame(t, f, "cli", plans)
	started := time.Now()
	out, err := f.e.ReadLaneDiagnostic(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), "cli")
	t.Logf("100-row shared ancestry read: %s", time.Since(started))
	if err != nil || !out.SourceComplete || len(out.Candidates) != 100 {
		t.Fatal(out, err)
	}
	for i, c := range out.Candidates {
		want := "complete"
		if i >= 50 {
			want = "locally_ready"
		}
		if c.LocalState != want {
			t.Fatal(i, c)
		}
	}
}

func TestLaneDiagnosticRepositoryChangeAndRevokedRead(t *testing.T) {
	f := schedulingFixture(t)
	scope := selectionScope(t, f, attemptProject, f.reader, "read")
	laneDiagnosticFrame(t, f, "cli", []SourcePlan{laneDiagnosticPlan(2), laneDiagnosticPlan(4)})
	admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
	out, err := f.e.ReadLaneDiagnostic(t.Context(), scope, "cli")
	if err != nil || out.SourceReason != "repository_changed" || out.LaneReason != "repository_changed" || out.SourceComplete {
		t.Fatal(out, err)
	}
	for _, c := range out.Candidates {
		if c.LocalState != "unknown" || c.HeadSHA != "" {
			t.Fatal(c)
		}
	}
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
	if _, err = f.e.ReadLaneDiagnostic(t.Context(), scope, "cli"); err == nil {
		t.Fatal("retained read scope bypassed credential revocation")
	}
}

func TestStackReviewSourceIdentityRemainsRequired(t *testing.T) {
	for _, cause := range []string{"parent_source_unavailable", "parent_moved", "parent_identity_changed", "pull_draft", "parent_unavailable", "parent_cycle"} {
		t.Run(cause, func(t *testing.T) {
			parent, child := laneDiagnosticPlan(2), laneDiagnosticPlan(4)
			child.ParentPullNumber, child.ParentSHA, child.Pull.BaseSHA = 2, parent.Pull.HeadSHA, parent.Pull.HeadSHA
			byNumber := map[int64]*SourcePlan{2: &parent, 4: &child}
			switch cause {
			case "parent_source_unavailable":
				delete(byNumber, 2)
			case "parent_moved":
				parent.Pull.HeadSHA = strings.Repeat("f", 40)
			case "parent_identity_changed":
				parent.Repository.ID++
			case "pull_draft":
				parent.SourceReason = cause
			case "parent_unavailable":
				parent.Pull.State = "closed"
			case "parent_cycle":
				parent.ParentPullNumber, parent.ParentSHA = 4, child.Pull.HeadSHA
			}
			calls := 0
			out, err := evaluateCandidateLocalState(&child, byNumber, func(p *SourcePlan) (evaluatedFront, error) {
				calls++
				if p.Pull.Number != 4 {
					t.Fatal("ancestor review state was evaluated")
				}
				return evaluatedFront{state: "ready", plan: p}, nil
			})
			if err != nil || out.state != "held" || out.blockingReason != cause || calls != 1 {
				t.Fatal("source identity was bypassed", out, calls, err)
			}
		})
	}
}

// Retain the source-free paused row through actual selection, then read it over
// the public service and strict client. This is not a hand-crafted HTTP reply.
func TestLaneDiagnosticRetainedPausedPlaceholderStrictClient(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{1, 2, 3})
			schedulingPause(t, f, 2, 0, true)
			receipt, _ := selectionEnqueue(t, f, "diagnostic-paused-placeholder", channel, nil)
			var calls, pausedCalls atomic.Int32
			reader := &refreshTestReader{pull: func(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
				calls.Add(1)
				if n == 2 {
					pausedCalls.Add(1)
				}
				p, err := selectionSnapshotReader{}.Pull(ctx, repo, n)
				if n == 1 {
					p.IsDraft = new(true)
				}
				return p, err
			}}
			selectionNext(t, f, reader)
			selected := selectionRead(t, f, receipt.ID)
			frame, found, err := readFrontFrame(t.Context(), f.store.DB(), attemptProject, channel)
			if err != nil || !found || frame.complete || len(frame.prefix) != 3 || frame.prefix[1].HeadSHA != "" || frame.prefix[1].ObservedAt != "" || frame.prefix[1].SourceReason != "pull_scheduling_paused" || selected.State != "selected" || pausedCalls.Load() != 0 {
				t.Fatal("selection did not retain the source-free paused placeholder", selected, frame, pausedCalls.Load(), err)
			}
			admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'diagnostic-reader','Reader')`, attemptProject)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
					return
				}
				f.handler.ServeHTTP(w, r)
			}))
			defer srv.Close()
			credential := filepath.Join(t.TempDir(), "reader.token")
			if err := os.WriteFile(credential, []byte(f.reader), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "fixture", ServerURL: srv.URL, InstanceID: f.store.InstanceID(), ProjectID: attemptProject, WorkerID: "diagnostic-reader"}, CredentialFile: credential})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			before, sourceCalls := selectionCounts(t, f), calls.Load()
			got, err := c.ReviewLaneDiagnostic(t.Context(), channel)
			if err != nil {
				t.Fatal("strict client rejected retained paused placeholder", err)
			}
			if got.SourceComplete || got.SourceConfigurationRevision != got.ConfigurationRevision || len(got.Candidates) != 3 {
				t.Fatal(got)
			}
			paused := got.Candidates[1]
			if paused.PullNumber != 2 || paused.HeadSHA != "" || paused.SourceObservedAt != "" || paused.ParentPullNumber != 0 || paused.LocalState != "held" || paused.LocalReason != "pull_scheduling_paused" || paused.BlockingPullNumber != 0 || paused.BlockingReason != "" {
				t.Fatal(paused)
			}
			if selectionCounts(t, f) != before || calls.Load() != sourceCalls || pausedCalls.Load() != 0 {
				t.Fatal("diagnostic changed durable state or contacted the provider")
			}
		})
	}
}

func TestLaneDiagnosticHostedPreChildSelection(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "diagnostic-hosted", "hosted", nil)
	scope := selectionScope(t, f, attemptProject, f.reader, "read")
	check := func(state string) {
		t.Helper()
		before := selectionCounts(t, f)
		out, err := f.e.ReadLaneDiagnostic(t.Context(), scope, "hosted")
		if err != nil {
			t.Fatal(err)
		}
		run := selectionRead(t, f, receipt.ID)
		if out.PendingSelection == nil || *out.PendingSelection != (LanePendingSelection{ID: run.ID, State: state, CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt}) {
			t.Fatal(out.PendingSelection, run)
		}
		if selectionCounts(t, f) != before {
			t.Fatal("diagnostic mutated journals")
		}
		cli, err := f.e.ReadLaneDiagnostic(t.Context(), scope, "cli")
		if err != nil || cli.PendingSelection != nil {
			t.Fatal(cli, err)
		}
	}
	check("queued")
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		r, err := readRun(ctx, tx, attemptProject, receipt.ID)
		if err != nil {
			return err
		}
		return updateRun(ctx, tx, r, "selecting", "", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	check("selecting")
	// Reset the synthetic claim so the real selector can create its child.
	if _, err := f.store.DB().Exec(`UPDATE review_selections SET state='queued' WHERE id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	reader := &refreshTestReader{}
	selectionNext(t, f, reader)
	run := selectionRead(t, f, receipt.ID)
	if run.State != "selected" || run.OperationID == "" {
		t.Fatal(run)
	}
	before, calls := selectionCounts(t, f), reader.calls.Load()
	out, err := f.e.ReadLaneDiagnostic(t.Context(), scope, "hosted")
	if err != nil || out.PendingSelection != nil {
		t.Fatal(out, err)
	}
	raw, err := json.Marshal(out)
	if err != nil || strings.Contains(string(raw), "pending_selection") {
		t.Fatal(string(raw), err)
	}
	if selectionCounts(t, f) != before || reader.calls.Load() != calls {
		t.Fatal("diagnostic performed work")
	}
}

func TestLaneDiagnosticRejectsCorruptPendingSelection(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "diagnostic-corrupt", "hosted", nil)
	if _, err := f.store.DB().Exec(`UPDATE review_selections SET updated_at='not-a-time' WHERE id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	before := selectionCounts(t, f)
	if _, err := f.e.ReadLaneDiagnostic(t.Context(), selectionScope(t, f, attemptProject, f.reader, "read"), "hosted"); err == nil {
		t.Fatal("corrupt retained selection became an empty healthy lane")
	}
	if selectionCounts(t, f) != before {
		t.Fatal("diagnostic repaired the journal")
	}
}
