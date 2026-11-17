package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestReviewQueueFinalTargetCIHealth(t *testing.T) {
	for _, state := range []string{"unknown", "pending", "success", "failure", "not_required"} {
		t.Run(state, func(t *testing.T) {
			c := finalGateQueueClient(t, `"ci_gate_ref":"main","ci_gate_state":"`+state+`"`)
			queue, err := c.ReviewQueue(t.Context(), client.PageOptions{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			if len(queue.Items) != 1 || queue.Items[0].CIState != "not_required" {
				t.Fatal("final-target health changed retained PR CI state")
			}
			data, err := json.Marshal(queue.Items[0].Observation)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err = json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got["ci_gate_ref"] != "main" || got["ci_gate_state"] != state {
				t.Fatalf("lost final-target health: %s", data)
			}
		})
	}
	t.Run("legacy_absence", func(t *testing.T) {
		if _, err := finalGateQueueClient(t, "").ReviewQueue(t.Context(), client.PageOptions{}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReviewQueueRefusesInvalidFinalTargetCIHealth(t *testing.T) {
	cases := map[string]string{
		"missing_state":   `"ci_gate_ref":"main"`,
		"missing_ref":     `"ci_gate_state":"success"`,
		"empty_state":     `"ci_gate_ref":"main","ci_gate_state":""`,
		"unknown_state":   `"ci_gate_ref":"main","ci_gate_state":"ready"`,
		"empty_ref":       `"ci_gate_ref":"","ci_gate_state":"success"`,
		"blank_ref":       `"ci_gate_ref":" ","ci_gate_state":"success"`,
		"long_ref":        `"ci_gate_ref":"` + strings.Repeat("a", 257) + `","ci_gate_state":"success"`,
		"null_state":      `"ci_gate_ref":"main","ci_gate_state":null`,
		"null_ref":        `"ci_gate_ref":null,"ci_gate_state":"success"`,
		"both_null":       `"ci_gate_ref":null,"ci_gate_state":null`,
		"wrong_type":      `"ci_gate_ref":123,"ci_gate_state":"success"`,
		"unknown_field":   `"ci_gate_ref":"main","ci_gate_state":"success","merge_ready":true`,
		"duplicate_state": `"ci_gate_ref":"main","ci_gate_state":"success","ci_gate_state":"failure"`,
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := finalGateQueueClient(t, fields).ReviewQueue(t.Context(), client.PageOptions{})
			var failure *client.Error
			if !errors.As(err, &failure) || failure.Code != "invalid_response" {
				t.Fatalf("malformed health accepted: %v", err)
			}
		})
	}
}

func finalGateQueueClient(t *testing.T, fields string) *client.Client {
	t.Helper()
	if fields != "" {
		fields = "," + fields
	}
	body := `{"project_id":"project-one","configured":true,"revision":1,"snapshot_id":"review_snapshot_` + strings.Repeat("a", 32) + `","source":"synthetic","observed_at":"2026-10-15T12:00:00Z","recorded_at":"2026-10-15T12:00:00Z","items":[{"observation":{"state":"current","last_success_at":"2026-10-15T12:00:00Z","ci_checked_at":"2026-10-15T12:00:00Z"` + fields + `},"number":1,"title":"Retained merged PR","state":"merged","head_sha":"","base_ref":"feature/parent","base_sha":"","ci_state":"not_required","mergeability":"unknown","complete":true,"run_count":0,"run_counts":{"hosted":0,"cli":0,"manual":0,"subagent":0},"attempt_count":0,"recent_runs":[],"latest_attempts":[]}],"has_more":false}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/instance":
			w.Write([]byte(`{"instance_id":"instance-one"}`))
		case "/api/v1/projects/project-one/reviews/queue":
			if r.Method != http.MethodGet || r.URL.Query().Get("include_observation") != "true" {
				t.Error("unexpected queue request")
			}
			w.Write([]byte(body))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte("synthetic-read-token"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "synthetic", ServerURL: server.URL, InstanceID: "instance-one", ProjectID: "project-one", WorkerID: "worker-one"}, CredentialFile: credential})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
