package client_test

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

func TestCIMismatchFactsStrictReadsAndSharedAgentDiagnostic(t *testing.T) {
	v := sampleCIAssessment()
	v.State, v.Reason, v.Selection, v.SelectedCommitSHA, v.MergeState = "unproven", "ci_merge_base_mismatch", "", "", "unproven"
	v.Required, v.Satisfied = 0, 0
	v.MergeBaseMismatch = &client.CIMergeBaseMismatch{PRBaseSHA: v.Target.BaseSHA, LiveBaseSHA: strings.Repeat("d", 40), MergeSHA: strings.Repeat("e", 40), MergeBaseParentSHA: v.Target.BaseSHA}
	pin := client.CIWatchTarget{PullNumber: 2, HeadSHA: v.Target.HeadSHA, BaseSHA: v.Target.BaseSHA}
	watch := client.CIWatchResult{ProjectID: v.ProjectID, State: "pending", TimedOut: true, Targets: []client.CIWatchTargetResult{{PullNumber: 2, HeadSHA: pin.HeadSHA, BaseSHA: pin.BaseSHA, ObservedHeadSHA: pin.HeadSHA, ObservedBaseSHA: pin.BaseSHA, ObservedAt: v.ObservedAt, State: "pending", Reason: v.Reason, BaseRef: v.Target.BaseRef, GateRef: "main", Checks: []client.ReviewCIContext{}, MergeBaseMismatch: v.MergeBaseMismatch}}}
	var bad atomic.Value
	var reads atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		reads.Add(1)
		expectedVersion := "change-v2"
		if r.Method == http.MethodPost {
			expectedVersion = "change-v3"
			w.Header().Set("X-FC-CI-Watch", "change-v3")
		}
		if r.Header.Get("X-FC-CI-Watch") != expectedVersion || r.Header.Get("Idempotency-Key") != "" {
			t.Error("missing opt-in or mutating diagnostic read")
		}
		if body := bad.Load(); body != nil {
			_, _ = w.Write([]byte(body.(string)))
		} else if r.Method == http.MethodPost {
			write(w, 200, watch)
		} else {
			write(w, 200, v)
		}
	}))
	defer s.Close()
	c := newClient(t, s)
	got, err := c.ReviewSourceCIAssessment(t.Context(), 2)
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Fatal("strict assessment lost exact mismatch identities", got, err)
	}
	gotWatch, err := c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
	if err != nil || !reflect.DeepEqual(gotWatch, watch) {
		t.Fatal("strict wait lost exact mismatch identities", gotWatch, err)
	}
	a, b := client.AgentReviewCI(got), client.AgentCIWatch(gotWatch)
	if reads.Load() != 2 || a.Diagnostic == nil || b.Targets[0].Diagnostic == nil || a.Diagnostic.Explanation != b.Targets[0].Diagnostic.Explanation || a.Diagnostic.NextAction != b.Targets[0].Diagnostic.NextAction {
		t.Fatal("diagnostics added reads or diverged", reads.Load(), a, b)
	}
	for _, fact := range []string{v.MergeBaseMismatch.PRBaseSHA, v.MergeBaseMismatch.LiveBaseSHA, v.MergeBaseMismatch.MergeSHA, v.MergeBaseMismatch.MergeBaseParentSHA} {
		if !strings.Contains(a.Diagnostic.Explanation, fact) {
			t.Fatal("actual identity missing from self-service explanation", fact)
		}
	}
	if !strings.Contains(a.Diagnostic.Explanation, "independently read stable live") || !strings.Contains(a.Diagnostic.NextAction, "normal authorized merge/rebase") {
		t.Fatal("diagnostic omitted identity distinction or safe recovery", a.Diagnostic)
	}
	for _, mutate := range []func(map[string]any){
		func(m map[string]any) { m["merge_base_mismatch"] = nil },
		func(m map[string]any) { m["merge_base_mismatch"].(map[string]any)["live_base_sha"] = "short" },
		func(m map[string]any) { m["merge_base_mismatch"].(map[string]any)["live_base_sha"] = v.Target.BaseSHA },
		func(m map[string]any) {
			m["merge_base_mismatch"].(map[string]any)["pr_base_sha"] = strings.Repeat("f", 40)
		},
		func(m map[string]any) { m["merge_base_mismatch"].(map[string]any)["raw_body"] = "unsupported" },
		func(m map[string]any) { m["reason"] = "ci_merge_pending" },
	} {
		body, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		mutate(m)
		body, _ = json.Marshal(m)
		bad.Store(string(body))
		_, err := c.ReviewSourceCIAssessment(t.Context(), 2)
		checkError(t, err, "invalid_response")
		body, _ = json.Marshal(watch)
		m = nil
		_ = json.Unmarshal(body, &m)
		row := m["targets"].([]any)[0].(map[string]any)
		mutate(row)
		body, _ = json.Marshal(m)
		bad.Store(string(body))
		_, err = c.WatchCI(t.Context(), []client.CIWatchTarget{pin}, 0)
		checkError(t, err, "invalid_response")
	}
}
