package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func reviewExecutionFixture(t *testing.T) (*serviceFixture, string, string) {
	t.Helper()
	f := setupServiceFixture(t)
	project := f.project(t, "execution-fixture")
	worker := f.worker(t, project, "Builder")
	if err := reviewexec.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "fixture", Provider: "synthetic", Account: "fixture", Channels: []string{"cli", "hosted"}}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if _, err := engine.ConfigurePolicy(t.Context(), scope, "policy", reviewexec.PolicyConfiguration{ExpectedRevision: &zero, RequiredChannels: []string{"cli"}, PausedChannels: []string{}, MaxReviewFiles: 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Configure(t.Context(), scope, "config", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: "fixture", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	// Reads and enqueue are public handlers, without a provider driver/supervisor.
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	engine.Register(mux)
	f.handler = mux
	return f, project, worker
}

func reviewRequestArgs() []string {
	return []string{"reviews", "request", "2", "--key", "explicit-review-key", "--expect-config", "1", "--channel", "cli", "--head", strings.Repeat("a", 40)}
}

func TestReviewExecutionCLIReadsAndQueuedReceiptRemainBound(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	other := f.worker(t, project, "Other")
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+f.admin || r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
			t.Error("lost credential/instance binding")
		}
		if strings.Contains(r.URL.Path, "/reviews/") && (r.Header.Get("X-FC-Worker") == "" || r.URL.RawQuery != "") {
			t.Error("lost worker binding or introduced selectors")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}, "other": {project, other}})
	providers := f.resolverHit.Load()
	code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "execution")
	var config client.ReviewExecutionConfiguration
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &config) != nil || config.ProjectID != project || config.Revision != 1 {
		t.Fatal("stored config read failed", code, stderr)
	}
	before := requests.Load()
	receipt := mutationSuccess(t, path, "builder", reviewRequestArgs()...)
	if receipt["state"] != "queued" || receipt["revision"] != float64(1) || requests.Load()-before != 2 {
		t.Fatal("enqueue waited, polled or expanded receipt")
	}
	replay := mutationSuccess(t, path, "builder", reviewRequestArgs()...)
	if encodeString(replay) != encodeString(receipt) {
		t.Fatal("exact request replay changed receipt")
	}
	id := receipt["id"].(string)
	code, out, stderr = mutationInvoke(t, path, "other", "reviews", "operation", id)
	var operation client.ReviewOperation
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &operation) != nil || operation.ID != id || operation.WorkerID != worker || operation.State != "queued" || operation.AttemptID != "" {
		t.Fatal("explicit operation read changed attribution or fabricated dispatch", code, stderr)
	}
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations`).Scan(&count); err != nil || count != 1 {
		t.Fatal("replay created another operation", err)
	}
	if f.resolverHit.Load() != providers {
		t.Fatal("local execution controls performed provider work")
	}
}

func TestReviewExecutionCLIInvalidArgumentsPrecedeCredentialAndHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	path := fixture(t, encode(t, map[string]any{"version": 1, "profiles": map[string]any{"chosen": map[string]any{"server_url": service.URL, "instance_id": "instance-fixture", "project_id": "project-fixture", "worker_id": "worker-fixture", "credential_file": "missing-private-token"}}}))
	base := reviewRequestArgs()
	replace := func(index int, value string) []string {
		out := append([]string(nil), base...)
		out[index] = value
		return out
	}
	invalid := [][]string{
		{"reviews", "execution", "extra"}, {"reviews", "execution", "--project", "other"}, {"reviews", "operation"}, {"reviews", "operation", "../escape"}, {"reviews", "operation", "review_operation_" + strings.Repeat("A", 32)},
		{"reviews", "operation", "review_operation_" + strings.Repeat("a", 32), "--worker", "other"},
		base[:3], replace(2, "0"), replace(2, "2147483648"), replace(2, "+2"), replace(2, "02"), replace(2, "2.0"), replace(4, ""), replace(4, " padded"), replace(4, "bad\nkey"), replace(6, "0"), replace(6, "-1"), replace(6, "01"), replace(6, "1.5"), replace(6, "9223372036854775808"), replace(8, "CLI"), replace(8, "manual"), replace(10, strings.Repeat("A", 40)), replace(10, strings.Repeat("a", 39)), replace(10, string([]byte{255})),
		append(append([]string(nil), base...), "--head", strings.Repeat("a", 40)), append(append([]string(nil), base...), "--project", "other"), append(append([]string(nil), base...), "extra"),
	}
	for _, args := range invalid {
		code, out, stderr := mutationInvoke(t, path, "chosen", args...)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatalf("invalid args reached credential: %v: %d %s", args, code, stderr)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid args reached HTTP")
	}
}

func TestReviewExecutionCLIUnknownReceiptSameKeyOneEffect(t *testing.T) {
	for _, mode := range []string{"dropped", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			f, project, worker := reviewExecutionFixture(t)
			var posts atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews/requests") && posts.Add(1) == 1 {
					rec := httptest.NewRecorder()
					f.handler.ServeHTTP(rec, r)
					if rec.Code != 202 {
						t.Error("fixture request did not commit")
						return
					}
					if mode == "dropped" {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(202)
					w.Write([]byte(`{"id":"invalid"}`))
					return
				}
				f.handler.ServeHTTP(w, r)
			}))
			defer service.Close()
			path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
			code, out, stderr := mutationInvoke(t, path, "builder", reviewRequestArgs()...)
			if code != cli.ExitService || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || !strings.Contains(stderr, "same idempotency key") || posts.Load() != 1 {
				t.Fatal("lost receipt retry/privacy failure", code, stderr, posts.Load())
			}
			mutationSuccess(t, path, "builder", reviewRequestArgs()...)
			var count int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations`).Scan(&count); err != nil || count != 1 || posts.Load() != 2 {
				t.Fatal("same-key recovery duplicated request", err)
			}
		})
	}
}

func TestReviewExecutionCLIAggregateBudgetAndCancellation(t *testing.T) {
	for _, mode := range []string{"deadline", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f, project, worker := reviewExecutionFixture(t)
			var posts atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					select {
					case <-time.After(func() time.Duration {
						if mode == "deadline" {
							return 1200 * time.Millisecond
						}
						return 50 * time.Millisecond
					}()):
					case <-r.Context().Done():
						return
					}
					f.handler.ServeHTTP(w, r)
					return
				}
				posts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-time.After(6 * time.Second):
				}
			}))
			defer service.Close()
			path := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				time.AfterFunc(300*time.Millisecond, cancel)
			}
			var out, stderr bytes.Buffer
			start := time.Now()
			code := cli.RunContext(ctx, append([]string{"--profiles-file", path, "--profile", "builder"}, reviewRequestArgs()...), nil, &out, &stderr)
			elapsed := time.Since(start)
			want := cli.ExitService
			if mode == "cancelled" {
				want = cli.ExitCancelled
			}
			if code != want || out.Len() != 0 || posts.Load() != 1 || !strings.Contains(stderr.String(), `"outcome_unknown":true`) {
				t.Fatal("request bound/cancellation failed", code, elapsed, stderr.String())
			}
			if mode == "deadline" && (elapsed < 4500*time.Millisecond || elapsed > 5500*time.Millisecond) {
				t.Fatal("aggregate deadline was reset after handshake", elapsed)
			}
			if mode == "cancelled" && elapsed > time.Second {
				t.Fatal("cancellation did not release request", elapsed)
			}
		})
	}
}
