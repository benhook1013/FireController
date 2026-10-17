package reviewexec

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func restoredHeld(t *testing.T, f *attemptFixture) {
	t.Helper()
	var credential string
	if err := f.store.DB().QueryRow(`SELECT id FROM credentials WHERE is_admin=1 LIMIT 1`).Scan(&credential); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"restore:state": "coordination_active", "restore:provider_state": "held", "restore:activation_credential_id": credential} {
		if _, err := f.store.DB().Exec(`INSERT INTO metadata(key,value) VALUES(?,?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
}
func restoredCall(t *testing.T, f *attemptFixture, at time.Time) (RestoredWorkSummary, error) {
	t.Helper()
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	summary, err := AbandonRestoredProviderWork(t.Context(), tx, f.store.InstanceID(), at)
	if err == nil {
		err = tx.Commit()
	}
	return summary, err
}
func restoredSQL(t *testing.T, f *attemptFixture, q string, args ...any) string {
	t.Helper()
	var s string
	if err := f.store.DB().QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRestoredProviderWorkAttemptsAndPostedPreparingQuery(t *testing.T) {
	for _, state := range []string{"preparing", "posting", "active", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			f, a, anchor, poster, provider := hostedQueryFixture(t)
			if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			// Real metadata query intent and confirmation precede review admission.
			query, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
			if err != nil || !created {
				t.Fatal(err)
			}
			if _, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, query.Revision, hostedQueryComment(t, query)); err != nil {
				t.Fatal(err)
			}
			savedQuery := restoredSQL(t, f, `SELECT body_json FROM review_hosted_allowance_queries WHERE attempt_id=?`, a.ID)
			if state != "preparing" {
				body, _ := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, body)
				if err != nil {
					t.Fatal(err)
				}
				if state != "posting" {
					a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, state, "test_transition", "991")
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			before := a
			original := attemptState(t, f, a)
			restoredHeld(t, f)
			summary, err := restoredCall(t, f, time.Now().Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if summary != (RestoredWorkSummary{Attempts: 1, PausedProjects: 1}) {
				t.Fatal(summary)
			}
			readTx, _ := f.store.DB().BeginTx(t.Context(), nil)
			got, err := readAttempt(t.Context(), readTx, attemptProject, a.ID)
			readTx.Rollback()
			if err != nil {
				t.Fatal(err)
			}
			extra := int64(1)
			if state == "posting" {
				extra = 2
			}
			if got.State != "cancelled" || got.Revision != before.Revision+extra || encoded(got.Anchor) != encoded(before.Anchor) || got.Intent != before.Intent || got.ExternalID != before.ExternalID {
				t.Fatalf("changed immutable attempt: %+v", got)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || restoredSQL(t, f, `SELECT body_json FROM review_hosted_allowance_queries WHERE attempt_id=?`, a.ID) != savedQuery {
				t.Fatal("reservation/query changed incorrectly")
			}
			var operation Operation
			tx, _ := f.store.DB().BeginTx(t.Context(), nil)
			operation, err = readOperation(t.Context(), tx, attemptProject, a.OperationID)
			if err == nil {
				_, err = attemptRecordingStamp(t.Context(), tx, operation, &got, time.Now())
			}
			tx.Rollback()
			if err != nil {
				t.Fatal("invalid effective chronology", err)
			}
			configTx, _ := f.store.DB().BeginTx(t.Context(), nil)
			config, rev, err := readConfiguration(t.Context(), configTx, attemptProject)
			configTx.Rollback()
			if err != nil || rev != 2 || !config.Paused || config.ExpectedRevision == nil || *config.ExpectedRevision != 1 {
				t.Fatal(config, rev, err)
			}
			var eventActor, eventPayload string
			if err := f.store.DB().QueryRow(`SELECT actor_id,payload_json FROM events WHERE event_type='reviews.configured' AND revision=2`).Scan(&eventActor, &eventPayload); err != nil {
				t.Fatal(err)
			}
			if eventActor != restoredSQL(t, f, `SELECT c.principal_id FROM credentials c JOIN metadata m ON m.value=c.id WHERE m.key='restore:activation_credential_id'`) || eventPayload != `{"paused":true,"revision":2}` {
				t.Fatal("configuration event lost operator attribution", eventActor, eventPayload)
			}
			eventCount := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			history := attemptState(t, f, got)
			again, err := restoredCall(t, f, time.Now())
			if err != nil || again != (RestoredWorkSummary{}) || attemptState(t, f, got) != history || attemptCount(t, f.store, `SELECT count(*) FROM events`) != eventCount {
				t.Fatal("not idempotent domain settlement", again, err, original)
			}
		})
	}
}

func TestRestoredProviderWorkSourceQueuePublicationAndRestart(t *testing.T) {
	f, terminal, evidence := checkpointFixture(t, 0)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	publication, receipt := checkpointRequest(t, f, evidence, "publication-original")
	original := checkpointRead(t, f, publication.ID)
	queued := f.enqueue(t, attemptProject, "unstarted-review", "hosted", 4, 1)
	source := refreshEnqueue(t, f, "source-original", refreshRequest())
	// Running source work has no result; emulate the canonical committed claim,
	// without executing a provider or inventing any observation.
	if _, err := f.store.DB().Exec(`UPDATE review_source_refreshes SET state='running',revision=revision+1 WHERE id=?`, source.ID); err != nil {
		t.Fatal(err)
	}
	preserved := restoredSQL(t, f, `SELECT body_json FROM review_execution_evidence WHERE id=?`, evidence)
	receipts := restoredSQL(t, f, `SELECT group_concat(response_json,'') FROM receipts`)
	restoredHeld(t, f)
	summary, err := restoredCall(t, f, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if summary != (RestoredWorkSummary{QueuedOperations: 1, SourceRefreshes: 1, Publications: 1, PausedProjects: 1}) {
		t.Fatal(summary)
	}
	if restoredSQL(t, f, `SELECT state||':'||reason FROM review_operations WHERE id=?`, queued.ID) != "held:restore_reconciled" {
		t.Fatal("queued review not held")
	}
	got := checkpointRead(t, f, publication.ID)
	if got.State != "cancelled" || got.Revision != 2 || got.Anchor != original.Anchor || got.Counts != original.Counts || got.CommentID != 0 {
		t.Fatal(got)
	}
	r := refreshRead(t, f, source.ID)
	if r.State != "held" || r.Reason != "restore_reconciled" || r.Revision != 3 || r.Observation != nil || r.CI != nil || r.ReviewState != nil {
		t.Fatal(r)
	}
	if restoredSQL(t, f, `SELECT body_json FROM review_execution_evidence WHERE id=?`, evidence) != preserved || restoredSQL(t, f, `SELECT group_concat(response_json,'') FROM receipts`) != receipts {
		t.Fatal("evidence/receipts rewritten")
	}
	if restoredSQL(t, f, `SELECT state FROM review_execution_attempts WHERE id=?`, terminal.ID) != "completed" {
		t.Fatal("terminal changed")
	}
	_, err = f.e.RecoverCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "no-recovery", publication.ID, CheckpointPublicationRecovery{ExpectedRevision: &got.Revision, RetryUnposted: true})
	attemptCode(t, err, "review_publication_held")
	replay, err := f.e.RequestCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "publication-original", CheckpointPublicationRequest{EvidenceID: evidence, ExpectedRevision: adjudicationRevision(1)})
	if err != nil || string(replay) != string(receipt) {
		t.Fatal("original immutable receipt changed", err)
	}
	fake := &checkpointFake{}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := f.e.StartCheckpointPublications(ctx, map[string]CheckpointProvider{"test-capability": fake})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if fake.posts != 0 || fake.reads != 0 || fake.actors != 0 || fake.repositories != 0 {
		t.Fatal("cancelled publication selected by restart")
	}
}

func TestRestoredProviderWorkRefusalAtomicAndAbsentDomains(t *testing.T) {
	t.Run("ordinary-and-absent", func(t *testing.T) {
		store, err := core.Open(t.Context(), filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		tx, _ := store.DB().BeginTx(t.Context(), nil)
		_, err = AbandonRestoredProviderWork(t.Context(), tx, store.InstanceID(), time.Now())
		tx.Rollback()
		attemptCode(t, err, "restore_reconciliation_invalid")
		for k, v := range map[string]string{"restore:state": "coordination_active", "restore:provider_state": "held"} {
			if _, err = store.DB().Exec(`INSERT INTO metadata VALUES(?,?)`, k, v); err != nil {
				t.Fatal(err)
			}
		}
		tx, _ = store.DB().BeginTx(t.Context(), nil)
		summary, err := AbandonRestoredProviderWork(t.Context(), tx, store.InstanceID(), time.Now())
		tx.Rollback()
		if err != nil || summary != (RestoredWorkSummary{}) {
			t.Fatal(summary, err)
		}
	})
	for _, kind := range []string{"reservation", "operation-link", "transition-external", "operation-time", "configuration", "old-schema", "unknown-operation", "unknown-attempt", "unknown-source", "anchor-json"} {
		t.Run(kind, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			first := f.enqueue(t, attemptProject, "queued-first", "hosted", 4, 1)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "preparing", "cli", 2, 1))
			restoredHeld(t, f)
			queries := map[string]string{"reservation": `UPDATE review_execution_reservations SET pull_number=4`, "operation-link": `UPDATE review_operations SET attempt_id='foreign' WHERE attempt_id<>''`, "transition-external": `UPDATE review_execution_transitions SET external_id='foreign'`, "operation-time": `UPDATE review_operations SET updated_at='invalid' WHERE attempt_id<>''`, "configuration": `UPDATE review_execution_config SET body_json='{}'`, "old-schema": `UPDATE metadata SET value='5' WHERE key='schema:review_execution'`, "unknown-operation": `UPDATE review_operations SET state='future_queued' WHERE attempt_id=''`, "unknown-attempt": `UPDATE review_execution_attempts SET state='future_active'`, "unknown-source": `INSERT INTO review_source_refreshes(id,project_id,repository_id,actor_id,worker_id,configuration_revision,capability_id,capability_fingerprint,pull_number,head_sha,state,revision,reason,created_at,updated_at,observation_json) SELECT 'review_refresh_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',project_id,801,actor_id,worker_id,configuration_revision,'test-capability',capability_fingerprint,pull_number,head_sha,'future_running',1,'',created_at,updated_at,'null' FROM review_operations LIMIT 1`, "anchor-json": `UPDATE review_execution_attempts SET anchor_json=' null '`}
			if _, err := f.store.DB().Exec(queries[kind]); err != nil {
				t.Fatal(err)
			}
			before := attemptState(t, f, a)
			queuedState := restoredSQL(t, f, `SELECT state FROM review_operations WHERE id=?`, first.ID)
			events := attemptCount(t, f.store, `SELECT count(*) FROM events`)
			_, err := restoredCall(t, f, time.Now())
			if err == nil {
				t.Fatal("corrupt state accepted")
			}
			if kind == "old-schema" {
				attemptCode(t, err, "restore_reconciliation_schema")
			}
			if attemptState(t, f, a) != before || attemptCount(t, f.store, `SELECT count(*) FROM events`) != events || restoredSQL(t, f, `SELECT state FROM review_operations WHERE id=?`, first.ID) != queuedState || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_config`) != 1 {
				t.Fatal("transaction failed to roll back")
			}
		})
	}
}

func TestRestoredProviderWorkCapacityRollback(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	restoredHeld(t, f)
	// Canonical queued rows use one genuine request as a template. No provider
	// evidence or completion is synthesized by this capacity fixture.
	original := f.enqueue(t, attemptProject, "capacity-template", "cli", 2, 1)
	_, err := f.store.DB().Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<10000) INSERT INTO review_operations SELECT 'overflow-'||x,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision FROM review_operations,n WHERE id=?`, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = restoredCall(t, f, time.Now())
	attemptCode(t, err, "restore_reconciliation_capacity")
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE state='queued'`) != 10001 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_config`) != 1 {
		t.Fatal("partial capacity settlement")
	}
}

func TestRestoredProviderWorkPausedEventUsesRevokedActivationPrincipal(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	restoredHeld(t, f)
	principal := restoredSQL(t, f, `SELECT principal_id FROM credentials WHERE id=(SELECT value FROM metadata WHERE key='restore:activation_credential_id')`)
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id=(SELECT value FROM metadata WHERE key='restore:activation_credential_id')`); err != nil {
		t.Fatal(err)
	}
	counts, err := restoredCall(t, f, time.Now())
	if err != nil || counts.PausedProjects != 1 {
		t.Fatal(counts, err)
	}
	if restoredSQL(t, f, `SELECT actor_id FROM events WHERE event_type='reviews.configured' AND revision=2`) != principal || attemptCount(t, f.store, `SELECT count(*) FROM credentials WHERE principal_id=? AND revoked=0`, principal) != 0 {
		t.Fatal("local attribution revived access")
	}
}

func TestRestoredProviderWorkSourceOptionIdentityPreserved(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	ci := refreshRequest()
	ci.IncludeCI = true
	readiness := refreshRequest()
	readiness.IncludeReadiness = true
	a := refreshEnqueue(t, f, "ci", ci)
	b := refreshEnqueue(t, f, "readiness", readiness)
	if _, err := f.store.DB().Exec(`UPDATE review_source_refreshes SET state='running',revision=2 WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	restoredHeld(t, f)
	counts, err := restoredCall(t, f, time.Now().Add(-time.Hour))
	if err != nil || counts.SourceRefreshes != 2 {
		t.Fatal(counts, err)
	}
	for _, id := range []string{a.ID, b.ID} {
		v := refreshRead(t, f, id)
		if v.State != "held" || v.Observation != nil || v.CI != nil || v.ReviewState != nil {
			t.Fatal(v)
		}
		if id == a.ID && (!v.IncludeCI || v.IncludeReadiness) || id == b.ID && (v.IncludeCI || !v.IncludeReadiness) {
			t.Fatal("source mode changed", v)
		}
	}
}
