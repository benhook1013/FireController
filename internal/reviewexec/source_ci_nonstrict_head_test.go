package reviewexec

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func sourceCINonstrictHeadObservation(target reviewprovider.CITarget, state string) reviewprovider.CIObservation {
	o := sourceCITestObservation(target, state)
	mismatch := testCIMismatchObservation(target)
	o.Candidate, o.MergeState = mismatch.Candidate, mismatch.MergeState
	return o
}

func TestSourceCINonstrictHeadRetainsProofAndReadsWithoutRecredit(t *testing.T) {
	for _, state := range []string{"satisfied", "pending", "failed", "not_required", "old mismatch"} {
		t.Run(state, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			var collected reviewprovider.CIObservation
			reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				if state == "old mismatch" {
					collected = testCIMismatchObservation(target)
					collected.Authority = sourceCITestObservation(target, "satisfied").Authority
				} else {
					collected = sourceCINonstrictHeadObservation(target, state)
				}
				return collected, nil
			}}
			receipt := refreshEnqueue(t, f, "nonstrict-head", sourceCIRequest())
			cancel, done := refreshStart(t, f, reader)
			operation := refreshWait(t, f, receipt.ID, "completed")
			refreshStop(t, cancel, done)
			wantState := state
			if state == "old mismatch" {
				wantState = "unproven"
			}
			if operation.Observation == nil || operation.CI == nil || operation.CI.State != wantState || operation.CI.MergeState != "unproven" {
				t.Fatal("current-head proof not retained or mismatch upgraded", operation)
			}
			var raw string
			if err := f.store.DB().QueryRow(`SELECT ci_observation_json FROM review_source_refreshes WHERE id=?`, receipt.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var retained reviewprovider.CIObservation
			if json.Unmarshal([]byte(raw), &retained) != nil || !reflect.DeepEqual(retained, collected) {
				t.Fatal("retained source/CI evidence rewritten", raw)
			}
			before := sourceCITestState(t, f)
			calls := reader.ciCalls.Load()
			assessment := readSourceCI(t, f)
			if assessment.State != wantState || assessment.MergeState != "unproven" || assessment.Target != *operation.Observation {
				t.Fatal("read changed source or CI state", assessment, operation)
			}
			if state == "old mismatch" {
				if assessment.Required != 0 || assessment.Selection != "" || assessment.Reason != "ci_merge_base_mismatch" {
					t.Fatal("historical mismatch recredited", assessment)
				}
			} else if state == "not_required" {
				if assessment.Required != 0 || assessment.Selection != "" || assessment.SelectedCommitSHA != "" || assessment.Reason != "" {
					t.Fatal("empty authority invented head proof", assessment)
				}
			} else if assessment.Selection != "head" || assessment.SelectedCommitSHA != assessment.Target.HeadSHA || assessment.Required != 1 || assessment.MergeBaseMismatch != nil {
				t.Fatal("head proof claimed merge verification or dropped checks", assessment)
			}
			request := httptest.NewRequest("GET", "/api/v1/projects/"+attemptProject+"/reviews/source/2/ci?diagnostics=true", nil)
			request.Header.Set("Authorization", "Bearer "+f.reader)
			request.Header.Set("X-FC-CI-Watch", "change-v2")
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, request)
			var wire SourceCIAssessment
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &wire) != nil || wire.State != wantState || wire.RefreshID != receipt.ID || wire.Checks == nil {
				t.Fatal("public local read lost retained proof", w.Code, w.Body.String())
			}
			again := refreshRead(t, f, receipt.ID)
			if again.CI == nil || again.CI.State != wantState || sourceCITestState(t, f) != before || reader.ciCalls.Load() != calls {
				t.Fatal("retained reads recollected or reinterpreted evidence", again)
			}
		})
	}
}

func TestSourceCINonstrictHeadRejectsUnboundOrStrictEvidence(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}
	for name, change := range map[string]func(*reviewprovider.CIObservation){
		"strict":                 func(o *reviewprovider.CIObservation) { o.Authority.Strict = true },
		"missing authority":      func(o *reviewprovider.CIObservation) { o.Authority.State = "unproven" },
		"wrong authority branch": func(o *reviewprovider.CIObservation) { o.Authority.Branch = "elsewhere" },
		"missing candidate":      func(o *reviewprovider.CIObservation) { o.Candidate = nil },
		"missing mismatch":       func(o *reviewprovider.CIObservation) { o.Candidate.MergeBaseMismatch = nil },
		"wrong head":             func(o *reviewprovider.CIObservation) { o.Candidate.HeadSHA = strings.Repeat("f", 40) },
		"wrong repository":       func(o *reviewprovider.CIObservation) { o.Candidate.RepositoryID++ },
		"wrong target":           func(o *reviewprovider.CIObservation) { o.Target.PullNumber++ },
		"wrong PR base": func(o *reviewprovider.CIObservation) {
			o.Candidate.MergeBaseMismatch.PRBaseSHA = strings.Repeat("f", 40)
		},
		"wrong merge parent": func(o *reviewprovider.CIObservation) {
			o.Candidate.MergeBaseMismatch.MergeBaseParentSHA = strings.Repeat("f", 40)
		},
		"no live mismatch":       func(o *reviewprovider.CIObservation) { o.Candidate.MergeBaseMismatch.LiveBaseSHA = target.BaseSHA },
		"malformed SHA":          func(o *reviewprovider.CIObservation) { o.Candidate.MergeBaseMismatch.LiveBaseSHA = "short" },
		"missing candidate time": func(o *reviewprovider.CIObservation) { o.Candidate.ObservedAt = time.Time{} },
		"wrong candidate state":  func(o *reviewprovider.CIObservation) { o.Candidate.State = "complete" },
		"wrong candidate reason": func(o *reviewprovider.CIObservation) { o.Candidate.Reason = "ci_merge_pending" },
		"claims verified merge":  func(o *reviewprovider.CIObservation) { o.MergeState = "verified" },
		"stale merge selection": func(o *reviewprovider.CIObservation) {
			o.Selection = "test_merge"
			o.SelectedCommitSHA = o.Candidate.MergeBaseMismatch.MergeSHA
		},
		"wrong assessment":           func(o *reviewprovider.CIObservation) { o.Assessment.Checks[0].StatusID++ },
		"unexpected merge inventory": func(o *reviewprovider.CIObservation) { v := *o.Results; v.HeadSHA = ""; o.TestMergeResults = &v },
		"missing result":             func(o *reviewprovider.CIObservation) { o.Results = nil },
		"foreign result":             func(o *reviewprovider.CIObservation) { o.Results.HeadSHA = strings.Repeat("f", 40) },
		"missing actions":            func(o *reviewprovider.CIObservation) { o.Actions = nil },
		"missing attempts":           func(o *reviewprovider.CIObservation) { o.Attempts = nil },
	} {
		t.Run(name, func(t *testing.T) {
			o := sourceCINonstrictHeadObservation(target, "satisfied")
			source := SourceObservation{RepositoryID: target.RepositoryID, RepositoryName: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, ObservedAt: o.StartedAt.Format(time.RFC3339Nano)}
			if !validSourceCI(o, source) {
				t.Fatal("positive head fixture rejected")
			}
			change(&o)
			if validSourceCI(o, source) {
				t.Fatal("unproven/strict head credit accepted", name)
			}
		})
	}
}

func TestSourceCINonstrictEmptyAuthorityRequiresExactAbsenceShape(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}
	for name, change := range map[string]func(*reviewprovider.CIObservation){
		"strict":            func(o *reviewprovider.CIObservation) { o.Authority.Strict = true },
		"nil requirements":  func(o *reviewprovider.CIObservation) { o.Authority.Checks = nil },
		"missing authority": func(o *reviewprovider.CIObservation) { o.Authority.State = "unproven" },
		"selected head":     func(o *reviewprovider.CIObservation) { o.Selection = "head"; o.SelectedCommitSHA = target.HeadSHA },
		"invented results": func(o *reviewprovider.CIObservation) {
			o.Results = sourceCITestObservation(target, "satisfied").Results
		},
		"invented actions": func(o *reviewprovider.CIObservation) {
			o.Actions = sourceCITestObservation(target, "satisfied").Actions
		},
		"non-nil attempts": func(o *reviewprovider.CIObservation) { o.Attempts = []reviewprovider.ActionsAttempt{} },
		"invented assessment": func(o *reviewprovider.CIObservation) {
			o.Assessment = sourceCITestObservation(target, "satisfied").Assessment
		},
		"missing assessment": func(o *reviewprovider.CIObservation) { o.Assessment = reviewprovider.RequiredCIAssessment{} },
		"wrong mismatch": func(o *reviewprovider.CIObservation) {
			o.Candidate.MergeBaseMismatch.MergeBaseParentSHA = strings.Repeat("f", 40)
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := sourceCINonstrictHeadObservation(target, "not_required")
			source := SourceObservation{RepositoryID: target.RepositoryID, RepositoryName: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, ObservedAt: o.StartedAt.Format(time.RFC3339Nano)}
			if !validSourceCI(o, source) {
				t.Fatal("positive empty-authority fixture rejected")
			}
			change(&o)
			if validSourceCI(o, source) {
				t.Fatal("malformed no-required proof accepted", name)
			}
		})
	}
}
