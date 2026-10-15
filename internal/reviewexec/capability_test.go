package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
)

func bindingCapability() Capability {
	return Capability{ID: "test-capability", Provider: "coderabbit", Account: "test-account", Channels: []string{"hosted", "cli"}, Fingerprint: strings.Repeat("a", 64)}
}

func bindEngine(t *testing.T, f *attemptFixture, capability Capability) {
	t.Helper()
	var err error
	f.e, err = New(f.store, []Capability{capability})
	if err != nil {
		t.Fatal(err)
	}
}

func bindingOperation(t *testing.T, f *attemptFixture, scope core.Scope, id string) Operation {
	t.Helper()
	o, err := f.e.ReadOperation(context.Background(), scope, id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func bindingRequest() Request {
	return Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
}

func bindingEnqueue(t *testing.T, f *attemptFixture, scope core.Scope, key string) (Receipt, []byte) {
	t.Helper()
	data, err := f.e.Enqueue(context.Background(), scope, key, bindingRequest())
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt, data
}

func TestCapabilityEquivalentBindingsAndCallerChannelIsolation(t *testing.T) {
	for _, mode := range []string{"identical", "reordered", "caller_mutation"} {
		t.Run(mode, func(t *testing.T) {
			f := newAttemptFixture(t)
			input := bindingCapability()
			bindEngine(t, f, input)
			if !reflect.DeepEqual(input.Channels, []string{"hosted", "cli"}) {
				t.Fatal("New sorted the caller's channel slice in place")
			}
			if mode == "caller_mutation" {
				input.Channels[0], input.Channels[1] = "invalid", "invalid-again"
			}
			// Configuration and enqueue still work after a caller changes its
			// own slice; the engine's permitted channels must be independent.
			f.configure(t, attemptProject, "test-capability", 0, false)
			scope := f.scope(t, attemptProject, f.writer, false)
			r, _ := bindingEnqueue(t, f, scope, "stable-binding")
			before := bindingOperation(t, f, scope, r.ID)
			if len(before.CapabilityFingerprint) != 64 || before.CapabilityFingerprint == bindingCapability().Fingerprint {
				t.Fatal("enqueue did not retain an effective capability identity")
			}
			if mode != "caller_mutation" {
				next := bindingCapability()
				if mode == "reordered" {
					next.Channels = []string{"cli", "hosted"}
				}
				bindEngine(t, f, next)
			}
			a := f.start(t, attemptProject, r)
			admitted, err := f.e.Admit(context.Background(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
			if err != nil || admitted.State != "active" {
				t.Fatal("equivalent effective capability could not admit pinned work", err)
			}
			after := bindingOperation(t, f, scope, r.ID)
			if after.CapabilityFingerprint != before.CapabilityFingerprint || after.ConfigurationRevision != 1 {
				t.Fatal("equivalent capability changed the operation binding or project configuration")
			}
		})
	}
}

func TestCapabilityChangesHoldQueuedReplayAndBindOnlyNewIntent(t *testing.T) {
	for _, change := range []string{"private_configuration", "provider", "account"} {
		t.Run(change, func(t *testing.T) {
			f := newAttemptFixture(t)
			bindEngine(t, f, bindingCapability())
			f.configure(t, attemptProject, "test-capability", 0, false)
			scope := f.scope(t, attemptProject, f.writer, false)
			oldReceipt, original := bindingEnqueue(t, f, scope, "old-intent")
			oldOperation := bindingOperation(t, f, scope, oldReceipt.ID)
			changed := bindingCapability()
			switch change {
			case "private_configuration":
				changed.Fingerprint = strings.Repeat("b", 64)
			case "provider":
				changed.Provider = "other-provider"
			case "account":
				changed.Account = "other-account"
			}
			bindEngine(t, f, changed)
			_, replay := bindingEnqueue(t, f, scope, "old-intent")
			if !bytes.Equal(replay, original) {
				t.Fatal("request retry rebound or replaced its original receipt")
			}
			a, err := f.e.StartAttempt(context.Background(), attemptProject, oldReceipt.ID)
			if err != nil || a.ID != "" {
				t.Fatal("changed capability must hold queued work without creating an attempt", err)
			}
			held := bindingOperation(t, f, scope, oldReceipt.ID)
			if held.State != "held" || held.Reason != "capability_changed" || held.CapabilityFingerprint != oldOperation.CapabilityFingerprint || held.ConfigurationRevision != 1 {
				t.Fatal("changed capability did not retain the old assertion in a visible hold", held)
			}
			_, replay = bindingEnqueue(t, f, scope, "old-intent")
			if !bytes.Equal(replay, original) || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("held request replay re-enqueued or reserved external work")
			}
			fresh, _ := bindingEnqueue(t, f, scope, "new-explicit-intent")
			current := bindingOperation(t, f, scope, fresh.ID)
			if current.ID == oldOperation.ID || current.CapabilityFingerprint == oldOperation.CapabilityFingerprint || current.ConfigurationRevision != 1 {
				t.Fatal("new intended request did not bind the changed effective capability")
			}
			started := f.start(t, attemptProject, fresh)
			if started.Provider != changed.Provider || started.Account != changed.Account {
				t.Fatal("new intent did not use its bound provider/account")
			}
			if _, err := f.e.Admit(context.Background(), attemptProject, started.ID, 1, attemptAnchor(attemptProject, 2), ""); err != nil {
				t.Fatal("freshly bound capability could not admit new explicit work", err)
			}
		})
	}
}

func TestCapabilityChangesDuringPreparationRefuseAdmission(t *testing.T) {
	for _, change := range []string{"private_configuration", "provider", "account"} {
		t.Run(change, func(t *testing.T) {
			f := newAttemptFixture(t)
			bindEngine(t, f, bindingCapability())
			f.configure(t, attemptProject, "test-capability", 0, false)
			r := f.enqueue(t, attemptProject, "preparing-bound-capability", "cli", 2, 1)
			a := f.start(t, attemptProject, r)
			before := attemptState(t, f, a)
			changed := bindingCapability()
			switch change {
			case "private_configuration":
				changed.Fingerprint = strings.Repeat("c", 64)
			case "provider":
				changed.Provider = "different-provider"
			case "account":
				changed.Account = "different-account"
			}
			bindEngine(t, f, changed)
			_, err := f.e.Admit(context.Background(), attemptProject, a.ID, 1, attemptAnchor(attemptProject, 2), "")
			attemptCode(t, err, "review_conflict")
			if attemptState(t, f, a) != before {
				t.Fatal("refused admission partially changed journal or reservation")
			}
		})
	}
}

func TestCapabilityMalformedPrivateFingerprintsAreRejected(t *testing.T) {
	f := newAttemptFixture(t)
	for _, fingerprint := range []string{"abc", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("F", 64), strings.Repeat("g", 64), " " + strings.Repeat("a", 64), strings.Repeat("a", 63) + "\x00"} {
		c := bindingCapability()
		c.Fingerprint = fingerprint
		_, err := New(f.store, []Capability{c})
		attemptCode(t, err, "validation_failed")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_config`) != 0 {
		t.Fatal("rejected operator configuration created domain state")
	}
}

func TestCapabilitySchemaUpgradeRetainsUnboundOperationsAndReplay(t *testing.T) {
	f := newAttemptFixture(t)
	if err := jobs.EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	bindEngine(t, f, bindingCapability())
	f.configure(t, attemptProject, "test-capability", 0, false)
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'retained-worker','Builder')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO jobs(project_id,id,title,brief,worker_id,status,revision,created_at,updated_at) VALUES(?,'retained-job','Retained title','Retained exact brief','retained-worker','active',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, attemptProject); err != nil {
		t.Fatal(err)
	}
	scope := f.scope(t, attemptProject, f.writer, false)
	scope.WorkerID = "retained-worker"
	oldReceipt, original := bindingEnqueue(t, f, scope, "legacy-queued-key")
	before := bindingOperation(t, f, scope, oldReceipt.ID)
	instance := f.store.InstanceID()
	// Reconstruct the actual version1 table shape only in this owned fixture.
	if _, err := f.store.DB().Exec(`DROP INDEX review_operations_coverage`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`ALTER TABLE review_operations DROP COLUMN policy_revision`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`ALTER TABLE review_operations DROP COLUMN capability_fingerprint`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE metadata SET value='1' WHERE key='schema:review_execution'`); err != nil {
		t.Fatal(err)
	}
	var sequence int
	var name, database string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &database); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(context.Background(), filepath.Dir(database))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	if reopened.InstanceID() != instance {
		t.Fatal("upgrade changed instance")
	}
	if err := EnsureSchema(context.Background(), reopened); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(context.Background(), reopened); err != nil {
		t.Fatal("second version3 startup was not idempotent", err)
	}
	bindEngine(t, f, bindingCapability())
	var version, title, brief, worker string
	if err := reopened.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil || version != "3" {
		t.Fatal("migration did not advance only the domain version", err)
	}
	if err := reopened.DB().QueryRow(`SELECT title,brief,worker_id FROM jobs WHERE id='retained-job'`).Scan(&title, &brief, &worker); err != nil || title != "Retained title" || brief != "Retained exact brief" || worker != "retained-worker" {
		t.Fatal("upgrade changed unrelated project data", err)
	}
	upgraded := bindingOperation(t, f, scope, oldReceipt.ID)
	before.CapabilityFingerprint = ""
	before.PolicyRevision = 0
	if !reflect.DeepEqual(before, upgraded) {
		t.Fatal("upgrade rewrote operation instead of leaving an explicitly unbound assertion", upgraded)
	}
	_, replay := bindingEnqueue(t, f, scope, "legacy-queued-key")
	if !bytes.Equal(original, replay) {
		t.Fatal("upgrade lost exact request receipt")
	}
	a, err := f.e.StartAttempt(context.Background(), attemptProject, oldReceipt.ID)
	if err != nil || a.ID != "" {
		t.Fatal("old unbound operation rebound automatically", err)
	}
	held := bindingOperation(t, f, scope, oldReceipt.ID)
	if held.State != "held" || held.Reason != "capability_changed" || held.CapabilityFingerprint != "" {
		t.Fatal("old unbound request did not hold", held)
	}
	_, replay = bindingEnqueue(t, f, scope, "legacy-queued-key")
	if !bytes.Equal(original, replay) || attemptCount(t, reopened, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, reopened, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("legacy replay created work or reserved an action")
	}
	fresh, _ := bindingEnqueue(t, f, scope, "explicit-post-upgrade-intent")
	if bindingOperation(t, f, scope, fresh.ID).CapabilityFingerprint == "" {
		t.Fatal("new request remained unbound after migration")
	}
	f.start(t, attemptProject, fresh)
}
