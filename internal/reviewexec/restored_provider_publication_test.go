package reviewexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestRestoredProviderWorkIssuedPublicationAndUnpostedRefusal(t *testing.T) {
	for _, issued := range []bool{false, true} {
		t.Run(map[bool]string{false: "known-unposted", true: "possibly-published"}[issued], func(t *testing.T) {
			f, _, evidence := checkpointFixture(t, 0)
			r, _ := checkpointRequest(t, f, evidence, "publication")
			p := &checkpointFake{refuse: !issued}
			if issued {
				p.postError = errors.New("private lost provider response")
			}
			cancel, done := checkpointStart(t, f.e, p)
			before := checkpointTerminal(t, f, r.ID)
			checkpointStop(t, cancel, done)
			if issued && before.State != "uncertain" || !issued && before.State != "held" {
				t.Fatal(before)
			}
			intent := restoredSQL(t, f, `SELECT group_concat(body_json,'') FROM review_checkpoint_intents`)
			tx, _ := f.store.DB().BeginTx(t.Context(), nil)
			original, err := readCheckpoint(t.Context(), tx, attemptProject, r.ID)
			tx.Rollback()
			if err != nil {
				t.Fatal(err)
			}
			restoredHeld(t, f)
			counts, err := restoredCall(t, f, time.Now().Add(-time.Hour))
			if err != nil || counts.Publications != 1 {
				t.Fatal(counts, err)
			}
			tx, _ = f.store.DB().BeginTx(t.Context(), nil)
			after, err := readCheckpoint(t.Context(), tx, attemptProject, r.ID)
			tx.Rollback()
			if err != nil {
				t.Fatal(err)
			}
			if after.View.State != "cancelled" || after.Unposted != original.Unposted || encoded(after.Intent) != encoded(original.Intent) || encoded(after.Payload) != encoded(original.Payload) || after.View.Revision != before.Revision+1 || restoredSQL(t, f, `SELECT group_concat(body_json,'') FROM review_checkpoint_intents`) != intent {
				t.Fatal("original posting distinction/intent lost")
			}
			posts, reads := p.counts()
			for _, retry := range []bool{false, true} {
				_, err = f.e.RecoverCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), map[bool]string{false: "observe", true: "retry"}[retry], r.ID, CheckpointPublicationRecovery{ExpectedRevision: &after.View.Revision, RetryUnposted: retry})
				attemptCode(t, err, "review_publication_held")
			}
			restarted, err := New(f.store, attemptCapabilities)
			if err != nil {
				t.Fatal(err)
			}
			cancel, done = checkpointStart(t, restarted, p)
			checkpointStop(t, cancel, done)
			newPosts, newReads := p.counts()
			if newPosts != posts || newReads != reads {
				t.Fatal("abandoned intent was reused")
			}
		})
	}
}

func TestRestoredProviderWorkNativeProjectionAndWholeTransactionRollback(t *testing.T) {
	t.Run("projection", func(t *testing.T) {
		f := attemptProjectionFixture(t, true)
		a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "projected", "cli", 2, 1))
		if _, err := f.e.Admit(t.Context(), attemptProject, a.ID, 1, attemptAnchor(attemptProject, 2), ""); err != nil {
			t.Fatal(err)
		}
		restoredHeld(t, f)
		counts, err := restoredCall(t, f, time.Now().Add(-time.Hour))
		if err != nil || counts.Attempts != 1 {
			t.Fatal(counts, err)
		}
		_, projected := projectedAttempt(t, f, a)
		if projected.State != "cancelled" || projected.FinishedAt == "" || projected.SourceRunID != "" {
			t.Fatal(projected)
		}
		tx, _ := f.store.DB().BeginTx(t.Context(), nil)
		current, err := readAttempt(t.Context(), tx, attemptProject, a.ID)
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		assertProjectedTimes(t, f, current, projected, true)
	})
	t.Run("late-publication-corruption-rolls-back-all", func(t *testing.T) {
		f, _, evidence := checkpointFixture(t, 0)
		if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
			t.Fatal(err)
		}
		r, _ := checkpointRequest(t, f, evidence, "will-corrupt")
		queued := f.enqueue(t, attemptProject, "queued", "hosted", 4, 1)
		source := refreshEnqueue(t, f, "queued-source", refreshRequest())
		a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "preparing", "cli", 4, 1))
		restoredHeld(t, f)
		if _, err := f.store.DB().Exec(`UPDATE review_checkpoint_revisions SET body_json='{}' WHERE publication_id=?`, r.ID); err != nil {
			t.Fatal(err)
		}
		before := attemptState(t, f, a)
		events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
		_, err := restoredCall(t, f, time.Now())
		if err == nil {
			t.Fatal("corrupt history accepted")
		}
		if attemptState(t, f, a) != before || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events || restoredSQL(t, f, `SELECT state FROM review_operations WHERE id=?`, queued.ID) != "queued" || refreshRead(t, f, source.ID).State != "queued" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_config`) != 1 {
			t.Fatal("late failure partially reconciled")
		}
	})
}

func TestRestoredProviderWorkCancelledContextAndCurrentShape(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	restoredHeld(t, f)
	f.enqueue(t, attemptProject, "queued", "cli", 2, 1)
	for _, kind := range []string{"cancelled", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			if kind == "deadline" {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			}
			cancel()
			before := attemptClockRows(t, f)
			metadata := restoredSQL(t, f, `SELECT json_group_array(json_array(key,value)) FROM (SELECT key,value FROM metadata ORDER BY key)`)
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			// The caller owns rollback, including its own earlier local writes.
			if _, err = tx.Exec(`INSERT INTO metadata(key,value) VALUES('restore:test_pending','not_committed')`); err != nil {
				t.Fatal(err)
			}
			summary, err := AbandonRestoredProviderWork(ctx, tx, f.store.InstanceID(), time.Now())
			attemptCode(t, err, "restore_reconciliation_interrupted")
			var failure *core.Error
			if !errors.As(err, &failure) || failure.Status != 503 || summary != (RestoredWorkSummary{}) || !strings.Contains(failure.Message, "Retry the exact") {
				t.Fatal("interruption lost safe status, empty counts or exact retry guidance", summary, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if attemptClockRows(t, f) != before || restoredSQL(t, f, `SELECT json_group_array(json_array(key,value)) FROM (SELECT key,value FROM metadata ORDER BY key)`) != metadata || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE state='queued'`) != 1 {
				t.Fatal("interrupted reconciliation escaped caller rollback")
			}
		})
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AbandonRestoredProviderWork(nil, tx, f.store.InstanceID(), time.Now())
	attemptCode(t, err, "restore_reconciliation_invalid")
	tx.Rollback()
	summary, err := restoredCall(t, f, time.Now())
	if err != nil || summary != (RestoredWorkSummary{QueuedOperations: 1, PausedProjects: 1}) || restoredSQL(t, f, `SELECT value FROM metadata WHERE key='restore:provider_state'`) != "held" {
		t.Fatal("uninterrupted exact reconciliation retry failed or released providers", summary, err)
	}
	if _, err = f.store.DB().Exec(`DROP TABLE review_hosted_allowance_queries`); err != nil {
		t.Fatal(err)
	}
	_, err = restoredCall(t, f, time.Now())
	attemptCode(t, err, "restore_reconciliation_schema")
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Status != 409 {
		t.Fatal("actual schema failure lost conflict status", err)
	}
}

func TestRestoredProviderWorkPublishedCheckpointPreserved(t *testing.T) {
	f, _, evidence := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, evidence, "published-original")
	provider := &checkpointFake{}
	cancel, done := checkpointStart(t, f.e, provider)
	published := checkpointTerminal(t, f, r.ID)
	checkpointStop(t, cancel, done)
	if published.State != "published" {
		t.Fatal(published)
	}
	original := restoredSQL(t, f, `SELECT body_json FROM review_checkpoint_publications WHERE id=?`, r.ID)
	events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
	restoredHeld(t, f)
	counts, err := restoredCall(t, f, time.Now())
	if err != nil || counts != (RestoredWorkSummary{PausedProjects: 1}) || restoredSQL(t, f, `SELECT body_json FROM review_checkpoint_publications WHERE id=?`, r.ID) != original || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events+1 || encoded(checkpointRead(t, f, r.ID)) != encoded(published) {
		t.Fatal("published history changed", counts, err)
	}
}

func TestRestoredProviderWorkSourceRollbackAndCheckpointStrictTiming(t *testing.T) {
	t.Run("source-keeps-valid-rollback", func(t *testing.T) {
		f := newAttemptFixture(t)
		f.configure(t, attemptProject, "test-capability", 0, false)
		source := refreshEnqueue(t, f, "source", refreshRequest())
		created := time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z")
		updated := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z")
		if _, err := f.store.DB().Exec(`UPDATE review_source_refreshes SET state='running',revision=2,created_at=?,updated_at=? WHERE id=?`, created, updated, source.ID); err != nil {
			t.Fatal(err)
		}
		before := refreshRead(t, f, source.ID)
		restoredHeld(t, f)
		counts, err := restoredCall(t, f, time.Now())
		if err != nil || counts.SourceRefreshes != 1 {
			t.Fatal(counts, err)
		}
		after := refreshRead(t, f, source.ID)
		if after.State != "held" || after.CreatedAt != before.CreatedAt || after.UpdatedAt != created {
			t.Fatal("source chronology invented or rewritten", after)
		}
	})
	t.Run("checkpoint-retains-strict-decoder", func(t *testing.T) {
		f, _, evidence := checkpointFixture(t, 0)
		publication, _ := checkpointRequest(t, f, evidence, "publication")
		tx, err := f.store.DB().BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := readCheckpoint(t.Context(), tx, attemptProject, publication.ID)
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		old, _ := checkpointTime(r.View.CreatedAt)
		r.View.UpdatedAt = old.Add(-time.Second).Format("2006-01-02T15:04:05.000000000Z")
		// Canonical body/index/history agree, but unsupported reversed checkpoint
		// chronology must remain a refusal rather than being silently repaired.
		if _, err = f.store.DB().Exec(`UPDATE review_checkpoint_publications SET body_json=? WHERE id=?`, encoded(r), publication.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.DB().Exec(`UPDATE review_checkpoint_revisions SET body_json=? WHERE publication_id=?`, encoded(r), publication.ID); err != nil {
			t.Fatal(err)
		}
		restoredHeld(t, f)
		_, err = restoredCall(t, f, time.Now())
		if err == nil {
			t.Fatal("invalid checkpoint chronology accepted")
		}
		if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_config`) != 1 {
			t.Fatal("partial configuration pause")
		}
	})
}
