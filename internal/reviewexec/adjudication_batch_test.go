package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

func decisionBatch(revision int64, findings ...int64) FindingDecisionBatchRequest {
	r := FindingDecisionBatchRequest{ExpectedRevision: adjudicationRevision(revision)}
	for _, finding := range findings {
		r.Items = append(r.Items, FindingDecisionBatchItem{Finding: adjudicationRevision(finding), Disposition: "accepted", Reason: "Explicit selected source decision"})
	}
	return r
}
func fixBatch(revision int64, findings ...int64) FindingFixBatchRequest {
	r := FindingFixBatchRequest{ExpectedRevision: adjudicationRevision(revision)}
	for _, finding := range findings {
		r.Items = append(r.Items, FindingFixBatchItem{Finding: adjudicationRevision(finding), ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("a", 40), ProofNote: "Exact focused proof"})
	}
	return r
}

func TestAdjudicationBatchAtomicRevisionReplayAndFrozenCounts(t *testing.T) {
	f, a, evidence := projectionFixture(t)
	run := projectionRun(evidence)
	initial := projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(evidence.Anchor.PullNumber), Run: &run})
	beforeSource, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), evidence.ID)
	if err != nil {
		t.Fatal(err)
	}
	writer := f.scope(t, attemptProject, f.writer, false)
	r := decisionBatch(0, 1, 0)
	body, err := f.e.DecideReviewFindings(t.Context(), writer, "batch-decisions", evidence.ID, r)
	first := adjudicationReceipt(t, body, err, evidence.ID, "adjudicated_batch", 1)
	if adjudicationRead(t, f, evidence.ID).Finalized {
		t.Fatal("batch implicitly finalized source")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_adjudication_changes`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.evidence.adjudicated_batch'`) != 1 {
		t.Fatal("batch produced per-item history/events")
	}
	var detail reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+runOpaqueForBatch(t, f, evidence.ID), &detail)
	if detail.Revision != initial.Revision+1 || detail.Findings[0].Disposition != "accepted" || detail.Findings[1].Disposition != "accepted" {
		t.Fatal("batch browser projection was not coherent", detail)
	}
	adjudicationComplete(t, f, a)
	body, err = f.e.FinalizeReviewEvidence(t.Context(), writer, "batch-finalize", evidence.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, evidence.ID, "finalized", 2)
	body, err = f.e.RecordFindingFixes(t.Context(), writer, "batch-fixes", evidence.ID, fixBatch(2, 0))
	adjudicationReceipt(t, body, err, evidence.ID, "fixes_recorded_batch", 3)
	replay, err := f.e.DecideReviewFindings(t.Context(), writer, "batch-decisions", evidence.ID, r)
	if err != nil || !bytes.Equal(first, replay) {
		t.Fatal("old-key batch replay changed", err)
	}
	r.Items[0].Reason = "Changed request"
	_, err = f.e.DecideReviewFindings(t.Context(), writer, "batch-decisions", evidence.ID, r)
	attemptCode(t, err, "idempotency_conflict")
	correction := decisionBatch(3, 0, 1)
	correction.Items[1].Disposition = "rejected"
	correction.Items[1].Reason = "Audited correction after finalization"
	body, err = f.e.DecideReviewFindings(t.Context(), writer, "batch-correction", evidence.ID, correction)
	adjudicationReceipt(t, body, err, evidence.ID, "adjudicated_batch", 4)
	state := adjudicationRead(t, f, evidence.ID)
	if !state.Finalized || *state.FrozenCounts != (FindingCounts{Found: 2, Accepted: 2}) || state.Findings[0].FixSHA != strings.Repeat("a", 40) || state.Findings[1].Disposition != "rejected" {
		t.Fatal("batch rewrote source counts or proof", state)
	}
	after, err := f.e.ReadReviewEvidence(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), evidence.ID)
	if err != nil || !reflect.DeepEqual(beforeSource, after) {
		t.Fatal("batch rewrote native capture", err)
	}
}

func TestAdjudicationBatchConcurrentCASAndCancellation(t *testing.T) {
	f, a, id := adjudicationFixture(t, 2, false)
	writer := f.scope(t, attemptProject, f.writer, false)
	before := adjudicationState(t, f, a)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.e.DecideReviewFindings(ctx, writer, "cancelled", id, decisionBatch(0, 0, 1)); err == nil || adjudicationState(t, f, a) != before {
		t.Fatal("cancelled batch changed state")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			<-start
			r := decisionBatch(0, 0, 1)
			if i == 1 {
				for j := range r.Items {
					r.Items[j].Disposition = "rejected"
				}
			}
			_, err := f.e.DecideReviewFindings(t.Context(), writer, fmt.Sprintf("batch-writer-%d", i), id, r)
			results <- err
		}(i)
	}
	close(start)
	winners := 0
	for i := 0; i < 2; i++ {
		if err := <-results; err == nil {
			winners++
		} else {
			attemptCode(t, err, "review_conflict")
		}
	}
	state := adjudicationRead(t, f, id)
	if winners != 1 || state.Revision != 1 || state.Findings[0].Disposition != state.Findings[1].Disposition || attemptCount(t, f.store, `SELECT count(*) FROM review_adjudication_changes`) != 1 {
		t.Fatal("concurrent batch CAS split selected actions", state)
	}
}

func runOpaqueForBatch(t *testing.T, f *attemptFixture, id string) string {
	t.Helper()
	var run string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_runs WHERE source_run_id=? LIMIT 1`, id).Scan(&run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestAdjudicationBatchBadMiddleCASAndProofGuardsRollback(t *testing.T) {
	f, a, id := adjudicationFixture(t, 3, false)
	writer := f.scope(t, attemptProject, f.writer, false)
	before := adjudicationState(t, f, a)
	_, err := f.e.DecideReviewFindings(t.Context(), writer, "bad-middle", id, decisionBatch(0, 0, 99, 1))
	attemptCode(t, err, "validation_failed")
	if adjudicationState(t, f, a) != before {
		t.Fatal("bad middle decision left durable effects")
	}
	body, err := f.e.DecideReviewFindings(t.Context(), writer, "good", id, decisionBatch(0, 0, 1, 2))
	adjudicationReceipt(t, body, err, id, "adjudicated_batch", 1)
	before = adjudicationState(t, f, a)
	_, err = f.e.DecideReviewFindings(t.Context(), writer, "stale", id, decisionBatch(0, 1, 2))
	attemptCode(t, err, "review_conflict")
	r := fixBatch(1, 0, 1, 2)
	r.Items[1].ExpectedSHA = adjudicationSHA(strings.Repeat("b", 40))
	_, err = f.e.RecordFindingFixes(t.Context(), writer, "bad-proof-middle", id, r)
	attemptCode(t, err, "review_conflict")
	if adjudicationState(t, f, a) != before {
		t.Fatal("stale/bad middle proof left a partial batch")
	}
	body, err = f.e.RecordFindingFixes(t.Context(), writer, "correct-proof", id, fixBatch(1, 0, 1, 2))
	adjudicationReceipt(t, body, err, id, "fixes_recorded_batch", 2)
	r = fixBatch(2, 0, 1)
	for i := range r.Items {
		r.Items[i].ExpectedSHA = adjudicationSHA(strings.Repeat("a", 40))
		r.Items[i].FixSHA = strings.Repeat("c", 40)
	}
	body, err = f.e.RecordFindingFixes(t.Context(), writer, "correct-proof-sha", id, r)
	adjudicationReceipt(t, body, err, id, "fixes_recorded_batch", 3)
	if adjudicationRead(t, f, id).Findings[2].FixSHA != strings.Repeat("a", 40) {
		t.Fatal("unselected proof changed")
	}
}

func TestAdjudicationBatchRouteEffectsRollbackAndReceivingAuthority(t *testing.T) {
	f, a, id := routeFixture(t, 2)
	writer := f.scope(t, attemptProject, f.writer, false)
	f.e.routeNow = func() time.Time { return time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC) }
	route := routeCreate(t, f, id, "batch-route", routeRequest(0, 0, 4))
	adjudicationDecide(t, f, id, "accept-other", 1, 1, "accepted")
	before := adjudicationState(t, f, a)
	priorRoute := routeRead(t, f, route.ID)
	_, err := f.e.RecordFindingFixes(t.Context(), writer, "no-receiver-authority", id, fixBatch(2, 1, 0))
	attemptCode(t, err, "review_conflict")
	if adjudicationState(t, f, a) != before {
		t.Fatal("source batch changed routed receiving proof")
	}
	r := decisionBatch(2, 0, 1)
	r.Items[0].ExpectedRouteRevision = route.Revision
	r.Items[1].ExpectedRouteRevision = 1
	_, err = f.e.DecideReviewFindings(t.Context(), writer, "route-middle-guard", id, r)
	attemptCode(t, err, "review_conflict")
	if adjudicationState(t, f, a) != before || !reflect.DeepEqual(priorRoute, routeRead(t, f, route.ID)) {
		t.Fatal("bad later guard left superseded route")
	}
	r.Items[1].ExpectedRouteRevision = 0
	body, err := f.e.DecideReviewFindings(t.Context(), writer, "route-convert", id, r)
	adjudicationReceipt(t, body, err, id, "adjudicated_batch", 3)
	if routeRead(t, f, route.ID).Route.State != "superseded" || adjudicationRead(t, f, id).Findings[0].RouteID != "" {
		t.Fatal("guarded route supersession missing")
	}
	var at string
	if err := f.store.DB().QueryRow(`SELECT created_at FROM review_adjudication_changes WHERE evidence_id=? AND revision=3`, id).Scan(&at); err != nil || at != routeRead(t, f, route.ID).Route.UpdatedAt {
		t.Fatal("batch history predates the retained route correction", err)
	}
}

func TestAdjudicationBatchProjectionAndEventFailureAreAtomic(t *testing.T) {
	for _, kind := range []string{"event", "projection"} {
		t.Run(kind, func(t *testing.T) {
			f, a, evidence := projectionFixture(t)
			run := projectionRun(evidence)
			projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(evidence.Anchor.PullNumber), Run: &run})
			original := f.e.publishAdjudication
			if kind == "event" {
				if _, err := f.store.DB().Exec(`CREATE TRIGGER fail_batch_event BEFORE INSERT ON events WHEN NEW.event_type='review.evidence.adjudicated_batch' BEGIN SELECT RAISE(ABORT,'private synthetic failure'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				f.e.publishAdjudication = func(ctx context.Context, tx *sql.Tx, scope core.Scope, state ReviewAdjudication) error {
					if err := original(ctx, tx, scope, state); err != nil {
						return err
					}
					return core.Fail("storage_unavailable", "Synthetic failure", 500)
				}
			}
			before, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
			_, err := f.e.DecideReviewFindings(t.Context(), f.scope(t, attemptProject, f.writer, false), "failed-batch", evidence.ID, decisionBatch(0, 0, 1))
			if err == nil || adjudicationState(t, f, a) != before || projectionDigest(t, f, "") != projection {
				t.Fatal("batch ledger/projection/audit/events/receipt did not roll back", err)
			}
		})
	}
}

func TestAdjudicationBatchHTTPExplicitItemsAuthorizationAndEscapedBound(t *testing.T) {
	f, a, id := adjudicationFixture(t, 100, false)
	path := "/api/v1/projects/" + attemptProject + "/reviews/evidence/" + id + "/decisions/batch"
	before := adjudicationState(t, f, a)
	for _, body := range []string{`null`, `{"expected_revision":0,"items":[]}`, `{"expected_revision":0,"items":[{"disposition":"accepted","reason":"explicit"}]}`, `{"expected_revision":0,"items":[{"finding":0,"disposition":"accepted","reason":"explicit","expected_route_revision":null}]}`, `{"expected_revision":0,"items":[{"finding":0,"finding":1,"disposition":"accepted","reason":"explicit"}]}`, `{"expected_revision":0,"items":[{"finding":0,"disposition":"accepted","reason":"explicit","extra":true}]}`, `{"expected_revision":0,"items":[{"finding":0,"disposition":"accepted","reason":"explicit"},{"finding":0,"disposition":"rejected","reason":"explicit"}]}`} {
		w := f.request(http.MethodPost, path, f.writer, "bad-wire", body)
		if w.Code != 400 {
			t.Fatalf("strict batch accepted %d %s", w.Code, w.Body.String())
		}
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("invalid wire changed source")
	}
	r := decisionBatch(0)
	for i := int64(0); i < 100; i++ {
		r.Items = append(r.Items, FindingDecisionBatchItem{Finding: adjudicationRevision(i), Disposition: "accepted", Reason: strings.Repeat("<", 2048)})
	}
	body, _ := json.Marshal(r)
	if len(body) <= 256<<10 {
		t.Fatal("fixture failed to exercise escaped body bound")
	}
	w := f.request(http.MethodPost, path, f.reader, "reader-batch", string(body))
	if w.Code != 403 {
		t.Fatal("reader changed batch", w.Code)
	}
	w = f.request(http.MethodPost, path, f.writer, "full-batch", string(body))
	if w.Code != 200 || adjudicationRead(t, f, id).Revision != 1 {
		t.Fatal("full escaped batch failed", w.Code, w.Body.String())
	}
	first := append([]byte(nil), w.Body.Bytes()...)
	w = f.request(http.MethodPost, path, f.writer, "full-batch", string(body))
	if w.Code != 200 || !bytes.Equal(first, w.Body.Bytes()) {
		t.Fatal("HTTP batch replay changed")
	}
	if _, err := f.store.DB().Exec(`DELETE FROM grants WHERE project_id=? AND credential_id='exec-writer-cred'`, attemptProject); err != nil {
		t.Fatal(err)
	}
	w = f.request(http.MethodPost, path, f.writer, "full-batch", string(body))
	if w.Code != 403 {
		t.Fatal("revoked writer replayed retained batch", w.Code)
	}
}
