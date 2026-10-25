package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"strings"
	"sync"
	"testing"
	"time"
)

func checkpointFixture(t *testing.T, count int) (*attemptFixture, Attempt, string) {
	t.Helper()
	f, a, id, _ := coverageFixture(t, count, false)
	for i := 0; i < count; i++ {
		adjudicationDecide(t, f, id, fmt.Sprintf("checkpoint-decision-%d", i), int64(i), int64(i), "accepted")
	}
	coverageFinalize(t, f, a, id, int64(count))
	// These explicit-publication tests exercise previously finalized history
	// without a delivery journal. Current finalization queues it automatically.
	admissionSQL(t, f, `DELETE FROM review_checkpoint_revisions WHERE publication_id IN (SELECT id FROM review_checkpoint_publications WHERE evidence_id=?)`, id)
	admissionSQL(t, f, `DELETE FROM review_checkpoint_publications WHERE evidence_id=?`, id)
	return f, a, id
}
func checkpointRequest(t *testing.T, f *attemptFixture, id, key string) (Receipt, []byte) {
	t.Helper()
	state := adjudicationRead(t, f, id)
	raw, err := f.e.RequestCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), key, CheckpointPublicationRequest{EvidenceID: id, ExpectedRevision: &state.Revision})
	if err != nil {
		t.Fatal(err)
	}
	var r Receipt
	if json.Unmarshal(raw, &r) != nil || r.State != "queued" || !validPublicationID(r.ID) || r.Revision < 1 {
		t.Fatal(string(raw))
	}
	return r, raw
}
func checkpointRead(t *testing.T, f *attemptFixture, id string) CheckpointPublication {
	t.Helper()
	v, err := f.e.ReadCheckpointPublication(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func checkpointSQLState(t *testing.T, f *attemptFixture) string {
	t.Helper()
	var out string
	for _, q := range []string{`SELECT coalesce(group_concat(body_json,''),'') FROM review_checkpoint_publications`, `SELECT coalesce(group_concat(body_json,''),'') FROM review_checkpoint_revisions`, `SELECT coalesce(group_concat(body_json,''),'') FROM review_checkpoint_intents`, `SELECT count(*) FROM receipts`, `SELECT count(*) FROM events`} {
		var s string
		if err := f.store.DB().QueryRow(q).Scan(&s); err != nil {
			t.Fatal(err)
		}
		out += s + "/"
	}
	return out
}
func TestCheckpointPublicationSelectionReplayAndFrozenSemantics(t *testing.T) {
	f, _, id := checkpointFixture(t, 1)
	source := encoded(adjudicationRead(t, f, id))
	r, original := checkpointRequest(t, f, id, "publish")
	v := checkpointRead(t, f, r.ID)
	if v.State != "queued" || v.SourceRevision != 2 || v.Counts != (FindingCounts{Found: 1, Accepted: 1}) || v.CommentID != 0 || v.ProviderAuthorID != 0 || len(encoded(v))+1 > 16<<10 {
		t.Fatal(v)
	}
	// Accepted-but-unfixed is valid historical publication, not current coverage.
	before := checkpointSQLState(t, f)
	scope := f.scope(t, attemptProject, f.writer, false)
	raw, err := f.e.RequestCheckpointPublication(t.Context(), scope, "publish", CheckpointPublicationRequest{EvidenceID: id, ExpectedRevision: adjudicationRevision(2)})
	if err != nil || !bytes.Equal(raw, original) || checkpointSQLState(t, f) != before {
		t.Fatal("exact replay changed state", err)
	}
	if encoded(adjudicationRead(t, f, id)) != source {
		t.Fatal("publication rewrote source adjudication")
	}

	coverageFix(t, f, id, "later-fix", 2, 0, "")
	r2, _ := checkpointRequest(t, f, id, "different-key")
	if r2.ID != r.ID || r2.Revision != r.Revision || checkpointRead(t, f, r.ID).SourceRevision != 2 {
		t.Fatal("correction made another publication")
	}
	_, err = f.e.RequestCheckpointPublication(t.Context(), scope, "stale", CheckpointPublicationRequest{EvidenceID: id, ExpectedRevision: adjudicationRevision(2)})
	attemptCode(t, err, "review_conflict")
	if attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications`) != 1 {
		t.Fatal("duplicate semantic publication")
	}
	for _, forbidden := range []string{"Explicit focused proof", "Accepted with explicit rationale", f.writer, "exec-writer-principal", "capture_sha256"} {
		if strings.Contains(encoded(v), forbidden) {
			t.Fatal("public private-field leak", forbidden)
		}
	}
}
func TestCheckpointPublicationAuthIncompleteAndAtomicFailure(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	reader := evidenceReadScope(t, f, attemptProject, f.reader)
	_, err := f.e.RequestCheckpointPublication(t.Context(), reader, "read-cannot-write", CheckpointPublicationRequest{EvidenceID: id, ExpectedRevision: adjudicationRevision(1)})
	attemptCode(t, err, "forbidden")
	other := f.scope(t, otherAttemptProject, f.other, false)
	_, err = f.e.RequestCheckpointPublication(t.Context(), other, "cross-project", CheckpointPublicationRequest{EvidenceID: id, ExpectedRevision: adjudicationRevision(1)})
	attemptCode(t, err, "not_found")
	before := checkpointSQLState(t, f)
	admissionSQL(t, f, `CREATE TRIGGER checkpoint_fail_event BEFORE INSERT ON events WHEN NEW.event_type='reviews.checkpoint_publication' BEGIN SELECT RAISE(ABORT,'synthetic event refusal'); END`)
	_, err = f.e.RequestCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "rollback", CheckpointPublicationRequest{EvidenceID: id, ExpectedRevision: adjudicationRevision(1)})
	if err == nil || checkpointSQLState(t, f) != before {
		t.Fatal("publication/event/receipt not atomic", err)
	}
	g, _, partial, _ := coverageFixture(t, 0, true)
	_, err = g.e.RequestCheckpointPublication(t.Context(), g.scope(t, attemptProject, g.writer, false), "partial", CheckpointPublicationRequest{EvidenceID: partial, ExpectedRevision: adjudicationRevision(0)})
	attemptCode(t, err, "review_publication_held")
}
func TestCheckpointPublicationConcurrentKeysOneSemanticRow(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	scope := f.scope(t, attemptProject, f.writer, false)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			raw, err := f.e.RequestCheckpointPublication(t.Context(), scope, fmt.Sprintf("concurrent-%d", i), CheckpointPublicationRequest{EvidenceID: id, ExpectedRevision: adjudicationRevision(1)})
			if err == nil {
				var r Receipt
				err = json.Unmarshal(raw, &r)
				ids <- r.ID
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first string
	for id := range ids {
		if first != "" && id != first {
			t.Fatal("duplicate publication")
		}
		first = id
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_revisions`) != 1 {
		t.Fatal("coalesced actions changed journal")
	}
}
func TestCheckpointPublicationStrictRetainedBindingAndReadOnly(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "read-proof")
	before := checkpointSQLState(t, f)
	for i := 0; i < 3; i++ {
		checkpointRead(t, f, r.ID)
	}
	if checkpointSQLState(t, f) != before {
		t.Fatal("read mutated")
	}
	_, err := f.e.ReadCheckpointPublication(t.Context(), f.scope(t, otherAttemptProject, f.other, false), r.ID)
	attemptCode(t, err, "not_found")
	admissionSQL(t, f, `UPDATE review_checkpoint_publications SET body_json=replace(body_json,'"found":0','"found":1') WHERE id=?`, r.ID)
	_, err = f.e.ReadCheckpointPublication(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID)
	attemptCode(t, err, "storage_unavailable")
}
func TestCheckpointPublicationWaitCancellationAndAuthorization(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "wait")
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	for _, seconds := range []int{0, 1} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, terminal, err := f.e.WaitCheckpointPublication(ctx, scope, r.ID, seconds)
		attemptCode(t, err, "overloaded")
		if terminal {
			t.Fatal("canceled wait reported a terminal result")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { _, _, err := f.e.WaitCheckpointPublication(ctx, scope, r.ID, 60); result <- err }()
	deadline := time.Now().Add(time.Second)
	for {
		f.e.checkpointMu.Lock()
		n := f.e.checkpointWaiterCount
		f.e.checkpointMu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wait not registered")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		attemptCode(t, err, "overloaded")
	case <-time.After(time.Second):
		t.Fatal("cancellation unbounded")
	}
	f.e.checkpointMu.Lock()
	n := f.e.checkpointWaiterCount
	f.e.checkpointMu.Unlock()
	if n != 0 {
		t.Fatal("wait leaked")
	}
	// Register-before-read cannot miss a completion and current revocation blocks output.
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
	_, _, err := f.e.WaitCheckpointPublication(t.Context(), scope, r.ID, 0)
	attemptCode(t, err, "unauthorized")
}

func TestCheckpointPublicationRoutedReceiverProofCannotRewritePayload(t *testing.T) {
	f, a, id := routeFixture(t, 1)
	route := routeCreate(t, f, id, "route-source", routeRequest(0, 0, 4))
	adjudicationComplete(t, f, a)
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "route-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	if err != nil {
		t.Fatal(string(raw), err)
	}
	r, _ := checkpointRequest(t, f, id, "routed-publication")
	original := adjudicationRead(t, f, id)
	routeDecision(t, f, route.ID, "receiver-accept", 1, "accepted")
	routeFix(t, f, route.ID, "receiver-fix", 2, "", strings.Repeat("f", 40))
	if encoded(adjudicationRead(t, f, id)) != encoded(original) {
		t.Fatal("receiver rewrote source")
	}
	view := checkpointRead(t, f, r.ID)
	if view.Counts != (FindingCounts{Found: 1, Routed: 1}) || strings.Contains(encoded(view), strings.Repeat("f", 40)) {
		t.Fatal("receiver proof changed public counts", view)
	}
	// A new caller key reconciles the same immutable semantic resource.
	second, _ := checkpointRequest(t, f, id, "routed-after-fix")
	if second.ID != r.ID || attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_revisions`) != 1 {
		t.Fatal("receiving proof generated another public action")
	}
}
func TestCheckpointPublicationHostedUnknownFilesAndFinalizedZero(t *testing.T) {
	f, a, in := hostedPublicationFixture(t, true)
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, a)
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "hosted-checkpoint-finalize", receipt.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	if err != nil {
		t.Fatal(string(raw), err)
	}
	r, _ := checkpointRequest(t, f, receipt.ID, "hosted-checkpoint")
	var payload reviewprovider.CheckpointPayload
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		record, err := readCheckpoint(ctx, tx, attemptProject, r.ID)
		payload = record.Payload
		return err
	})
	if err != nil || payload.Channel != "hosted" || payload.Files != nil || payload.DurationSeconds == nil || payload.Found != 0 {
		t.Fatal(payload, err)
	}
}

func TestCheckpointPublicationCapacityAndWaitSubscriptionBounds(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	// Fill active capacity using independent native captures and real requests.
	for i := 0; i < 16; i++ {
		operation := f.enqueue(t, attemptProject, fmt.Sprintf("capacity-op-%d", i), "cli", 2, 1)
		a := f.start(t, attemptProject, operation)
		anchor := attemptAnchor(attemptProject, 2)
		a2, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
		if err != nil {
			t.Fatal(err)
		}
		digest := fmt.Sprintf("%064x", i+100)
		input := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1, StartedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReviewedFiles: []string{"changed.go"}, Findings: []reviewprovider.ParsedFinding{}}}
		capture, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a2.ID, input)
		if err != nil {
			t.Fatal(err)
		}
		adjudicationComplete(t, f, a2)
		_, err = f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), fmt.Sprintf("capacity-finalize-%d", i), capture.ID, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
		if err != nil {
			t.Fatal(err)
		}
		checkpointRequest(t, f, capture.ID, fmt.Sprintf("capacity-pub-%d", i))
	}
	before := checkpointSQLState(t, f)
	_, err := f.e.RequestCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "capacity-overflow", CheckpointPublicationRequest{EvidenceID: id, ExpectedRevision: adjudicationRevision(1)})
	attemptCode(t, err, "overloaded")
	if checkpointSQLState(t, f) != before {
		t.Fatal("capacity refusal changed journal")
	}
	var pubID string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_checkpoint_publications LIMIT 1`).Scan(&pubID); err != nil {
		t.Fatal(err)
	}
	f.e.checkpointMu.Lock()
	f.e.checkpointWaiterPrincipals["exec-reader-principal"] = 8
	f.e.checkpointWaiterCount = 8
	f.e.checkpointMu.Unlock()
	_, _, err = f.e.WaitCheckpointPublication(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), pubID, 1)
	attemptCode(t, err, "overloaded")
	f.e.checkpointMu.Lock()
	f.e.checkpointWaiterPrincipals = map[string]int{}
	f.e.checkpointWaiterCount = 0
	f.e.checkpointMu.Unlock()
}
