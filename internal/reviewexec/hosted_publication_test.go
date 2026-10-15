package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// This fixture supplies trusted final metadata, not provider scope/inventory proof.
func hostedPublicationFixture(t *testing.T, zero bool) (*attemptFixture, Attempt, ReviewEvidenceInput) {
	t.Helper()
	ctx := context.Background()
	f := newAttemptFixture(t)
	for _, ensure := range []func(context.Context, *core.Store) error{EnsureEvidenceSchema, EnsureAdjudicationSchema, reviews.EnsureSchema} {
		if err := ensure(ctx, f.store); err != nil {
			t.Fatal(err)
		}
	}
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "hosted-publication-request", "hosted", 2, 1))
	anchor := attemptAnchor(attemptProject, 2)
	anchor.ParentSHA = anchor.BaseSHA
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(ctx, attemptProject, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(ctx, attemptProject, a.ID, a.Revision, "active", "posted", "101")
	if err != nil {
		t.Fatal(err)
	}
	h, _, parsed := hostedEvidenceMetadataFixture()
	h.ObservedBaseSHA = anchor.ParentSHA
	for i := range parsed.Findings {
		parsed.Findings[i].Title = fmt.Sprintf("Immutable %s finding", h.Sources[i].Kind)
		parsed.Findings[i].Detail = "Retained literal <script>source</script> and Unicode Ω"
		parsed.Findings[i].Severity = "medium"
	}
	if zero {
		h.Sources = []HostedFindingSource{}
		parsed.Findings, parsed.ReportedFindings = []reviewprovider.ParsedFinding{}, 0
	}
	digest := strings.Repeat("1", 64)
	in := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 3072, StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: parsed, Hosted: h}
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	return f, a, in
}

func hostedPublicationComparison(t *testing.T, f *attemptFixture, a Attempt) {
	t.Helper()
	anchor := *a.Anchor
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: anchor.RepositoryID, FullName: anchor.Repository, DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: anchor.PullNumber, RepositoryID: anchor.RepositoryID, State: "open", HeadSHA: anchor.CandidateSHA, BaseSHA: anchor.BaseSHA, BaseRef: anchor.BaseRef, ChangedFiles: 1}, ParentSHA: anchor.ParentSHA, ObservedAt: stamp()}
	before := adjudicationState(t, f, a)
	comparison, err := f.e.CLIComparison(t.Context(), attemptProject, plan)
	if err != nil || comparison != (CLIComparisonState{}) || adjudicationState(t, f, a) != before {
		t.Fatal("hosted source gained CLI credit/pending state or comparison mutated state", comparison, err)
	}
}

func TestHostedPublicationRetainedEvidenceAndSharedControls(t *testing.T) {
	f, a, in := hostedPublicationFixture(t, false)
	trap := &cliPublicationHTTPTrap{}
	oldTransport, oldClient := http.DefaultTransport, http.DefaultClient
	http.DefaultTransport, http.DefaultClient = trap, &http.Client{Transport: trap}
	t.Cleanup(func() { http.DefaultTransport, http.DefaultClient = oldTransport, oldClient })
	journal := attemptState(t, f, a)
	receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	retained, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil || retained.State != "captured" || retained.Revision != 1 {
		t.Fatal("trusted hosted capture failed", retained, err)
	}
	before := adjudicationState(t, f, a)
	replay, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, evidenceClone(in))
	if err != nil || replay != retained || adjudicationState(t, f, a) != before {
		t.Fatal("immutable hosted capture replay changed state", err)
	}
	changed := evidenceClone(in)
	changed.Hosted.TriggerCommentID++
	_, err = f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, changed)
	attemptCode(t, err, "review_conflict")
	if adjudicationState(t, f, a) != before {
		t.Fatal("changed hosted capture replay altered immutable state")
	}
	path := "/api/v1/projects/" + attemptProject + "/reviews/evidence/" + retained.ID
	response := f.request(http.MethodGet, path, f.reader, "", "")
	public := evidenceHTTPRequire(t, response, http.StatusOK, "")
	var exact evidenceResponse
	if err := json.Unmarshal(response.Body.Bytes(), &exact); err != nil {
		t.Fatal(err)
	}
	h := in.Hosted
	want := &hostedPublicationResponse{PublicationID: h.PublicationID, ProviderAttemptID: h.ProviderAttemptID, TriggerCommentID: h.TriggerCommentID, ReviewIDs: h.ReviewIDs, ObservedBaseSHA: h.ObservedBaseSHA}
	if exact.Channel != "hosted" || !reflect.DeepEqual(exact.Capture.Hosted, want) || len(exact.Capture.Findings) != 3 || len(exact.Capture.ReviewedFiles) != 0 {
		t.Fatal("public capture lost hosted identity or invented files", exact)
	}
	capture := public["capture"].(map[string]any)
	evidenceHTTPKeys(t, capture["hosted"].(map[string]any), "publication_id", "provider_attempt_id", "trigger_comment_id", "review_ids", "observed_base_sha")
	for i, finding := range exact.Capture.Findings {
		if finding.Finding != int64(i) || finding.Source == nil || *finding.Source != h.Sources[i] || finding.Title != in.Review.Findings[i].Title || finding.Path != in.Review.Findings[i].Path || finding.StartLine != in.Review.Findings[i].StartLine || finding.EndLine != in.Review.Findings[i].EndLine {
			t.Fatal("public atomic finding/source/location changed", finding)
		}
	}
	original := append([]byte(nil), response.Body.Bytes()...)
	pull := projectionPull(2)
	publication, err := f.e.PublishEvidence(t.Context(), attemptProject, retained.ID, &pull)
	if err != nil || publication.Outcome != "observed" {
		t.Fatal("hosted native publication failed", publication, err)
	}
	detail := cliPublicationDetail(t, f, 2, retained.ID)
	if detail.Run.Channel != "hosted" || detail.Run.Found != 3 || detail.Run.Accepted != 0 || detail.Run.Finalized || !detail.Run.Complete || !detail.Run.Attributable || detail.Run.HeadSHA != in.ExpectedAnchor.CandidateSHA || len(detail.Findings) != 3 {
		t.Fatal("hosted publication lost source or fabricated adjudication", detail)
	}
	for i, finding := range detail.Findings {
		if finding.Title != in.Review.Findings[i].Title || finding.Detail != in.Review.Findings[i].Detail || finding.Disposition != "unadjudicated" {
			t.Fatal("hosted atomic source content changed", finding)
		}
	}
	if attemptState(t, f, a) != journal || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != receipts || adjudicationRead(t, f, retained.ID).Revision != 0 {
		t.Fatal("capture/publication settled attempt or manufactured control receipts")
	}
	before, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
	publishedAgain, err := f.e.PublishEvidence(t.Context(), attemptProject, retained.ID, nil)
	if err != nil || publishedAgain != publication || adjudicationState(t, f, a) != before || projectionDigest(t, f, "") != projection {
		t.Fatal("hosted publication replay changed journal, receipts or projection", err)
	}
	hostedPublicationComparison(t, f, a)
	decision := `{"expected_revision":0,"finding":0,"disposition":"accepted","reason":"Explicit hosted acceptance"}`
	accepted := evidenceHTTPReceipt(t, f.request(http.MethodPost, path+"/decisions", f.writer, "hosted-accept", decision), retained.ID, "adjudicated", 1)
	before, projection = adjudicationState(t, f, a), projectionDigest(t, f, "")
	if retry := f.request(http.MethodPost, path+"/decisions", f.writer, "hosted-accept", decision); retry.Code != http.StatusOK || !bytes.Equal(retry.Body.Bytes(), accepted) {
		t.Fatal("hosted decision same-key recovery lost receipt")
	}
	evidenceHTTPRequire(t, f.request(http.MethodPost, path+"/decisions", f.writer, "hosted-accept", strings.Replace(decision, "Explicit hosted acceptance", "Changed request", 1)), http.StatusConflict, "idempotency_conflict")
	if adjudicationState(t, f, a) != before || projectionDigest(t, f, "") != projection {
		t.Fatal("hosted decision replay/conflict caused extra effects")
	}
	adjudicationDecide(t, f, retained.ID, "hosted-reject", 1, 1, "rejected")
	adjudicationDecide(t, f, retained.ID, "hosted-accept-duplicate", 2, 2, "accepted")
	evidenceHTTPRequire(t, f.request(http.MethodPost, path+"/finalization", f.writer, "hosted-too-early", `{"expected_revision":3}`), http.StatusConflict, "review_conflict")
	if attemptState(t, f, a) != journal {
		t.Fatal("shared hosted controls settled execution")
	}
	adjudicationComplete(t, f, a)
	evidenceHTTPReceipt(t, f.request(http.MethodPost, path+"/finalization", f.writer, "hosted-finalize", `{"expected_revision":3}`), retained.ID, "finalized", 4)
	for i, index := range []int{0, 2} {
		fix := fmt.Sprintf(`{"expected_revision":%d,"finding":%d,"expected_sha":"","fix_sha":%q,"proof_note":"Focused hosted proof"}`, 4+i, index, strings.Repeat("f", 40))
		evidenceHTTPReceipt(t, f.request(http.MethodPost, path+"/fixes", f.writer, fmt.Sprintf("hosted-fix-%d", index), fix), retained.ID, "fix_recorded", int64(5+i))
	}
	after := cliPublicationDetail(t, f, 2, retained.ID)
	if !after.Run.Finalized || after.Run.Found != 3 || after.Run.Accepted != 2 || after.Run.Routed != 0 || after.Run.Fixed != 2 || len(after.Findings) != 3 {
		t.Fatal("shared controls did not project hosted frozen counts/fixes", after)
	}
	for i, finding := range after.Findings {
		if finding.SourceFindingID != detail.Findings[i].SourceFindingID || finding.Title != detail.Findings[i].Title || finding.Detail != detail.Findings[i].Detail {
			t.Fatal("hosted controls replaced frozen source identity")
		}
	}
	if current := f.request(http.MethodGet, path, f.reader, "", ""); current.Code != http.StatusOK || !bytes.Equal(current.Body.Bytes(), original) {
		t.Fatal("hosted controls changed immutable public capture")
	}
	hostedPublicationComparison(t, f, a)
	if trap.calls.Load() != 0 {
		t.Fatal("local hosted capture/publication/controls performed provider work")
	}
}

func TestHostedPublicationWrongBindingHasNoEffects(t *testing.T) {
	t.Run("wrong trigger before capture", func(t *testing.T) {
		f, a, in := hostedPublicationFixture(t, false)
		in.Hosted.TriggerCommentID++
		before, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
		_, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
		attemptCode(t, err, "review_conflict")
		if adjudicationState(t, f, a) != before || projectionDigest(t, f, "") != projection {
			t.Fatal("wrong trigger produced durable evidence/projection")
		}
	})
	t.Run("hosted metadata on CLI", func(t *testing.T) {
		f, a, in := evidenceFixture(t, "cli", true)
		h, _, parsed := hostedEvidenceMetadataFixture()
		h.ObservedBaseSHA = in.ExpectedAnchor.ParentSHA
		for i := range parsed.Findings {
			parsed.Findings[i].Title, parsed.Findings[i].Severity = "Trusted source", "medium"
		}
		in.Hosted, in.Review = h, parsed
		before := evidenceState(t, f, a)
		_, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
		attemptCode(t, err, "review_conflict")
		if evidenceState(t, f, a) != before {
			t.Fatal("wrong-channel capture produced durable effects")
		}
	})
}

func TestHostedPublicationZeroFindingsDoNotCreditCLI(t *testing.T) {
	f, a, in := hostedPublicationFixture(t, true)
	retained, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	pull := projectionPull(2)
	if _, err := f.e.PublishEvidence(t.Context(), attemptProject, retained.ID, &pull); err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, a)
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "hosted-zero-finalize", retained.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	adjudicationReceipt(t, body, err, retained.ID, "finalized", 1)
	detail := cliPublicationDetail(t, f, 2, retained.ID)
	if detail.Run.Channel != "hosted" || !detail.Run.Finalized || detail.Run.Found != 0 || detail.Run.Accepted != 0 || len(detail.Findings) != 0 {
		t.Fatal("zero hosted source was lost or fabricated findings", detail)
	}
	hostedPublicationComparison(t, f, a)
}
