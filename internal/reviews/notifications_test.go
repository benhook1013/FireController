package reviews

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
)

func noticeFixture(t *testing.T, state string) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	if err := inbox.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	for _, worker := range []string{"participant", "muted", "retired", "uninvolved"} {
		if _, err := f.store.DB().Exec(`INSERT INTO workers VALUES(?,?,?)`, f.project, worker, worker); err != nil {
			t.Fatal(err)
		}
	}
	p := nativePull(1)
	p.State = state
	f.observe(t, reviewRequest(0, []PullInput{p}), "initial-notice-state")
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		for _, worker := range []string{"participant", "muted", "retired"} {
			if err := AutoFollow(ctx, tx, core.Scope{ProjectID: f.project, WorkerID: worker}, 1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE review_pr_followers SET mode='muted' WHERE worker_id='muted'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO worker_retirements VALUES(?,'retired','2026-11-01T00:00:00Z','test')`, f.project); err != nil {
		t.Fatal(err)
	}
	return f
}
func noticeCount(t *testing.T, f *apiFixture) int {
	t.Helper()
	var n int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM inbox_messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func noticeState(t *testing.T, f *apiFixture, state string) {
	t.Helper()
	p := sourcePull(1, "Selected PR title")
	p.State = state
	if _, err := publishStatus(t, f, sourcePins(t, f, 1), p, "", "unknown"); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalNoticesVerifiedTransitionReplayReopenAndRecipients(t *testing.T) {
	f := noticeFixture(t, "open")
	pins := sourcePins(t, f, 1)
	p := sourcePull(1, "Selected PR title")
	p.State = "merged"
	if _, err := publishStatus(t, f, pins, p, "", "unknown"); err != nil {
		t.Fatal(err)
	}
	if noticeCount(t, f) != 1 {
		t.Fatal("missing or overbroad notification")
	}
	var recipient, body, actor string
	if err := f.store.DB().QueryRow(`SELECT recipient_worker_id,body FROM inbox_messages`).Scan(&recipient, &body); err != nil {
		t.Fatal(err)
	}
	if recipient != "participant" || !strings.Contains(body, "PR #1 was merged: Selected PR title") || !strings.Contains(body, "https://github.com/owner/reviews/pull/1") {
		t.Fatal(recipient, body)
	}
	if err := f.store.DB().QueryRow(`SELECT actor_id FROM events WHERE event_type='message.sent'`).Scan(&actor); err != nil || actor != "service-lifecycle-sync" {
		t.Fatal(actor, err)
	}
	if _, err := publishStatus(t, f, pins, p, "", "unknown"); err != nil {
		t.Fatal(err)
	} // stale pin
	noticeState(t, f, "merged") // unchanged
	p.Title = "Updated closed title"
	if _, err := publishStatus(t, f, sourcePins(t, f, 1), p, "", "unknown"); err != nil {
		t.Fatal(err)
	}
	if noticeCount(t, f) != 1 {
		t.Fatal("same terminal state notified again")
	}
	noticeState(t, f, "open")
	noticeState(t, f, "closed")
	if noticeCount(t, f) != 2 {
		t.Fatal("reopened PR did not produce its new closure")
	}
	var unseen, unack int
	if err := f.store.DB().QueryRow(`SELECT sum(seen),sum(acknowledged) FROM inbox_recipient_state`).Scan(&unseen, &unack); err != nil || unseen != 0 || unack != 0 {
		t.Fatal("producer changed handling state", unseen, unack, err)
	}
}

func TestTerminalNoticesBaselineAndUnverifiedMetadataStayQuiet(t *testing.T) {
	for _, state := range []string{"unknown", "closed", "merged"} {
		t.Run(state, func(t *testing.T) {
			f := noticeFixture(t, state)
			if noticeCount(t, f) != 0 {
				t.Fatal("initial history spam")
			}
			noticeState(t, f, "closed")
			if noticeCount(t, f) != 0 {
				t.Fatal("non-open baseline sent notification")
			}
		})
	}
	f := noticeFixture(t, "open")
	p := sourcePull(1, "ordinary metadata")
	p.State = "closed"
	if _, err := sourcePublish(f, sourcePins(t, f, 1), p); err != nil {
		t.Fatal(err)
	}
	if noticeCount(t, f) != 0 {
		t.Fatal("ordinary metadata/import supplied notification authority")
	}
}

func TestTerminalNoticesConcurrentObservationPublishesOnce(t *testing.T) {
	f := noticeFixture(t, "open")
	pins := sourcePins(t, f, 1)
	p := sourcePull(1, "concurrent close")
	p.State = "closed"
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := publishStatus(t, f, pins, p, "", "unknown"); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if noticeCount(t, f) != 1 {
		t.Fatal("concurrent observation duplicated notice")
	}
}

func TestTerminalNoticesAtomicFailureAndRetry(t *testing.T) {
	for _, event := range []string{"message.sent", "review.observed"} {
		t.Run(event, func(t *testing.T) {
			f := noticeFixture(t, "open")
			if _, err := f.store.DB().Exec(fmt.Sprintf(`CREATE TRIGGER reject_notice BEFORE INSERT ON events WHEN NEW.event_type='%s' BEGIN SELECT RAISE(ABORT,'synthetic notice failure'); END`, event)); err != nil {
				t.Fatal(err)
			}
			before := historyMigrationDatabaseDigest(t, f)
			p := sourcePull(1, "atomic close")
			p.State = "closed"
			pins := sourcePins(t, f, 1)
			if _, err := publishStatus(t, f, pins, p, "", "unknown"); err == nil {
				t.Fatal("synthetic failure accepted")
			}
			if historyMigrationDatabaseDigest(t, f) != before || noticeCount(t, f) != 0 {
				t.Fatal("notification failure leaked state/message/dedupe")
			}
			if _, err := f.store.DB().Exec(`DROP TRIGGER reject_notice`); err != nil {
				t.Fatal(err)
			}
			if _, err := publishStatus(t, f, pins, p, "", "unknown"); err != nil {
				t.Fatal(err)
			}
			if noticeCount(t, f) != 1 {
				t.Fatal("retry failed")
			}
		})
	}
}

func TestNotificationPreferencesAutoFollowPreservesMuteAndBackupValidation(t *testing.T) {
	f := noticeFixture(t, "open")
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return AutoFollow(ctx, tx, core.Scope{ProjectID: f.project, WorkerID: "muted"}, 1)
	}); err != nil {
		t.Fatal(err)
	}
	v, err := ReadFollowing(t.Context(), f.store.DB(), f.project, "muted", 1)
	if err != nil || v.Mode != "muted" || v.Revision != 1 {
		t.Fatal(v, err)
	}
	if err := ValidateNotificationPreferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE review_pr_followers SET updated_at='not a timestamp' WHERE worker_id='participant'`); err != nil {
		t.Fatal(err)
	}
	before := historyMigrationDatabaseDigest(t, f)
	if err := ValidateNotificationPreferences(t.Context(), f.store.DB()); err == nil || historyMigrationDatabaseDigest(t, f) != before {
		t.Fatal("backup validation accepted/repaired invalid preferences")
	}
}

func TestNotificationFollowingIndependentReviewUsesActualWorkerBindings(t *testing.T) {
	f, token, base := subagentFixture(t)
	for _, worker := range []string{"reporter", "finalizer"} {
		if _, err := f.store.DB().Exec(`INSERT INTO workers VALUES(?,?,?)`, f.project, worker, worker); err != nil {
			t.Fatal(err)
		}
	}
	call := func(worker, path, key string, input any) SubagentReceipt {
		t.Helper()
		body, _ := json.Marshal(input)
		r := httptest.NewRequest("POST", path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-FC-Worker", worker)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		var value SubagentReceipt
		if w.Code < 200 || w.Code >= 300 || json.Unmarshal(w.Body.Bytes(), &value) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return value
	}
	input := subagentInput()
	input.Findings = []SubagentFindingInput{}
	first := call("reporter", base, "report", input)
	call("finalizer", base+"/"+first.ID+"/updates", "finalize", SubagentUpdateRequest{ExpectedRevision: &first.Revision, Finalize: true})
	for _, worker := range []string{"reporter", "finalizer"} {
		v, err := ReadFollowing(t.Context(), f.store.DB(), f.project, worker, 7)
		if err != nil || v.Mode != "auto" {
			t.Fatal(worker, v, err)
		}
	}
}

func TestTerminalNoticePreservesExplicitSourceObserverProvenance(t *testing.T) {
	f := noticeFixture(t, "open")
	pins := sourcePins(t, f, 1)
	p := sourcePull(1, "Explicit observed close")
	p.State = "closed"
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := PublishSourceStatus(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "explicit-source-requester", WorkerID: "participant"}, 701, pins, []SourceStatus{{Pull: p, Mergeability: "unknown"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var actor, origin string
	var worker sql.NullString
	if err := f.store.DB().QueryRow(`SELECT e.actor_id,m.origin_kind,m.sender_worker_id FROM events e JOIN inbox_messages m ON m.id=e.resource_id AND m.project_id=e.project_id WHERE e.event_type='message.sent'`).Scan(&actor, &origin, &worker); err != nil || actor != "explicit-source-requester" || origin != "controller" || worker.Valid {
		t.Fatal("system origin changed actual observer attribution", actor, origin, worker, err)
	}
}

func TestNotificationPreferenceValidationRejectsCoercedAndOversizedStorage(t *testing.T) {
	for name, damage := range map[string]string{
		"invalid seed marker": `INSERT INTO metadata(key,value) VALUES('review_notifications:manual_seed_v1','0')`,
		"blob seed marker":    `INSERT INTO metadata(key,value) VALUES('review_notifications:manual_seed_v1',CAST('1' AS BLOB))`,
		"blob timestamp":      `UPDATE review_pr_followers SET updated_at=CAST(updated_at AS BLOB) WHERE worker_id='participant'`,
		"blob revision":       `UPDATE review_pr_followers SET revision=CAST('1' AS BLOB) WHERE worker_id='participant'`,
		"oversized timestamp": `UPDATE review_pr_followers SET updated_at=hex(zeroblob(65536)) WHERE worker_id='participant'`,
	} {
		t.Run(name, func(t *testing.T) {
			f := noticeFixture(t, "open")
			if _, err := f.store.DB().Exec(damage); err != nil {
				t.Fatal(err)
			}
			before := historyMigrationDatabaseDigest(t, f)
			if err := ValidateNotificationPreferences(t.Context(), f.store.DB()); err == nil {
				t.Fatal("invalid physical preference accepted")
			}
			if historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("validation repaired corruption")
			}
		})
	}
}
