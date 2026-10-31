package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
)

func automaticCheckpoint(t *testing.T, f *attemptFixture, evidence string) CheckpointPublication {
	t.Helper()
	var id string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_checkpoint_publications WHERE evidence_id=?`, evidence).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return checkpointRead(t, f, id)
}

func TestCheckpointAutomaticFinalizationBeforeFixAndOneDelivery(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 1, false)
	adjudicationComplete(t, f, a)
	adjudicationDecide(t, f, id, "automatic-accept", 0, 0, "accepted")
	scope := f.scope(t, attemptProject, f.writer, false)
	request := FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications`); n != 0 {
		t.Fatal("posted before adjudication", n)
	}
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), scope, "automatic-finalize", id, request)
	if err != nil {
		t.Fatal(err)
	}
	v := automaticCheckpoint(t, f, id)
	if v.State != "queued" || v.Counts != (FindingCounts{Found: 1, Accepted: 1}) || v.SourceRevision != 2 || adjudicationRead(t, f, id).Findings[0].FixSHA != "" {
		t.Fatal("delivery requires fixes", v)
	}
	var receipt Receipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.ID != id || receipt.State != "finalized" || receipt.Revision != 2 {
		t.Fatal("changed compact finalization receipt", string(raw))
	}
	before := checkpointSQLState(t, f)
	replay, err := f.e.FinalizeReviewEvidence(t.Context(), scope, "automatic-finalize", id, request)
	if err != nil || !bytes.Equal(raw, replay) || checkpointSQLState(t, f) != before {
		t.Fatal("replay changed queued delivery", err)
	}
	// No explicit publish call. A restarted worker discovers the committed row.
	restarted, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	p := &checkpointFake{}
	cancel, done := checkpointStart(t, restarted, p)
	got, terminal, err := restarted.WaitCheckpointPublication(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), v.ID, 3)
	checkpointStop(t, cancel, done)
	if err != nil || !terminal || got.State != "published" {
		t.Fatal(got, err)
	}
	posts, _ := p.counts()
	if posts != 1 {
		t.Fatal("automatic delivery duplicated", posts)
	}
	coverageFix(t, f, id, "automatic-later-fix", 2, 0, "")
	later, _ := checkpointRequest(t, f, id, "old-agent-extra-publish")
	if later.ID != v.ID || attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications`) != 1 {
		t.Fatal("old agent created another comment")
	}
}

func TestCheckpointAutomaticHostedZeroAndRollback(t *testing.T) {
	f, a, in := hostedPublicationFixture(t, true)
	captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, a)
	scope := f.scope(t, attemptProject, f.writer, false)
	request := FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)}
	admissionSQL(t, f, `CREATE TRIGGER automatic_queue_failure BEFORE INSERT ON review_checkpoint_publications BEGIN SELECT RAISE(ABORT,'refuse delivery'); END`)
	before := checkpointSQLState(t, f)
	_, err = f.e.FinalizeReviewEvidence(t.Context(), scope, "automatic-hosted-finalize", captured.ID, request)
	if err == nil || adjudicationRead(t, f, captured.ID).Finalized || checkpointSQLState(t, f) != before {
		t.Fatal("delivery failure left finalized work without outbox", err)
	}
	admissionSQL(t, f, `DROP TRIGGER automatic_queue_failure`)
	_, err = f.e.FinalizeReviewEvidence(t.Context(), scope, "automatic-hosted-finalize", captured.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	v := automaticCheckpoint(t, f, captured.ID)
	p := &checkpointFake{}
	cancel, done := checkpointStart(t, f.e, p)
	got := checkpointTerminal(t, f, v.ID)
	checkpointStop(t, cancel, done)
	if got.State != "published" || got.Counts != (FindingCounts{}) {
		t.Fatal(got)
	}
}

func TestCheckpointAutomaticBacklogCannotBlockFinalization(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 18, 30, "automatic-backlog-grant")
	for i := 0; i < 18; i++ {
		a, id := roundCapture(t, f, 900+i, false)
		roundFinalize(t, f, a, id, 900+i, false)
		if automaticCheckpoint(t, f, id).State != "queued" {
			t.Fatal("backlog dropped")
		}
	}
	if n := attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications`); n != 18 {
		t.Fatal(n)
	}
}

func TestCheckpointAutomaticMissingBindingRetainsFinalizationAndRecovery(t *testing.T) {
	f, a, id, _ := coverageFixture(t, 0, false)
	adjudicationComplete(t, f, a)
	f.e.capabilities = map[string]Capability{}
	_, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "automatic-missing-binding", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
	if err != nil {
		t.Fatal("provider configuration blocked adjudication", err)
	}
	v := automaticCheckpoint(t, f, id)
	if !adjudicationRead(t, f, id).Finalized || v.State != "held" || v.Reason != "capability_unavailable" {
		t.Fatal(v)
	}
	var record checkpointRecord
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		record, err = readCheckpoint(ctx, tx, attemptProject, v.ID)
		return err
	})
	if err != nil || !record.Unposted || record.Intent != nil {
		t.Fatal("unposted proof lost", err)
	}
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	checkpointRecover(t, f, v, "automatic-binding-restored", true)
	p := &checkpointFake{}
	cancel, done := checkpointStart(t, f.e, p)
	got := checkpointTerminal(t, f, v.ID)
	checkpointStop(t, cancel, done)
	if got.State != "published" {
		t.Fatal(got)
	}
}

func TestCheckpointAutomaticNoQueueForRejectedFinalization(t *testing.T) {
	for _, mode := range []string{"unadjudicated", "reader", "foreign", "stale"} {
		t.Run(mode, func(t *testing.T) {
			f, _, id, _ := coverageFixture(t, 1, false)
			scope := f.scope(t, attemptProject, f.writer, false)
			revision := int64(0)
			if mode == "reader" {
				scope = evidenceReadScope(t, f, attemptProject, f.reader)
			}
			if mode == "foreign" {
				scope = f.scope(t, otherAttemptProject, f.other, false)
			}
			if mode == "stale" {
				revision = 1
			}
			_, err := f.e.FinalizeReviewEvidence(t.Context(), scope, fmt.Sprintf("automatic-invalid-%s", mode), id, FinalizeReviewRequest{ExpectedRevision: &revision})
			if err == nil || attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications`) != 0 {
				t.Fatal("invalid finalization queued a comment", err)
			}
		})
	}
}
