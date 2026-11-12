package reviewprovider

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func ciHeadAttentionFixture(t *testing.T, mode string) *ciObservationFixture {
	t.Helper()
	f := ciSkippedRunFixture(t, mode)
	original := f.g.baseURL
	branchReads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == candidateBranchPath {
			branchReads++
			base := strings.Repeat("d", 40)
			if mode == "moving base" && branchReads > 1 {
				base = strings.Repeat("e", 40)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]any{"sha": base}})
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, original+r.URL.RequestURI(), nil)
		if err != nil {
			t.Error(err)
			return
		}
		request.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Error(err)
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var value any
		if json.Unmarshal(body, &value) != nil {
			t.Error("fixture JSON")
			return
		}
		// Reuse the same numbered-run/skipped-job fixture on the pinned head.
		var head func(any)
		head = func(v any) {
			switch x := v.(type) {
			case map[string]any:
				for key, child := range x {
					if key == "head_sha" {
						x[key] = candidateHead
					} else {
						head(child)
					}
				}
			case []any:
				for _, child := range x {
					head(child)
				}
			}
		}
		head(value)
		w.WriteHeader(response.StatusCode)
		_ = json.NewEncoder(w).Encode(value)
	}))
	t.Cleanup(server.Close)
	f.g = testGitHub(server, "synthetic-ci-observation-token")
	return f
}

func TestHeadWorkflowAttentionKeepsMismatchedGateAndUsesExactProof(t *testing.T) {
	baseline := ciHeadAttentionFixture(t, "skip")
	gate, err := baseline.g.ObserveCIGate(t.Context(), ciObservationTarget(), "main")
	if err != nil {
		t.Fatal(err)
	}
	f := ciHeadAttentionFixture(t, "skip")
	got, rows, err := f.g.ObserveCIGateWithWorkflowAttention(t.Context(), ciObservationTarget(), "main", []int64{30, 31}, []int64{31}, nil)
	if err != nil || len(rows) != 1 || rows[0].RunID != 60 || !CIWorkflowFailureApplies(got, rows[0]) || CIWorkflowAttentionBasis(got) != CIWorkflowHeadOnlyBasis || !reflect.DeepEqual(workflowGateFacts(t, gate), workflowGateFacts(t, got)) {
		t.Fatal("same-head attention changed the gate or lost exact proof", got, rows, err)
	}
	if got.State != "unproven" || got.Reason != "ci_merge_base_mismatch" || got.Results != nil || got.Actions != nil || got.SelectedCommitSHA != "" || got.Selection != "" || got.Candidate.State != "unproven" || got.WorkflowHeadActions.HeadSHA != candidateHead {
		t.Fatal("head inventory supplied applicable CI", got)
	}
	_, calls := f.snapshot()
	f.mu.Lock()
	walks := f.headWalks
	f.mu.Unlock()
	if calls[actionsCurrentPath] != 0 || calls[actionsJobsPath] != 0 || walks != 2 {
		t.Fatal("unselected/required workflow was probed or final check missing", calls, walks)
	}
	for _, damage := range []string{"recheck", "foreign head", "foreign repo", "bad candidate", "wrong job", "repaired attempt"} {
		t.Run(damage, func(t *testing.T) {
			o, row := got, rows[0]
			switch damage {
			case "recheck":
				o.WorkflowActions = nil
			case "foreign head":
				o.Target.HeadSHA = strings.Repeat("f", 40)
			case "foreign repo":
				o.Target.RepositoryID++
			case "bad candidate":
				c := *o.Candidate
				c.State = "complete"
				o.Candidate = &c
			case "wrong job":
				row.JobID++
			case "repaired attempt":
				current := *o.WorkflowActions
				current.Runs = append([]ActionsHeadRun(nil), current.Runs...)
				current.Runs[1].RunAttempt++
				o.WorkflowActions = &current
			}
			if CIWorkflowFailureApplies(o, row) {
				t.Fatal("unproven head-only attention accepted", damage)
			}
		})
	}
}

func TestHeadWorkflowAttentionPreservesSkippedJobCurrentnessAndBounds(t *testing.T) {
	for _, mode := range []string{"success", "missing", "matrix ambiguity", "pending", "cancelled", "different event", "repaired", "new run during recheck", "rerun during recheck", "capacity", "moving base"} {
		t.Run(mode, func(t *testing.T) {
			f := ciHeadAttentionFixture(t, mode)
			got, rows, err := f.g.ObserveCIGateWithWorkflowAttention(t.Context(), ciObservationTarget(), "main", []int64{31}, []int64{31}, nil)
			if len(rows) != 0 || got.State != "unproven" || got.Actions != nil || got.Results != nil || got.SelectedCommitSHA != "" {
				t.Fatal("stale head failure or CI credit", mode, rows, got, err)
			}
			if mode == "capacity" {
				if !errors.Is(err, ErrCIWorkflowAttemptLimit) {
					t.Fatal("attention fanout capacity was not reported", err)
				}
			} else if err != nil {
				t.Fatal("unexpected head attention read error", err)
			}
		})
	}
}

func TestHeadWorkflowAttentionRequiresSelectionAndExplicitOptIn(t *testing.T) {
	for _, opt := range []bool{false, true} {
		f := ciHeadAttentionFixture(t, "skip")
		var rows []CIWorkflowFailure
		var err error
		if opt {
			_, rows, err = f.g.ObserveCIGateWithWorkflowAttention(t.Context(), ciObservationTarget(), "main", nil, nil, nil)
		} else {
			_, rows, err = f.g.ObserveCIGateWithWorkflows(t.Context(), ciObservationTarget(), "main", []int64{31})
		}
		_, calls := f.snapshot()
		if err != nil || len(rows) != 0 || calls[actionsRunsPath] != 0 {
			t.Fatal("default/older wait added head-only provider reads", opt, rows, calls, err)
		}
	}
	f := ciHeadAttentionFixture(t, "skip")
	if _, _, err := f.g.ObserveCIGateWithWorkflowAttention(t.Context(), ciObservationTarget(), "main", []int64{31}, []int64{32}, nil); err == nil {
		t.Fatal("unselected head workflow accepted")
	}
}
