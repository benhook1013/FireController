package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewAllowanceCLIRoundTrip(t *testing.T) {
	for _, channel := range []string{"cli", "hosted", "legacy"} {
		t.Run(channel, func(t *testing.T) {
			f := setupServiceFixture(t)
			project := f.project(t, "retained-allowance")
			worker := f.worker(t, project, "Builder")
			op := client.ReviewOperation{ID: "review_operation_" + strings.Repeat("a", 32), ProjectID: project, ActorID: "principal_one", WorkerID: worker, ConfigurationRevision: 1, PolicyRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40), State: "held", Revision: 3, Reason: "allowance_exhausted", AttemptID: "review_attempt_" + strings.Repeat("b", 32), CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
			zero, limit, window := int64(0), int64(3), int64(86400000)
			if channel == "cli" {
				op.Allowance = &client.ReviewAllowanceObservation{Kind: "cli_exhausted", ObservedAt: op.UpdatedAt, Remaining: &zero, Limit: &limit, FullCapacityInMS: &zero, RollingWindowMS: &window}
			}
			if channel == "hosted" {
				op.Channel = channel
				op.Reason = "hosted_allowance_cooldown"
				op.Allowance = &client.ReviewAllowanceObservation{Kind: "hosted_cooldown", ObservedAt: op.UpdatedAt, RetryNotBefore: "2026-01-01T00:10:00Z"}
			}
			selection := client.ReviewRunState{Selection: client.ReviewRunSelection{ID: cliSelectionID, ProjectID: project, ActorID: op.ActorID, WorkerID: worker, Channel: op.Channel, ConfigurationRevision: 1, PolicyRevision: 1, ExpectedPullNumber: 2, State: "selected", Revision: 3, OperationID: op.ID, CreatedAt: op.CreatedAt, UpdatedAt: op.UpdatedAt}, Operation: &op}
			var calls atomic.Int64
			var invalid atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/instance" {
					_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": f.store.InstanceID()})
					return
				}
				calls.Add(1)
				if r.Header.Get("X-FC-Worker") != worker || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+f.admin {
					t.Error("allowance lost profile binding")
				}
				if invalid.Load() {
					raw, _ := json.Marshal(op)
					_, _ = w.Write([]byte(strings.Replace(string(raw), `"state":"held"`, `"state":"held","private":"PRIVATE_SENTINEL"`, 1)))
					return
				}
				if strings.Contains(r.URL.Path, "/selections") {
					_ = json.NewEncoder(w).Encode(selection)
				} else {
					_ = json.NewEncoder(w).Encode(op)
				}
			}))
			defer srv.Close()
			profiles := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
			expected, _ := json.Marshal(op)
			var want map[string]any
			_ = json.Unmarshal(expected, &want)
			for _, args := range [][]string{{"reviews", "operation", op.ID}, {"reviews", "run-read", "--selection-id", cliSelectionID}, {"reviews", "request", "2", "--key", "retained-request", "--expect-config", "1", "--channel", op.Channel, "--head", op.HeadSHA}, {"reviews", "start", "--channel", op.Channel, "--key", "retained-selection", "--expect-pr", "2"}} {
				code, out, stderr := mutationInvoke(t, profiles, "builder", args...)
				var got map[string]any
				if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &got) != nil {
					t.Fatal("CLI retained result failed", code, stderr)
				}
				if nested, ok := got["operation"].(map[string]any); ok {
					got = nested
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatal("CLI allowance changed or zero omitted", out)
				}
			}
			if calls.Load() != 4 {
				t.Fatal("CLI polled retained state", calls.Load())
			}
			invalid.Store(true)
			code, out, stderr := mutationInvoke(t, profiles, "builder", "reviews", "operation", op.ID)
			if code == 0 || out != "" || !strings.Contains(stderr, "invalid_response") || strings.Contains(stderr, "PRIVATE_SENTINEL") || calls.Load() != 5 {
				t.Fatal("CLI accepted/leaked invalid retained facts", code, stderr)
			}
		})
	}
}
