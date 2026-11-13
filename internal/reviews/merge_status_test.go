package reviews

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestMergeStatusReflectsGitHubIndependentlyOfRequiredCI(t *testing.T) {
	for _, ci := range []string{"success", "not_required", "pending", "failure", "unknown"} {
		for _, conflict := range []string{"mergeable", "conflicting", "unknown"} {
			for _, native := range []struct{ github, state, reason string }{
				{"CLEAN", "ready", "github_clean"}, {"BLOCKED", "blocked", "github_blocked"},
				{"BEHIND", "blocked", "github_behind"}, {"DIRTY", "blocked", "github_dirty"},
				{"DRAFT", "blocked", "github_draft"}, {"UNSTABLE", "pending", "github_unstable"},
				{"HAS_HOOKS", "pending", "github_has_hooks"}, {"UNKNOWN", "unknown", "merge_state_unknown"},
				{"", "unknown", "merge_state_unknown"},
			} {
				got := mergeStatus(PullSummary{State: "open", CIState: ci, Mergeability: conflict, mergeStateStatus: native.github})
				if got == nil || got.State != native.state || got.Reason != native.reason {
					t.Fatalf("CI=%s conflicts=%s GitHub=%s got=%+v want=%s/%s", ci, conflict, native.github, got, native.state, native.reason)
				}
			}
		}
	}
	p := PullSummary{State: "open", CIState: "unknown", mergeStateStatus: "CLEAN"}
	draft := true
	p.IsDraft = &draft
	if got := mergeStatus(p); got.State != "blocked" || got.Reason != "draft" {
		t.Fatal(got)
	}
	p.IsDraft = nil
	p.State = "unknown"
	if got := mergeStatus(p); got.State != "unknown" || got.Reason != "pull_state_unknown" {
		t.Fatal("unknown lifecycle gained eligibility", got)
	}
	for _, state := range []string{"merged", "closed"} {
		p.State = state
		if mergeStatus(p) != nil {
			t.Fatal("terminal PR retained current merge claim")
		}
	}
}

func TestMergeStatusProjectionIsExplicitPinnedAndReadOnly(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	p.State = "open"
	p.CIState = "unknown"
	p.Mergeability = "mergeable"
	p.MergeStateStatus = "CLEAN"
	old := f.observe(t, reviewRequest(0, []PullInput{p}), "merge-start")["id"].(string)
	for _, route := range []string{"/queue", "/queue?view=recent", "/pulls/1"} {
		body := nativeRead(t, f, route, &json.RawMessage{})
		if bytes.Contains(body, []byte("merge_status")) || bytes.Contains(body, []byte("merge_state_status")) {
			t.Fatal("default response changed agent contract", route)
		}
		separator := "?"
		if strings.Contains(route, "?") {
			separator = "&"
		}
		var value map[string]any
		nativeRead(t, f, route+separator+"include_merge_status=true", &value)
		var pull map[string]any
		if route == "/pulls/1" {
			pull = value["pull"].(map[string]any)
		} else {
			pull = value["items"].([]any)[0].(map[string]any)
		}
		if pull["merge_status"].(map[string]any)["state"] != "ready" {
			t.Fatal(value)
		}
	}
	pins := sourcePins(t, f, 1)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := PublishSourceStatus(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "service-test"}, 701, pins, []SourceStatus{{Pull: sourcePull(1, p.Title), CIState: "failure", Mergeability: "mergeable", MergeStateStatus: "BLOCKED"}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var current PullResponse
	nativeRead(t, f, "/pulls/1?include_merge_status=true", &current)
	if current.Pull.MergeStatus.State != "blocked" || current.Pull.MergeStatus.Reason != "github_blocked" {
		t.Fatal(current.Pull.MergeStatus)
	}
	var pinned PullResponse
	nativeRead(t, f, "/pulls/1?snapshot="+old+"&include_merge_status=true", &pinned)
	if pinned.Pull.MergeStatus.State != "ready" {
		t.Fatal("historical facts changed", pinned.Pull.MergeStatus)
	}
	for _, route := range []string{"/queue?include_merge_status=false", "/queue?include_merge_status=true&include_merge_status=true", "/pulls/1?include_merge_status=1"} {
		w := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews"+route, f.readToken, "", nil)
		if w.Code != http.StatusBadRequest {
			t.Fatal("invalid projection selector accepted", w.Code)
		}
	}
	// A base move cannot carry a formerly clear merge gate to a different comparison.
	moved := sourcePull(1, p.Title)
	moved.BaseSHA = strings.Repeat("c", 40)
	if _, err := publishStatus(t, f, sourcePins(t, f, 1), moved, "success", "mergeable"); err != nil {
		t.Fatal(err)
	}
	nativeRead(t, f, "/pulls/1?include_merge_status=true", &current)
	if current.Pull.MergeStatus.State != "unknown" {
		t.Fatal("old branch restrictions crossed comparison", current.Pull.MergeStatus)
	}
}
