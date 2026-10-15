package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

const attemptProject = "exec-project-one"
const otherAttemptProject = "exec-project-two"

var attemptCapabilities = []Capability{
	{ID: "test-capability", Provider: "coderabbit", Account: "test-account", Channels: []string{"cli", "hosted"}},
	{ID: "other-account", Provider: "coderabbit", Account: "second-account", Channels: []string{"cli", "hosted"}},
}

type attemptFixture struct {
	*httpFixture
	e *Engine
}

func newAttemptFixture(t *testing.T) *attemptFixture {
	t.Helper()
	return newAttemptFixtureWithPolicy(t, true)
}

func newAttemptFixtureWithPolicy(t *testing.T, configured bool) *attemptFixture {
	t.Helper()
	f := &attemptFixture{httpFixture: newHTTPFixtureWithPolicy(t, configured)}
	var err error
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *attemptFixture) scope(t *testing.T, project, token string, admin bool) core.Scope {
	t.Helper()
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+token)
	var scope core.Scope
	var err error
	if admin {
		scope, err = f.store.AdminScope(r)
	} else {
		scope, err = f.store.Scope(r, "write")
	}
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func (f *attemptFixture) configure(t *testing.T, project, capability string, previous int64, paused bool) {
	t.Helper()
	c := Configuration{ExpectedRevision: &previous, CapabilityID: capability, Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, Paused: paused}
	_, err := f.e.Configure(context.Background(), f.scope(t, project, f.admin, true), fmt.Sprintf("config-%s-%d", project, previous), c)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *attemptFixture) enqueue(t *testing.T, project, key, channel string, number, revision int64) Receipt {
	t.Helper()
	token := f.writer
	if project == otherAttemptProject {
		token = f.other
	}
	data, err := f.e.Enqueue(context.Background(), f.scope(t, project, token, false), key, Request{ExpectedConfigurationRevision: revision, PullNumber: number, Channel: channel, HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func (f *attemptFixture) start(t *testing.T, project string, receipt Receipt) Attempt {
	t.Helper()
	a, err := f.e.StartAttempt(context.Background(), project, receipt.ID)
	if err != nil || a.State != "preparing" || a.ID == "" || a.Revision != 1 {
		t.Fatalf("start: %+v %v", a, err)
	}
	return a
}

func attemptAnchor(project string, number int64) Anchor {
	repositoryID := int64(801)
	if project == otherAttemptProject {
		repositoryID = 802
	}
	return Anchor{RepositoryID: repositoryID, Repository: "owner/repo", PullNumber: number, BaseRef: "main", CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("c", 40), MergeBaseSHA: strings.Repeat("d", 40), PatchSHA256: strings.Repeat("e", 64), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
}

func attemptCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("expected %s, got %v", code, err)
	}
}

func attemptCount(t *testing.T, store *core.Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// Includes the durable projection and journal lengths, so failed calls cannot
// pass merely by returning the correct error after partially mutating state.
func attemptState(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var state, anchor, intent, external, opState, reason string
	var revision, opRevision int64
	if err := f.store.DB().QueryRow(`SELECT a.state,a.revision,a.anchor_json,a.intent,a.external_id,o.state,o.revision,o.reason FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE a.id=?`, a.ID).Scan(&state, &revision, &anchor, &intent, &external, &opState, &opRevision, &reason); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%q/%d/%q/%q/%q/%q/%d/%q/%d/%d", state, revision, anchor, intent, external, opState, opRevision, reason,
		attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=?`, a.ID),
		attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID))
}

func TestAttemptRequestReplayAndConcurrentStart(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	scope := f.scope(t, attemptProject, f.writer, false)
	r := Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
	first, err := f.e.Enqueue(context.Background(), scope, "replayed-request", r)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	var compact map[string]any
	if json.Unmarshal(first, &receipt) != nil || json.Unmarshal(first, &compact) != nil || len(compact) != 3 || receipt.State != "queued" || receipt.Revision != 1 {
		t.Fatal("request did not return only compact id/state/revision")
	}
	start := make(chan struct{})
	type result struct {
		a   Attempt
		err error
	}
	results := make(chan result, 8)
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			a, err := f.e.StartAttempt(context.Background(), attemptProject, receipt.ID)
			results <- result{a, err}
		}()
	}
	close(start)
	var winner Attempt
	for i := 0; i < 8; i++ {
		got := <-results
		if got.err == nil {
			if winner.ID != "" || got.a.State != "preparing" {
				t.Fatal("more than one start winner")
			}
			winner = got.a
		} else {
			attemptCode(t, got.err, "review_conflict")
		}
	}
	if winner.ID == "" {
		t.Fatal("no start won")
	}
	// Exact request retries retain their original receipt even after state/config movement.
	f.configure(t, attemptProject, "test-capability", 1, true)
	before := attemptState(t, f, winner)
	replay, err := f.e.Enqueue(context.Background(), scope, "replayed-request", r)
	if err != nil || !bytes.Equal(first, replay) || attemptState(t, f, winner) != before {
		t.Fatal("request replay re-enqueued or changed active work", err)
	}
	r.HeadSHA = strings.Repeat("b", 40)
	_, err = f.e.Enqueue(context.Background(), scope, "replayed-request", r)
	attemptCode(t, err, "idempotency_conflict")
	for table, expected := range map[string]int{"review_operations": 1, "review_execution_attempts": 1, "review_execution_reservations": 1, "review_execution_transitions": 1} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != expected {
			t.Fatal("replay or concurrent start duplicated", table)
		}
	}
}

func TestAttemptReservationsSerializeAccountAndExactTarget(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	first := f.start(t, attemptProject, f.enqueue(t, attemptProject, "first", "cli", 2, 1))
	for _, project := range []string{attemptProject, otherAttemptProject} {
		r := f.enqueue(t, project, "account-busy-"+project, "cli", 4, 1)
		_, err := f.e.StartAttempt(context.Background(), project, r.ID)
		attemptCode(t, err, "review_busy")
		if strings.Contains(err.Error(), first.ID) || strings.Contains(err.Error(), first.ProjectID) {
			t.Fatal("busy error exposed another reservation")
		}
	}
	// Same account and exact target are independently available to hosted work.
	hosted := f.start(t, attemptProject, f.enqueue(t, attemptProject, "hosted-independent", "hosted", 2, 1))
	if hosted.ID == first.ID || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 2 {
		t.Fatal("hosted and CLI shared a reservation slot")
	}
	if _, err := f.e.Admit(context.Background(), attemptProject, first.ID, 1, attemptAnchor(attemptProject, 2), ""); err != nil {
		t.Fatal(err)
	}
	// Changing configured accounts does not bypass an already occupied target.
	f.configure(t, attemptProject, "other-account", 1, false)
	r := f.enqueue(t, attemptProject, "same-target-other-account", "cli", 2, 2)
	_, err := f.e.StartAttempt(context.Background(), attemptProject, r.ID)
	attemptCode(t, err, "review_busy")
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 2 {
		t.Fatal("busy calls created attempts")
	}
	// A different target with the new account is independent.
	f.start(t, attemptProject, f.enqueue(t, attemptProject, "different-target-other-account", "cli", 4, 2))
}

func TestAttemptStartHoldsChangedConfigurationOrAuthorization(t *testing.T) {
	for _, change := range []string{"configuration", "paused", "revoked", "write_removed", "worker_removed", "capability_removed", "capability_channel_removed"} {
		t.Run(change, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			scope := f.scope(t, attemptProject, f.writer, false)
			if change == "worker_removed" {
				if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'bound-worker','Builder')`, attemptProject); err != nil {
					t.Fatal(err)
				}
				scope.WorkerID = "bound-worker"
			}
			data, err := f.e.Enqueue(context.Background(), scope, "hold-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
			if err != nil {
				t.Fatal(err)
			}
			var receipt Receipt
			if err := json.Unmarshal(data, &receipt); err != nil {
				t.Fatal(err)
			}
			wantReason := "authorization_changed"
			switch change {
			case "configuration", "paused":
				f.configure(t, attemptProject, "test-capability", 1, change == "paused")
				wantReason = "configuration_changed"
			case "revoked":
				_, err = f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "write_removed":
				_, err = f.store.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id='exec-writer-cred'`)
			case "worker_removed":
				_, err = f.store.DB().Exec(`DELETE FROM workers WHERE id='bound-worker'`)
			case "capability_removed":
				f.e, err = New(f.store, nil)
				wantReason = "capability_unavailable"
			case "capability_channel_removed":
				f.e, err = New(f.store, []Capability{{ID: "test-capability", Provider: "coderabbit", Account: "test-account", Channels: []string{"hosted"}}})
				wantReason = "capability_unavailable"
			}
			if err != nil {
				t.Fatal(err)
			}
			a, err := f.e.StartAttempt(context.Background(), attemptProject, receipt.ID)
			if err != nil || a.ID != "" {
				t.Fatal("hold must not authorize an attempt", err)
			}
			var state, reason string
			var revision int
			if err := f.store.DB().QueryRow(`SELECT state,reason,revision FROM review_operations WHERE id=?`, receipt.ID).Scan(&state, &reason, &revision); err != nil || state != "held" || reason != wantReason || revision != 2 {
				t.Fatalf("held projection %s %s %d: %v", state, reason, revision, err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`)+attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("held request reserved an external action")
			}
		})
	}
}

func TestAttemptAdmissionRejectsInvalidOrChangedEvidenceAtomically(t *testing.T) {
	cases := []struct {
		name, code string
		change     func(*Anchor, *string)
	}{
		{"pull", "review_conflict", func(a *Anchor, _ *string) { a.PullNumber = 4 }},
		{"head", "review_conflict", func(a *Anchor, _ *string) { a.CandidateSHA = strings.Repeat("b", 40) }},
		{"repository_identity", "review_conflict", func(a *Anchor, _ *string) { a.RepositoryID = 802 }},
		{"repository_zero", "validation_failed", func(a *Anchor, _ *string) { a.RepositoryID = 0 }},
		{"repository_name", "validation_failed", func(a *Anchor, _ *string) { a.Repository = "../repo/extra" }},
		{"parent_number_negative", "validation_failed", func(a *Anchor, _ *string) { a.ParentPullNumber = -1 }},
		{"parent_number_large", "validation_failed", func(a *Anchor, _ *string) { a.ParentPullNumber = 2147483648 }},
		{"base_ref_missing", "validation_failed", func(a *Anchor, _ *string) { a.BaseRef = "" }},
		{"base_ref_large", "validation_failed", func(a *Anchor, _ *string) { a.BaseRef = strings.Repeat("x", 257) }},
		{"base_ref_control", "validation_failed", func(a *Anchor, _ *string) { a.BaseRef = "main\x00" }},
		{"candidate_sha", "validation_failed", func(a *Anchor, _ *string) { a.CandidateSHA = "main" }},
		{"base_sha", "validation_failed", func(a *Anchor, _ *string) { a.BaseSHA = strings.Repeat("B", 40) }},
		{"parent_sha", "validation_failed", func(a *Anchor, _ *string) { a.ParentSHA = "" }},
		{"merge_base", "validation_failed", func(a *Anchor, _ *string) { a.MergeBaseSHA = strings.Repeat("a", 39) }},
		{"patch_hash", "validation_failed", func(a *Anchor, _ *string) { a.PatchSHA256 = strings.Repeat("E", 64) }},
		{"stale", "validation_failed", func(a *Anchor, _ *string) {
			a.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
		}},
		{"future", "validation_failed", func(a *Anchor, _ *string) { a.ObservedAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano) }},
		{"clock", "validation_failed", func(a *Anchor, _ *string) { a.ObservedAt = "not-a-time" }},
		{"intent_missing", "validation_failed", func(_ *Anchor, i *string) { *i = "" }},
		{"intent_large", "validation_failed", func(_ *Anchor, i *string) { *i = strings.Repeat("x", 4097) }},
		{"intent_control", "validation_failed", func(_ *Anchor, i *string) { *i = "intent\x00" }},
		{"intent_utf8", "validation_failed", func(_ *Anchor, i *string) { *i = "intent\xff" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "admission", "hosted", 2, 1))
			anchor, intent := attemptAnchor(attemptProject, 2), "unique-intent-marker"
			tc.change(&anchor, &intent)
			before := attemptState(t, f, a)
			_, err := f.e.Admit(context.Background(), attemptProject, a.ID, 1, anchor, intent)
			attemptCode(t, err, tc.code)
			if attemptState(t, f, a) != before {
				t.Fatal("rejected anchor partially changed journal or reservation")
			}
		})
	}
	for _, change := range []string{"config", "paused", "revoked", "grant", "capability", "capability_channel_removed", "stale_revision", "cross_project", "cli_intent"} {
		t.Run(change, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "changed-admission", "cli", 2, 1))
			project, revision, intent, wantCode := attemptProject, int64(1), "", "review_conflict"
			var err error
			switch change {
			case "config", "paused":
				f.configure(t, attemptProject, "test-capability", 1, change == "paused")
			case "revoked":
				_, err = f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			case "grant":
				_, err = f.store.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id='exec-writer-cred'`)
			case "capability":
				f.e, err = New(f.store, []Capability{{ID: "test-capability", Provider: "coderabbit", Account: "changed-account", Channels: []string{"cli", "hosted"}}})
			case "capability_channel_removed":
				f.e, err = New(f.store, []Capability{{ID: "test-capability", Provider: "coderabbit", Account: "test-account", Channels: []string{"hosted"}}})
			case "stale_revision":
				revision = 2
			case "cross_project":
				project, wantCode = otherAttemptProject, "not_found"
			case "cli_intent":
				intent, wantCode = "unexpected-intent", "validation_failed"
			}
			if err != nil {
				t.Fatal(err)
			}
			before := attemptState(t, f, a)
			_, err = f.e.Admit(context.Background(), project, a.ID, revision, attemptAnchor(attemptProject, 2), intent)
			attemptCode(t, err, wantCode)
			if attemptState(t, f, a) != before {
				t.Fatal("changed admission partially committed")
			}
		})
	}
}

func TestAttemptTransitionCASExternalIdentityAndHistory(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "hosted-journal", "hosted", 2, 1))
	anchor := attemptAnchor(attemptProject, 2)
	posting, err := f.e.Admit(context.Background(), attemptProject, a.ID, 1, anchor, "exact private hosted intent")
	if err != nil || posting.State != "posting" || posting.Revision != 2 || posting.Intent != "exact private hosted intent" || posting.Anchor == nil || *posting.Anchor != anchor {
		t.Fatal("admission did not preserve exact private anchor/intent", err)
	}
	serialized, _ := json.Marshal(posting)
	if bytes.Contains(serialized, []byte(posting.Intent)) {
		t.Fatal("public attempt exposed private intent")
	}
	before := attemptState(t, f, a)
	_, err = f.e.Advance(context.Background(), attemptProject, a.ID, 2, "active", "posted", "")
	attemptCode(t, err, "validation_failed")
	if attemptState(t, f, a) != before {
		t.Fatal("posting without a proven external ID partially changed state")
	}
	active, err := f.e.Advance(context.Background(), attemptProject, a.ID, 2, "active", "posted", "comment_123")
	if err != nil || active.ExternalID != "comment_123" || active.Revision != 3 {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		revision                      int64
		state, reason, external, code string
	}{
		{2, "completed", "captured", "comment_123", "review_conflict"},
		{3, "completed", "captured", "comment_456", "review_conflict"},
		{3, "preparing", "backwards", "", "review_conflict"},
		{3, "completed", "bad reason", "", "validation_failed"},
		{3, "completed", "captured", "bad/external", "validation_failed"},
	} {
		before := attemptState(t, f, a)
		_, err := f.e.Advance(context.Background(), attemptProject, a.ID, tc.revision, tc.state, tc.reason, tc.external)
		attemptCode(t, err, tc.code)
		if attemptState(t, f, a) != before {
			t.Fatal("failed CAS/identity validation rewrote history")
		}
	}
	completed, err := f.e.Advance(context.Background(), attemptProject, a.ID, 3, "completed", "captured", "")
	if err != nil || completed.ExternalID != "comment_123" || completed.Revision != 4 {
		t.Fatal("external ID lost when omitted on a later transition", err)
	}
	type transition struct {
		Revision                  int
		State, Reason, ExternalID string
	}
	want := []transition{{1, "preparing", "preparing", ""}, {2, "posting", "admitted", ""}, {3, "active", "posted", "comment_123"}, {4, "completed", "captured", "comment_123"}}
	rows, err := f.store.DB().Query(`SELECT revision,state,reason,external_id,created_at FROM review_execution_transitions WHERE attempt_id=? ORDER BY revision`, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []transition
	for rows.Next() {
		var x transition
		var at string
		if err := rows.Scan(&x.Revision, &x.State, &x.Reason, &x.ExternalID, &at); err != nil {
			t.Fatal(err)
		}
		if _, err := time.Parse(time.RFC3339Nano, at); err != nil {
			t.Fatal("transition lost timestamp")
		}
		got = append(got, x)
	}
	if rows.Err() != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("immutable transition history: got %+v want %+v", got, want)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("terminal attempt retained account/target reservation")
	}
}

func TestAttemptRestartRecoveryIsIdempotentAndNeverRedispatches(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	preparingReceipt := f.enqueue(t, attemptProject, "prepare-before-restart", "cli", 2, 1)
	preparing := f.start(t, attemptProject, preparingReceipt)
	postingReceipt := f.enqueue(t, attemptProject, "post-before-restart", "hosted", 2, 1)
	posting := f.start(t, attemptProject, postingReceipt)
	var err error
	posting, err = f.e.Admit(context.Background(), attemptProject, posting.ID, 1, attemptAnchor(attemptProject, 2), "retained-recovery-intent")
	if err != nil {
		t.Fatal(err)
	}
	activeReceipt := f.enqueue(t, otherAttemptProject, "active-before-restart", "cli", 4, 1)
	active := f.start(t, otherAttemptProject, activeReceipt)
	active, err = f.e.Admit(context.Background(), otherAttemptProject, active.ID, 1, attemptAnchor(otherAttemptProject, 4), "")
	if err != nil {
		t.Fatal(err)
	}
	var databasePath string
	var sequence int
	var databaseName string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &databaseName, &databasePath); err != nil {
		t.Fatal(err)
	}
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(context.Background(), filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	if reopened.InstanceID() != instance {
		t.Fatal("restart changed instance")
	}
	if err := EnsureSchema(context.Background(), reopened); err != nil {
		t.Fatal(err)
	}
	f.e, err = New(reopened, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, tc := range []struct {
		a            Attempt
		receipt      Receipt
		state        string
		revision     int64
		reservations int
	}{
		{preparing, preparingReceipt, "cancelled", 2, 0}, {posting, postingReceipt, "uncertain", 3, 1}, {active, activeReceipt, "uncertain", 3, 1},
	} {
		var a Attempt
		var anchor, intent, external string
		if err := f.store.DB().QueryRow(`SELECT state,revision,anchor_json,intent,external_id FROM review_execution_attempts WHERE id=?`, tc.a.ID).Scan(&a.State, &a.Revision, &anchor, &intent, &external); err != nil {
			t.Fatal(err)
		}
		if a.State != tc.state || a.Revision != tc.revision || attemptCount(t, reopened, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, tc.a.ID) != tc.reservations {
			t.Fatal("unsafe recovered state", a)
		}
		if tc.a.ID == posting.ID && intent != "retained-recovery-intent" {
			t.Fatal("restart lost hosted reconciliation marker")
		}
		if tc.a.Anchor != nil && anchor != encoded(tc.a.Anchor) {
			t.Fatal("restart changed admitted identity")
		}
		_, err := f.e.StartAttempt(context.Background(), tc.a.ProjectID, tc.receipt.ID)
		attemptCode(t, err, "review_conflict")
		states[tc.a.ID] = attemptState(t, f, tc.a)
	}
	if err := f.e.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, a := range []Attempt{preparing, posting, active} {
		if attemptState(t, f, a) != states[a.ID] {
			t.Fatal("repeat recovery changed journal")
		}
	}
	if attemptCount(t, reopened, `SELECT count(*) FROM review_execution_attempts`) != 3 {
		t.Fatal("recovery created a replacement attempt")
	}
	// CLI preparing was proven undispatched, so its released account is reusable.
	f.start(t, attemptProject, f.enqueue(t, attemptProject, "released-cli-slot", "cli", 4, 1))
	// Uncertain posting/active accounts and targets remain occupied.
	for _, tc := range []struct {
		project, channel string
		number           int64
	}{{attemptProject, "hosted", 4}, {otherAttemptProject, "cli", 2}} {
		r := f.enqueue(t, tc.project, "uncertain-blocked-"+tc.project, tc.channel, tc.number, 1)
		_, err := f.e.StartAttempt(context.Background(), tc.project, r.ID)
		attemptCode(t, err, "review_busy")
	}
}

func TestAttemptTerminalStatesReleaseWithoutManufacturingCoverage(t *testing.T) {
	for _, state := range []string{"completed", "failed", "cancelled", "rate_limited"} {
		t.Run(state, func(t *testing.T) {
			f := newAttemptFixture(t)
			if err := reviews.EnsureSchema(context.Background(), f.store); err != nil {
				t.Fatal(err)
			}
			f.configure(t, attemptProject, "test-capability", 0, false)
			r := f.enqueue(t, attemptProject, "terminal-"+state, "cli", 2, 1)
			a := f.start(t, attemptProject, r)
			active, err := f.e.Admit(context.Background(), attemptProject, a.ID, 1, attemptAnchor(attemptProject, 2), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.e.Advance(context.Background(), attemptProject, a.ID, active.Revision, state, "process_exit_zero", ""); err != nil {
				t.Fatal(err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("terminal state did not release reservation")
			}
			for _, table := range []string{"review_workspace", "review_snapshots", "review_runs", "review_findings", "review_attempts"} {
				if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
					t.Fatal("state/exit reason manufactured review evidence", table)
				}
			}
			_, err = f.e.StartAttempt(context.Background(), attemptProject, r.ID)
			attemptCode(t, err, "review_conflict")
			f.start(t, attemptProject, f.enqueue(t, attemptProject, "explicit-new-"+state, "cli", 2, 1))
		})
	}
}
