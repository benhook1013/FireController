package reviewprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Keep the established provider fixture and change only the configured strict
// policy and the live base tip. Current-head job proof still uses real readers.
func newNonstrictHeadFixture(t *testing.T, mode string) *ciObservationFixture {
	t.Helper()
	underlying := "fallback"
	if mode == "wrong_app" || mode == "newer_pending" || mode == "move_results" || mode == "move_actions" || mode == "move_candidate" {
		underlying = mode
	}
	f := &ciObservationFixture{t: t, mode: underlying, counts: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := httptest.NewRecorder()
		f.serve(recorded, r)
		if recorded.Code != http.StatusOK {
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		var body any
		if json.Unmarshal(recorded.Body.Bytes(), &body) != nil {
			t.Error("invalid fixture JSON")
			return
		}
		_, counts := f.snapshot()
		switch r.URL.Path {
		case "/repos/owner/repo/branches/main/protection":
			strict := mode == "strict" || mode == "move_authority" && counts[r.URL.Path] > 1
			required := body.(map[string]any)["required_status_checks"].(map[string]any)
			required["strict"] = strict
			if mode == "empty" {
				required["checks"], required["contexts"] = []any{}, []any{}
			}
			if mode == "attention" {
				required["checks"].([]any)[0].(map[string]any)["context"] = "missing-required"
			}
		case candidateBranchPath:
			tip := strings.Repeat("d", 40)
			if mode == "moving_base" && counts[r.URL.Path]%2 == 0 || mode == "move_mismatch" && counts[r.URL.Path] > 2 {
				tip = strings.Repeat("e", 40)
			}
			body.(map[string]any)["commit"].(map[string]any)["sha"] = tip
		case candidateCommitPath:
			if mode == "wrong_parent" {
				body = candidateCommitFixture(strings.Repeat("e", 40), candidateHead)
			}
		}
		// Some existing fixture modes normally select merge CI. Rebind their
		// Actions records to the explicitly requested head, never the merge.
		if strings.HasPrefix(r.URL.Path, actionsRunsPath) {
			var rebind func(any)
			rebind = func(value any) {
				switch v := value.(type) {
				case map[string]any:
					for key, child := range v {
						if key == "head_sha" {
							v[key] = candidateHead
						} else {
							rebind(child)
						}
					}
				case []any:
					for _, child := range v {
						rebind(child)
					}
				}
			}
			rebind(body)
		}
		if mode == "failed" || mode == "attention" {
			if strings.HasSuffix(r.URL.Path, "/check-runs") {
				for _, check := range body.(map[string]any)["check_runs"].([]any) {
					check.(map[string]any)["conclusion"] = "failure"
				}
			}
			if strings.HasSuffix(r.URL.Path, "/jobs") {
				for _, job := range body.(map[string]any)["jobs"].([]any) {
					job.(map[string]any)["conclusion"] = "failure"
				}
			}
		}
		if mode == "absent" && strings.HasSuffix(r.URL.Path, "/check-runs") {
			body = ciEnvelope([]map[string]any{}, 0)
		}
		if mode == "status_pending" && strings.HasSuffix(r.URL.Path, "/statuses") {
			status := ciStatusFixture(91)
			status["context"], status["state"] = "build", "pending"
			body = []any{status}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	f.g = testGitHub(server, "synthetic-ci-observation-token")
	return f
}

func TestNonstrictHeadCIAssessesCurrentHeadWithoutStaleMergeCredit(t *testing.T) {
	for _, mode := range []string{"success", "failed", "newer_pending", "absent", "wrong_app", "status_pending"} {
		t.Run(mode, func(t *testing.T) {
			f := newNonstrictHeadFixture(t, mode)
			o, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
			want := map[string]string{"success": "satisfied", "failed": "failed", "newer_pending": "pending", "absent": "unproven", "wrong_app": "unproven", "status_pending": "pending"}[mode]
			if err != nil || o.State != want || o.Selection != "head" || o.SelectedCommitSHA != candidateHead || o.Results == nil || o.Results.HeadSHA != candidateHead || o.Actions == nil || o.Actions.HeadSHA != candidateHead || o.TestMergeResults != nil || o.Candidate == nil || !NonstrictHeadCICandidate(*o.Candidate, o.Target, o.Authority) || o.Candidate.State != "unproven" || o.MergeState != "unproven" || o.Candidate.MergeSHA != "" {
				t.Fatal("non-strict head CI lost proof or fabricated merge coverage", mode, o, err)
			}
			paths, counts := f.snapshot()
			for _, path := range paths {
				if strings.Contains(path, "/commits/"+candidateMerge+"/") {
					t.Fatal("stale merge CI was selected or queried", path)
				}
			}
			if mode == "success" && (counts[candidatePullPath] != 4 || counts[candidateBranchPath] != 4 || counts["/repos/owner/repo/branches/main/protection"] != 2 || counts["/repos/owner/repo/commits/"+candidateHead+"/check-runs"] != 2) {
				t.Fatal("missing final source/authority/head-result proof", counts)
			}
		})
	}
}

func TestNonstrictHeadCIRetainsStrictAndMovementGuards(t *testing.T) {
	for _, mode := range []string{"strict", "moving_base", "wrong_parent", "move_authority", "move_mismatch", "move_candidate", "move_results", "move_actions"} {
		t.Run(mode, func(t *testing.T) {
			f := newNonstrictHeadFixture(t, mode)
			o, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
			if err != nil || o.State != "unproven" || o.Reason == "" {
				t.Fatal("changed or strict comparison gained CI credit", mode, o, err)
			}
			if (mode == "strict" || mode == "moving_base" || mode == "wrong_parent") && (o.Results != nil || o.Actions != nil || o.Selection != "") {
				t.Fatal("unsafe candidate reached head collection", mode, o)
			}
		})
	}
}

func TestNonstrictHeadCICandidateRejectsMissingOrForeignProof(t *testing.T) {
	f := newNonstrictHeadFixture(t, "success")
	o, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
	if err != nil || o.Candidate == nil || !NonstrictHeadCICandidate(*o.Candidate, o.Target, o.Authority) {
		t.Fatal("fixture missing coherent head candidate", o, err)
	}
	for _, damage := range []string{"strict", "authority", "missing checks", "missing mismatch", "malformed mismatch", "foreign repo", "foreign PR", "wrong head", "wrong base", "wrong ref", "wrong parent", "fake verified"} {
		t.Run(damage, func(t *testing.T) {
			c, a := *o.Candidate, o.Authority
			m := *c.MergeBaseMismatch
			c.MergeBaseMismatch = &m
			switch damage {
			case "strict":
				a.Strict = true
			case "authority":
				a.State = "unproven"
			case "missing checks":
				a.Checks = nil
			case "missing mismatch":
				c.MergeBaseMismatch = nil
			case "malformed mismatch":
				m.LiveBaseSHA = "invalid"
			case "foreign repo":
				c.Repository = "foreign/repo"
			case "foreign PR":
				c.PullNumber++
			case "wrong head":
				c.HeadSHA = strings.Repeat("e", 40)
			case "wrong base":
				c.BaseSHA = strings.Repeat("e", 40)
			case "wrong ref":
				c.BaseRef = "other"
			case "wrong parent":
				m.MergeBaseParentSHA = strings.Repeat("e", 40)
			case "fake verified":
				c.MergeState = "verified"
			}
			if NonstrictHeadCICandidate(c, o.Target, a) {
				t.Fatal("unproven head exception accepted", damage)
			}
		})
	}
}

func TestNonstrictHeadCIAllowsExplicitWorkflowAttentionWithoutGateCredit(t *testing.T) {
	f := newNonstrictHeadFixture(t, "attention")
	o, rows, err := f.g.ObserveCIGateWithWorkflowAttention(t.Context(), ciObservationTarget(), "main", []int64{30}, []int64{30}, []int64{30})
	if err != nil || o.State != "unproven" || o.Selection != "head" || len(rows) != 1 || rows[0].WorkflowID != 30 || rows[0].Basis != "" || !CIWorkflowFailureApplies(o, rows[0]) || o.Assessment.State != "unproven" {
		t.Fatal("head-policy candidate blocked selected attention or changed required gate", o, rows, err)
	}
	if CIWorkflowAttentionBasis(o) != "" || o.WorkflowHeadActions != nil || o.TestMergeResults != nil {
		t.Fatal("applicable head CI was mislabelled or stale merge was selected", o)
	}
}

func TestNonstrictHeadCIEmptyAuthorityNeedsNoResultsOrSelection(t *testing.T) {
	f := newNonstrictHeadFixture(t, "empty")
	o, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
	if err != nil || o.State != "not_required" || o.Assessment.State != "not_required" || o.Selection != "" || o.SelectedCommitSHA != "" || o.Results != nil || o.Actions != nil || o.TestMergeResults != nil || len(o.Attempts) != 0 || len(o.Assessment.Checks) != 0 || o.Candidate == nil || !NonstrictHeadCICandidate(*o.Candidate, o.Target, o.Authority) {
		t.Fatal("complete empty authority invented CI proof or retained a blanket refusal", o, err)
	}
	paths, counts := f.snapshot()
	if counts[candidatePullPath] != 4 || counts[candidateBranchPath] != 4 || counts["/repos/owner/repo/branches/main/protection"] != 2 {
		t.Fatal("empty authority skipped source/authority rechecks", counts)
	}
	for _, path := range paths {
		if strings.HasPrefix(path, "/repos/owner/repo/commits/") || strings.Contains(path, "/actions/") {
			t.Fatal("empty authority fetched unneeded CI inventory", path)
		}
	}
}
