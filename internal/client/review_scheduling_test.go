package client_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewSchedulingClientActualHTTPControlsDoNotStartWork(t *testing.T) {
	f := newMutationFixture(t)
	for _, ensure := range []func() error{
		func() error { return reviewexec.EnsureSchema(t.Context(), f.store) },
		func() error { return reviews.EnsureSchema(t.Context(), f.store) },
	} {
		if err := ensure(); err != nil {
			t.Fatal(err)
		}
	}
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "scheduling-client", Provider: "synthetic", Account: "fixture", Channels: []string{"cli", "hosted"}}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	e.Register(mux)
	f.handler = mux
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); f.handler.ServeHTTP(w, r) }))
	defer service.Close()
	grant := func(token string, permissions []string) string {
		sum := sha256.Sum256([]byte(token))
		return f.call(t, "POST", "/api/v1/projects/"+f.project+"/credentials", f.admin, f.sender, map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": permissions})["id"].(string)
	}
	const ownerToken = "synthetic-scheduling-owner"
	ownerID := grant(ownerToken, []string{"manage"})
	grant("synthetic-scheduling-writer", []string{"read", "write"})
	grant("synthetic-scheduling-reader", []string{"read"})
	owner := f.newClient(t, ownerToken, f.sender, service.URL)
	writer := f.newClient(t, "synthetic-scheduling-writer", f.sender, service.URL)
	reader := f.newClient(t, "synthetic-scheduling-reader", f.sender, service.URL)
	providerCalls := f.providerCalls.Load()
	initial, err := reader.ReviewScheduling(t.Context())
	if err != nil || initial.Revision != 0 || initial.AutomaticHosted || initial.PausedPulls == nil || len(initial.PausedPulls) != 0 || initial.Reason != "" {
		t.Fatal(initial, err)
	}
	pull, err := reader.ReviewPullScheduling(t.Context(), 2)
	if err != nil || pull.Revision != 0 || pull.Paused || pull.ProjectID != f.project || pull.PullNumber != 2 {
		t.Fatal(pull, err)
	}
	paused := client.ConfigureReviewPullSchedulingRequest{Paused: true, Reason: "Fix in progress"}
	first, err := writer.ConfigureReviewPullScheduling(t.Context(), "pause-before-intent", 2, paused)
	if err != nil || first.ID != "2" || first.Revision != 1 {
		t.Fatal(first, err)
	}
	replay, err := writer.ConfigureReviewPullScheduling(t.Context(), "pause-before-intent", 2, paused)
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatal("pause replay changed", first, replay, err)
	}
	_, err = writer.ConfigureReviewPullScheduling(t.Context(), "stale-pause", 2, paused)
	checkError(t, err, "review_conflict")
	_, err = reader.ConfigureReviewPullScheduling(t.Context(), "reader-pause", 2, paused)
	checkError(t, err, "forbidden")
	_, err = writer.ConfigureReviewPullScheduling(t.Context(), "pause-outside-queue", 99, paused)
	if err != nil {
		t.Fatal(err)
	}
	configuration := client.ConfigureReviewExecutionRequest{CapabilityID: "scheduling-client", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}
	f.call(t, "POST", "/api/v1/projects/"+f.project+"/reviews/execution/configuration", f.admin, f.sender, configuration)
	before := requests.Load()
	filtered, err := reader.ReviewScheduling(t.Context())
	if err != nil || requests.Load() != before+1 || len(filtered.PausedPulls) != 1 || filtered.PausedPulls[0].PullNumber != 2 || !filtered.PausedPulls[0].Paused || filtered.PausedPulls[0].Reason != paused.Reason {
		t.Fatal("project GET lost queue-filtered pauses or added per-PR requests", filtered, err)
	}
	enable := client.ConfigureReviewSchedulingRequest{AutomaticHosted: true}
	_, err = writer.ConfigureReviewScheduling(t.Context(), "writer-enable", enable)
	checkError(t, err, "forbidden")
	enabled, err := owner.ConfigureReviewScheduling(t.Context(), "owner-enable", enable)
	if err != nil || enabled.ID != f.project || enabled.Revision != 1 {
		t.Fatal(enabled, err)
	}
	again, err := owner.ConfigureReviewScheduling(t.Context(), "owner-enable", enable)
	if err != nil || !reflect.DeepEqual(enabled, again) {
		t.Fatal("enable replay changed", again, err)
	}
	_, err = owner.ConfigureReviewScheduling(t.Context(), "stale-enable", enable)
	checkError(t, err, "review_conflict")
	current, err := reader.ReviewScheduling(t.Context())
	if err != nil || !current.AutomaticHosted || current.Revision != 1 || current.AuthorizingWorkerID != f.sender || current.Reason != "" {
		t.Fatal(current, err)
	}
	_, err = writer.ConfigureReviewPullScheduling(t.Context(), "writer-resume", 2, client.ConfigureReviewPullSchedulingRequest{ExpectedRevision: 1, Paused: false, Reason: "discard on resume"})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := reader.ReviewPullScheduling(t.Context(), 2)
	if err != nil || resumed.Paused || resumed.Revision != 2 || resumed.Reason != "" {
		t.Fatal(resumed, err)
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id=?`, ownerID); err != nil {
		t.Fatal(err)
	}
	withdrawn, err := reader.ReviewScheduling(t.Context())
	if err != nil || !withdrawn.AutomaticHosted || withdrawn.Reason != "authorizer_unavailable" || withdrawn.AuthorizingWorkerID != f.sender || len(withdrawn.PausedPulls) != 0 {
		t.Fatal("withdrawn original authorization was not reported truthfully", withdrawn, err)
	}
	_, err = owner.ConfigureReviewScheduling(t.Context(), "owner-enable", enable)
	checkError(t, err, "unauthorized")
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_reservations", "review_execution_evidence", "review_round_completions", "review_selections", "review_automatic_selections"} {
		var count int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("settings handler started or credited review work", table, count, err)
		}
	}
	if f.providerCalls.Load() != providerCalls {
		t.Fatal("settings/read/pause contacted a provider")
	}
}

func TestReviewSchedulingClientRejectsMalformedWireAndKeepsUnknownMutationsSingleShot(t *testing.T) {
	var response atomic.Value
	response.Store(`{"project_id":"project-one","revision":0,"automatic_hosted":false,"paused_pulls":[]}`)
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			body, _ := io.ReadAll(r.Body)
			want := `{"expected_revision":0,"automatic_hosted":true}`
			if strings.Contains(r.URL.Path, "/pulls/2/") {
				want = `{"expected_revision":0,"paused":true}`
			}
			if r.Header.Get("Idempotency-Key") != "original-scheduling-key" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || string(body) != want {
				t.Error("mutation lost original key/body/binding")
			}
		}
		_, _ = io.WriteString(w, response.Load().(string))
	}))
	defer service.Close()
	c := newClient(t, service)
	defaultProject := map[string]any{"project_id": "project-one", "revision": 0, "automatic_hosted": false, "paused_pulls": []any{}}
	validPaused := map[string]any{"project_id": "project-one", "pull_number": 2, "revision": 1, "paused": true, "reason": "Fixing", "actor_id": "actor-one", "updated_at": "2026-11-01T00:00:00Z"}
	// Exhausted stored revisions remain readable; only another increment is
	// invalid. Keep the largest legitimate native int64 exact on the wire.
	maxProject := map[string]any{}
	for key, value := range defaultProject {
		maxProject[key] = value
	}
	maxProject["revision"], maxProject["updated_at"] = int64(math.MaxInt64), "2026-11-01T00:00:00Z"
	encoded, _ := json.Marshal(maxProject)
	response.Store(string(encoded))
	projectAtLimit, err := c.ReviewScheduling(t.Context())
	if err != nil || projectAtLimit.Revision != math.MaxInt64 {
		t.Fatal("exhausted project revision became unreadable", projectAtLimit, err)
	}
	maxPull := map[string]any{}
	for key, value := range validPaused {
		maxPull[key] = value
	}
	maxPull["revision"] = int64(math.MaxInt64)
	encoded, _ = json.Marshal(maxPull)
	response.Store(string(encoded))
	pullAtLimit, err := c.ReviewPullScheduling(t.Context(), 2)
	if err != nil || pullAtLimit.Revision != math.MaxInt64 {
		t.Fatal("exhausted PR revision became unreadable", pullAtLimit, err)
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"project-binding", func(v map[string]any) { v["project_id"] = "another-project" }},
		{"missing-paused-array", func(v map[string]any) { delete(v, "paused_pulls") }},
		{"null-paused-array", func(v map[string]any) { v["paused_pulls"] = nil }},
		{"duplicate-paused", func(v map[string]any) { v["paused_pulls"] = []any{validPaused, validPaused} }},
		{"default-enabled", func(v map[string]any) { v["automatic_hosted"] = true }},
		{"default-dated", func(v map[string]any) { v["updated_at"] = "2026-11-01T00:00:00Z" }},
		{"negative-revision", func(v map[string]any) { v["revision"] = -1 }},
		{"fractional-revision", func(v map[string]any) { v["revision"] = 1.5 }},
		{"bad-date", func(v map[string]any) { v["revision"] = 1; v["updated_at"] = "PRIVATE_INVALID_DATE" }},
		{"zero-date", func(v map[string]any) { v["revision"] = 1; v["updated_at"] = "0001-01-01T00:00:00Z" }},
		{"oversized-date", func(v map[string]any) {
			v["revision"] = 1
			v["updated_at"] = "2026-11-01T00:00:00." + strings.Repeat("1", 50) + "Z"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := map[string]any{}
			for k, x := range defaultProject {
				v[k] = x
			}
			tc.change(v)
			b, _ := json.Marshal(v)
			response.Store(string(b))
			_, err := c.ReviewScheduling(t.Context())
			checkError(t, err, "invalid_response", "PRIVATE_INVALID_DATE")
		})
	}
	for _, tc := range []struct{ name, timestamp string }{
		{"offset", "2026-11-01T05:30:00+05:30"},
		{"redundant-precision", "2026-11-01T00:00:00.000000000Z"},
	} {
		t.Run("project-valid-date-"+tc.name, func(t *testing.T) {
			v := map[string]any{}
			for k, x := range defaultProject {
				v[k] = x
			}
			v["revision"], v["updated_at"] = 1, tc.timestamp
			b, _ := json.Marshal(v)
			response.Store(string(b))
			if _, err := c.ReviewScheduling(t.Context()); err != nil {
				t.Fatalf("valid RFC3339Nano timestamp %q was rejected: %v", tc.timestamp, err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"project-binding", func(v map[string]any) { v["project_id"] = "another-project" }},
		{"pull-binding", func(v map[string]any) { v["pull_number"] = 4 }},
		{"default-paused", func(v map[string]any) { v["revision"] = 0 }},
		{"missing-actor", func(v map[string]any) { delete(v, "actor_id") }},
		{"unpaused-reason", func(v map[string]any) { v["paused"] = false }},
		{"bad-date", func(v map[string]any) { v["updated_at"] = "PRIVATE_INVALID_DATE" }},
		{"zero-date", func(v map[string]any) { v["updated_at"] = "0001-01-01T00:00:00Z" }},
	} {
		t.Run("pull-"+tc.name, func(t *testing.T) {
			v := map[string]any{}
			for k, x := range validPaused {
				v[k] = x
			}
			tc.change(v)
			b, _ := json.Marshal(v)
			response.Store(string(b))
			_, err := c.ReviewPullScheduling(t.Context(), 2)
			checkError(t, err, "invalid_response")
		})
	}
	for _, tc := range []struct{ name, timestamp string }{
		{"offset", "2026-11-01T05:30:00+05:30"},
		{"redundant-precision", "2026-11-01T00:00:00.000000000Z"},
	} {
		t.Run("pull-valid-date-"+tc.name, func(t *testing.T) {
			v := map[string]any{}
			for k, x := range validPaused {
				v[k] = x
			}
			v["updated_at"] = tc.timestamp
			b, _ := json.Marshal(v)
			response.Store(string(b))
			if _, err := c.ReviewPullScheduling(t.Context(), 2); err != nil {
				t.Fatalf("valid RFC3339Nano timestamp %q was rejected: %v", tc.timestamp, err)
			}
		})
	}
	for _, pull := range []bool{false, true} {
		body := `{"id":"wrong-target","state":"scheduling_configured","revision":1}`
		if pull {
			body = `{"id":"4","state":"pull_scheduling_updated","revision":1}`
		}
		response.Store(body)
		before := calls.Load()
		var err error
		if pull {
			_, err = c.ConfigureReviewPullScheduling(t.Context(), "original-scheduling-key", 2, client.ConfigureReviewPullSchedulingRequest{Paused: true})
		} else {
			_, err = c.ConfigureReviewScheduling(t.Context(), "original-scheduling-key", client.ConfigureReviewSchedulingRequest{AutomaticHosted: true})
		}
		checkError(t, err, "invalid_response")
		var unknown *client.Error
		if !errors.As(err, &unknown) || !unknown.OutcomeUnknown || calls.Load() != before+1 {
			t.Fatal("invalid mutation receipt lost uncertainty or retried", err)
		}
	}
}

func TestReviewSchedulingClientLocalValidationNeedsNoConnection(t *testing.T) {
	c := &client.Client{}
	for _, invoke := range []func() error{
		func() error { _, err := c.ReviewPullScheduling(t.Context(), 0); return err },
		func() error {
			_, err := c.ConfigureReviewScheduling(t.Context(), "", client.ConfigureReviewSchedulingRequest{})
			return err
		},
		func() error {
			_, err := c.ConfigureReviewScheduling(t.Context(), "key", client.ConfigureReviewSchedulingRequest{ExpectedRevision: -1})
			return err
		},
		func() error {
			_, err := c.ConfigureReviewScheduling(t.Context(), "key", client.ConfigureReviewSchedulingRequest{ExpectedRevision: math.MaxInt64})
			return err
		},
		func() error {
			_, err := c.ConfigureReviewPullScheduling(t.Context(), "key", 2147483648, client.ConfigureReviewPullSchedulingRequest{})
			return err
		},
		func() error {
			_, err := c.ConfigureReviewPullScheduling(t.Context(), "key", 2, client.ConfigureReviewPullSchedulingRequest{ExpectedRevision: -1})
			return err
		},
		func() error {
			_, err := c.ConfigureReviewPullScheduling(t.Context(), "key", 2, client.ConfigureReviewPullSchedulingRequest{ExpectedRevision: math.MaxInt64})
			return err
		},
		func() error {
			_, err := c.ConfigureReviewPullScheduling(t.Context(), "key", 2, client.ConfigureReviewPullSchedulingRequest{Reason: "line\nbreak"})
			return err
		},
		func() error {
			_, err := c.ConfigureReviewPullScheduling(t.Context(), "key", 2, client.ConfigureReviewPullSchedulingRequest{Reason: strings.Repeat("a", 257)})
			return err
		},
		func() error {
			_, err := c.ConfigureReviewPullScheduling(t.Context(), "key", 2, client.ConfigureReviewPullSchedulingRequest{Reason: string([]byte{255})})
			return err
		},
	} {
		if err := invoke(); err == nil {
			t.Fatal("invalid local input reached the unconfigured connection")
		}
	}
}

func TestSchedulingOptionalCLIWireContract(t *testing.T) {
	for _, state := range []string{"omitted", "true", "false"} {
		request := client.ConfigureReviewSchedulingRequest{AutomaticHosted: true}
		want := `{"expected_revision":0,"automatic_hosted":true`
		if state != "omitted" {
			enabled := state == "true"
			request.AutomaticCLI = &enabled
			want += `,"automatic_cli":` + state
		}
		encoded, err := json.Marshal(request)
		if err != nil || string(encoded) != want+"}" {
			t.Fatalf("wrong optional CLI body: %s %v", encoded, err)
		}
	}
	var response string
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	defer service.Close()
	c := newClient(t, service)
	for _, tc := range []struct {
		body           string
		valid, enabled bool
	}{
		{`{"project_id":"project-one","revision":0,"automatic_hosted":false,"paused_pulls":[]}`, true, false},
		{`{"project_id":"project-one","revision":0,"automatic_hosted":false,"automatic_cli":true,"paused_pulls":[]}`, false, false},
		{`{"project_id":"project-one","revision":0,"automatic_hosted":false,"automatic_cli":null,"paused_pulls":[]}`, false, false},
		{`{"project_id":"project-one","revision":1,"automatic_hosted":false,"automatic_cli":true,"paused_pulls":[],"updated_at":"2026-11-01T00:00:00Z"}`, false, false},
		{`{"project_id":"project-one","revision":1,"automatic_hosted":false,"automatic_cli":true,"paused_pulls":[],"updated_at":"2026-11-01T00:00:00Z","authorizing_worker_id":"worker-one"}`, true, true},
	} {
		response = tc.body
		value, err := c.ReviewScheduling(t.Context())
		if (err == nil) != tc.valid || tc.valid && value.AutomaticCLI != tc.enabled {
			t.Fatalf("wrong scheduling validation: %+v %v", value, err)
		}
	}
}

func TestReviewSchedulingClientReadsMaximumEscapedPausedQueue(t *testing.T) {
	project := strings.Repeat("<", 128)
	value := client.ReviewScheduling{
		ProjectID: project, Revision: math.MaxInt64, AutomaticHosted: true, AutomaticCLI: true,
		AuthorizingWorkerID: strings.Repeat("&", 128), UpdatedAt: "2026-11-01T00:00:00.999999999+05:30",
		PausedPulls: make([]client.ReviewPullScheduling, 100),
	}
	for i := range value.PausedPulls {
		value.PausedPulls[i] = client.ReviewPullScheduling{
			ProjectID: project, PullNumber: 2147483647 - int64(i), Revision: math.MaxInt64, Paused: true,
			Reason: strings.Repeat("<", 256), ActorID: strings.Repeat(">", 128), UpdatedAt: value.UpdatedAt,
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) <= 256<<10 {
		t.Fatalf("fixture must exercise escaped reasons and maximum identities: bytes=%d err=%v", len(encoded), err)
	}
	var response atomic.Value
	response.Store(encoded)
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/"+project+"/reviews/scheduling" {
			t.Error("scheduling read changed scope or method")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(response.Load().([]byte))
	}))
	defer service.Close()
	selected := binding(t, service.URL)
	selected.Context.ProjectID = project
	c, err := client.New(t.Context(), selected)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.ReviewScheduling(t.Context())
	if err != nil || !reflect.DeepEqual(got, value) || calls.Load() != 1 {
		t.Fatalf("valid maximum escaped scheduling response failed: calls=%d err=%v", calls.Load(), err)
	}
	// Retain a finite wire bound even for an otherwise valid response.
	response.Store(append(append([]byte(nil), encoded...), []byte(strings.Repeat(" ", 512<<10))...))
	_, err = c.ReviewScheduling(t.Context())
	checkError(t, err, "invalid_response")
	if calls.Load() != 2 {
		t.Fatal("oversized scheduling read retried", calls.Load())
	}
}
