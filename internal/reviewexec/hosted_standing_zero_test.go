package reviewexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func standingZeroEvidenceFixture(t *testing.T) (*attemptFixture, Attempt, ReviewEvidenceInput) {
	t.Helper()
	f, a, in := hostedPublicationFixture(t, true)
	in.Hosted.PublicationID, in.Hosted.ProviderAttemptID, in.Hosted.ReviewIDs = "", "", nil
	in.Hosted.StandingZero = &HostedStandingZeroMetadata{BaselineCaptureID: "review_capture_" + strings.Repeat("2", 32), BaselineCaptureSHA256: strings.Repeat("2", 64), CommentID: 301, CompletionCommentID: 302, RunID: "33333333-3333-3333-3333-333333333333"}
	in.Review.ReviewedFiles = []string{}
	return f, a, in
}

func TestHostedStandingZeroEvidenceRetainedPublicReplayAndGuardedCredit(t *testing.T) {
	f, a, in := standingZeroEvidenceFixture(t)
	initial := attemptState(t, f, a)
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil || receipt.State != "captured" {
		t.Fatal(receipt, err)
	}
	original := adjudicationState(t, f, a)
	reopened, err := New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := reopened.RecordReviewEvidence(t.Context(), attemptProject, a.ID, evidenceClone(in))
	if err != nil || replay != receipt || adjudicationState(t, f, a) != original || attemptState(t, f, a) != initial {
		t.Fatal("restart/replay changed capture or attempt", replay, err)
	}
	retained, err := reopened.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), receipt.ID)
	if err != nil || !reflect.DeepEqual(retained.Capture.Hosted, in.Hosted) || retained.Capture.Review.Findings == nil || len(retained.Capture.Review.Findings) != 0 {
		t.Fatal("standing identities were fabricated/lost", retained, err)
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/evidence/" + receipt.ID
	response := f.request(http.MethodGet, path, f.reader, "", "")
	public := evidenceHTTPRequire(t, response, http.StatusOK, "")
	hosted := public["capture"].(map[string]any)["hosted"].(map[string]any)
	evidenceHTTPKeys(t, hosted, "standing_zero", "trigger_comment_id", "observed_base_sha")
	evidenceHTTPKeys(t, hosted["standing_zero"].(map[string]any), "comment_id", "completion_comment_id", "run_id", "baseline_capture_id", "baseline_capture_sha256")
	for _, forbidden := range []string{"publication_id", "provider_attempt_id", "review_ids", "recent_review", "private"} {
		if bytes.Contains(response.Body.Bytes(), []byte(forbidden)) {
			t.Fatal("public result exposed invented/private source", forbidden)
		}
	}
	projected := publicEvidence(retained)
	projected.Capture.Hosted.StandingZero.CommentID++
	if retained.Capture.Hosted.StandingZero.CommentID != 301 {
		t.Fatal("public standing pointer aliases retained evidence")
	}
	anchor := *a.Anchor
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: anchor.RepositoryID, FullName: anchor.Repository, DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: anchor.PullNumber, RepositoryID: anchor.RepositoryID, State: "open", HeadSHA: anchor.CandidateSHA, BaseSHA: anchor.BaseSHA, BaseRef: anchor.BaseRef, ChangedFiles: 1}, ParentSHA: anchor.ParentSHA, ObservedAt: stamp()}
	hostedComparisonRead(t, f, a, plan, ComparisonState{})
	pull := projectionPull(2)
	if _, err = f.e.PublishEvidence(t.Context(), attemptProject, receipt.ID, &pull); err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, a)
	hostedComparisonRead(t, f, a, plan, ComparisonState{Pending: true})
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "standing-zero-finalize", receipt.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	adjudicationReceipt(t, body, err, receipt.ID, "finalized", 1)
	hostedComparisonRead(t, f, a, plan, ComparisonState{Covered: true})
	coverageComparison(t, f, a, plan, ComparisonState{})
	detail := cliPublicationDetail(t, f, 2, receipt.ID)
	if detail.Run.Found != 0 || detail.Run.Accepted != 0 || detail.Run.Routed != 0 || !detail.Run.Finalized || len(detail.Findings) != 0 {
		t.Fatal("zero result fabricated source counts", detail)
	}
}

func TestHostedStandingZeroEvidenceInvalidShapeIsAtomic(t *testing.T) {
	f, a, valid := standingZeroEvidenceFixture(t)
	before := evidenceState(t, f, a)
	cases := map[string]func(*ReviewEvidenceInput){
		"baseline absent":        func(in *ReviewEvidenceInput) { in.Hosted.StandingZero.BaselineCaptureID = "" },
		"baseline hash mismatch": func(in *ReviewEvidenceInput) { in.Hosted.StandingZero.BaselineCaptureSHA256 = strings.Repeat("3", 64) },
		"current capture baseline": func(in *ReviewEvidenceInput) {
			in.Hosted.StandingZero.BaselineCaptureID = in.CaptureID
			in.Hosted.StandingZero.BaselineCaptureSHA256 = in.CaptureSHA256
		},
		"mixed publication":            func(in *ReviewEvidenceInput) { in.Hosted.PublicationID = "11111111-1111-1111-1111-111111111111" },
		"mixed provider attempt":       func(in *ReviewEvidenceInput) { in.Hosted.ProviderAttemptID = "22222222-2222-2222-2222-222222222222" },
		"invented review":              func(in *ReviewEvidenceInput) { in.Hosted.ReviewIDs = []int64{301} },
		"standing ID missing":          func(in *ReviewEvidenceInput) { in.Hosted.StandingZero.CommentID = 0 },
		"completion ID missing":        func(in *ReviewEvidenceInput) { in.Hosted.StandingZero.CompletionCommentID = 0 },
		"duplicate IDs":                func(in *ReviewEvidenceInput) { in.Hosted.StandingZero.CompletionCommentID = 301 },
		"trigger duplicate":            func(in *ReviewEvidenceInput) { in.Hosted.StandingZero.CommentID = in.Hosted.TriggerCommentID },
		"completion trigger duplicate": func(in *ReviewEvidenceInput) { in.Hosted.StandingZero.CompletionCommentID = in.Hosted.TriggerCommentID },
		"run noncanonical":             func(in *ReviewEvidenceInput) { in.Hosted.StandingZero.RunID = strings.Repeat("A", 36) },
		"null sources":                 func(in *ReviewEvidenceInput) { in.Hosted.Sources = nil },
		"null findings":                func(in *ReviewEvidenceInput) { in.Review.Findings = nil },
		"null reviewed files":          func(in *ReviewEvidenceInput) { in.Review.ReviewedFiles = nil },
		"fabricated file":              func(in *ReviewEvidenceInput) { in.Review.ReviewedFiles = []string{"source.go"} },
		"fabricated count":             func(in *ReviewEvidenceInput) { in.Review.ReportedFindings = 1 },
		"fabricated finding": func(in *ReviewEvidenceInput) {
			in.Review.Findings = []reviewprovider.ParsedFinding{{Title: "Invented", Severity: "high"}}
			in.Review.ReportedFindings = 1
		},
		"incomplete":          func(in *ReviewEvidenceInput) { in.Review.Complete = false; in.Review.Outcome = "uncertain" },
		"wrong observed base": func(in *ReviewEvidenceInput) { in.Hosted.ObservedBaseSHA = strings.Repeat("f", 40) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			in := evidenceClone(valid)
			change(&in)
			_, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
			attemptCode(t, err, "validation_failed")
			if evidenceState(t, f, a) != before {
				t.Fatal("invalid variant partially wrote evidence/events")
			}
		})
	}
}

func TestHostedStandingZeroMetadataStrictConditionalJSON(t *testing.T) {
	_, _, in := standingZeroEvidenceFixture(t)
	data, _ := json.Marshal(in.Hosted)
	for _, alter := range []func(string) string{
		func(s string) string {
			return strings.Replace(s, `"standing_zero":{`, `"publication_id":"", "standing_zero":{`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"standing_zero":{`, `"review_ids":[], "standing_zero":{`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"comment_id":301`, `"comment_id":301,"unknown":true`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"comment_id":301`, `"comment_id":301,"CommentID":301`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"comment_id":301`, `"comment_id":301,"comment_id":301`, 1)
		},
		func(s string) string {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal([]byte(s), &fields)
			fields["standing_zero"] = json.RawMessage("null")
			data, _ := json.Marshal(fields)
			return string(data)
		},
	} {
		var h HostedEvidenceMetadata
		if json.Unmarshal([]byte(alter(string(data))), &h) == nil {
			t.Fatal("malformed conditional metadata accepted")
		}
	}
}
