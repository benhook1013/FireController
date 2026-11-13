package reviewexec

import (
	"strings"
	"testing"
)

func TestDriverlessInterruptedRecoveryPreservesHostedReplayEvidence(t *testing.T) {
	f, a, _ := cooldownObservationFixture(t)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "unused-cli-driverless", "cli", 2, 1))
	// Rebuild through the supported constructor with the last capability removed,
	// matching daemon startup's zero-driver branch.
	engine, err := New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, a)
	if err := engine.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if before != attemptState(t, f, a) {
		t.Fatal("driverless recovery changed retained Hosted attempt/evidence/reservation")
	}
	got, err := readAttempt(t.Context(), f.store.DB(), otherAttemptProject, other.ID)
	if err != nil || got.State != "cancelled" {
		t.Fatal("unrelated unused CLI preparation not recovered", got, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id='`+a.ID+`'`) != 1 {
		t.Fatal("original Hosted reservation released")
	}
}

func TestRecoveryCapabilityAliasesHaveDistinctNormalizedIdentity(t *testing.T) {
	f := newAttemptFixture(t)
	a := Capability{ID: "original", Provider: "coderabbit", Account: "shared", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("a", 64)}
	b := a
	b.ID = "alias"
	engine, err := New(f.store, []Capability{a, b})
	if err != nil {
		t.Fatal(err)
	}
	original, alias := engine.capabilities[a.ID], engine.capabilities[b.ID]
	if original.Fingerprint == alias.Fingerprint || original.Provider != alias.Provider || original.Account != alias.Account {
		t.Fatal("distinct keys aliased canonical recovery identity")
	}
	if !engine.MatchesCapability(a) || !engine.MatchesCapability(b) {
		t.Fatal("native bindings not retained")
	}
	if _, err := New(f.store, []Capability{a, a}); err == nil {
		t.Fatal("duplicate capability key accepted")
	}
}
