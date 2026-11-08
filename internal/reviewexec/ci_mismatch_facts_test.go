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

func testCIMismatchObservation(target reviewprovider.CITarget) reviewprovider.CIObservation {
	now := time.Now().UTC()
	facts := &reviewprovider.CIMergeBaseMismatch{PRBaseSHA: target.BaseSHA, LiveBaseSHA: strings.Repeat("d", 40), MergeSHA: strings.Repeat("e", 40), MergeBaseParentSHA: target.BaseSHA}
	return reviewprovider.CIObservation{Target: target, StartedAt: now, ObservedAt: now, State: "unproven", Reason: "ci_merge_base_mismatch", MergeState: "unproven", Candidate: &reviewprovider.CICandidate{State: "unproven", Reason: "ci_merge_base_mismatch", RepositoryID: target.RepositoryID, Repository: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, MergeState: "unproven", ObservedAt: now, MergeBaseMismatch: facts}}
}

func TestSourceCIMismatchFactsRetainedAndWireOptInOnly(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return testCIMismatchObservation(target), nil
	}}
	receipt := refreshEnqueue(t, f, "mismatch-facts", sourceCIRequest())
	cancel, done := refreshStart(t, f, reader)
	refreshWait(t, f, receipt.ID, "completed")
	refreshStop(t, cancel, done)
	before := sourceCITestState(t, f)
	want := readSourceCI(t, f)
	if want.MergeBaseMismatch == nil || want.Required != 0 || want.State != "unproven" || want.SelectedCommitSHA != "" {
		t.Fatal("retained mismatch lost or credited", want)
	}
	for _, version := range []string{"", "change-v1", "change-v2"} {
		r := httptest.NewRequest("GET", "/api/v1/projects/"+attemptProject+"/reviews/source/2/ci?diagnostics=true", nil)
		r.Header.Set("Authorization", "Bearer "+f.reader)
		if version != "" {
			r.Header.Set("X-FC-CI-Watch", version)
		}
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		var got SourceCIAssessment
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Reason != want.Reason || got.RefreshID != want.RefreshID {
			t.Fatal("mismatch projection lost original observation", version, w.Code, w.Body.String())
		}
		if version == "change-v2" {
			if !reflect.DeepEqual(got.MergeBaseMismatch, want.MergeBaseMismatch) {
				t.Fatal("exact retained identities missing", got)
			}
		} else if got.MergeBaseMismatch != nil || strings.Contains(w.Body.String(), "merge_base_mismatch\":") {
			t.Fatal("old strict response shape changed", version, w.Body.String())
		}
	}
	if sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
		t.Fatal("diagnostic read changed history or fetched provider")
	}
}

func TestCIMismatchFactsRejectUnboundOrContradictoryEvidence(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main"}
	for name, change := range map[string]func(*reviewprovider.CIObservation){
		"wrong target": func(o *reviewprovider.CIObservation) { o.Candidate.RepositoryID++ },
		"wrong reason": func(o *reviewprovider.CIObservation) { o.Candidate.Reason = "ci_merge_pending" },
		"bad SHA":      func(o *reviewprovider.CIObservation) { o.Candidate.MergeBaseMismatch.LiveBaseSHA = "short" },
		"same parent":  func(o *reviewprovider.CIObservation) { o.Candidate.MergeBaseMismatch.LiveBaseSHA = target.BaseSHA },
		"wrong PR base": func(o *reviewprovider.CIObservation) {
			o.Candidate.MergeBaseMismatch.PRBaseSHA = strings.Repeat("f", 40)
		},
		"selected commit": func(o *reviewprovider.CIObservation) { o.Selection, o.SelectedCommitSHA = "head", target.HeadSHA },
	} {
		t.Run(name, func(t *testing.T) {
			o := testCIMismatchObservation(target)
			source := SourceObservation{RepositoryID: target.RepositoryID, RepositoryName: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, ObservedAt: o.StartedAt.Format(time.RFC3339Nano)}
			if !validSourceCI(o, source) {
				t.Fatal("invalid positive fixture")
			}
			change(&o)
			if validSourceCI(o, source) {
				t.Fatal("contradictory mismatch facts accepted")
			}
		})
	}
}

type ciMismatchWatchReader struct{ *ciWatchReader }

func (r *ciMismatchWatchReader) ObserveCIGate(_ context.Context, target reviewprovider.CITarget, _ string) (reviewprovider.CIObservation, error) {
	return testCIMismatchObservation(target), nil
}

func TestCIWatchMismatchFactsWireOptInPreservesPendingGate(t *testing.T) {
	f, s, _, base, _ := ciWatchFixture(t)
	s.readers["test-capability"] = &ciMismatchWatchReader{base}
	for _, version := range []string{"change-v1", "change-v2"} {
		body := `{"targets":[{"pull_number":42,"head_sha":"` + strings.Repeat("a", 40) + `"}]}`
		r := httptest.NewRequest("POST", "/api/v1/projects/"+attemptProject+"/reviews/ci/watch", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.reader)
		r.Header.Set("X-FC-CI-Watch", version)
		r.Header.Set("Prefer", "wait=1")
		out := make(chan *httptest.ResponseRecorder, 1)
		go func() { w := httptest.NewRecorder(); f.handler.ServeHTTP(w, r); out <- w }()
		ciWatchRegistered(t, s, 1)
		watchTick(t, f, s)
		w := <-out
		var got CIWatchResult
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.State != "pending" || !got.TimedOut || got.Targets[0].Reason != "ci_merge_base_mismatch" || got.Targets[0].Required != 0 || got.Targets[0].CICommitSHA != "" {
			t.Fatal("mismatch watch changed gate", version, w.Code, w.Body.String())
		}
		facts := got.Targets[0].MergeBaseMismatch
		if version == "change-v2" {
			if facts == nil || facts.PRBaseSHA != got.Targets[0].ObservedBaseSHA || facts.LiveBaseSHA != strings.Repeat("d", 40) {
				t.Fatal("watch lost actual mismatch identities", got)
			}
		} else if facts != nil || strings.Contains(w.Body.String(), "merge_base_mismatch\":") {
			t.Fatal("legacy strict watch shape changed", w.Body.String())
		}
		ciWatchRegistered(t, s, 0)
	}
}
