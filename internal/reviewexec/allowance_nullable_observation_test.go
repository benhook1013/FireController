package reviewexec

import "testing"

func TestCooldownReferenceNullIdentityIsLocalDamage(t *testing.T) {
	f, a := interruptedAllowanceFixture(t, "hosted")
	o := cliWaitRead(t, f, a.OperationID)
	admissionSQL(t, f, "UPDATE review_hosted_observations SET id=NULL WHERE attempt_id=?", a.ID)
	if _, err := readHostedAllowanceRecord(t.Context(), f.store.DB(), o, a); !storedAllowanceUnavailable(err) {
		t.Fatal("nullable observation identity lost corruption classification", err)
	}
	assertCorruptAllowanceRecovery(t, f, a)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	next := f.enqueue(t, otherAttemptProject, "after-null-observation", "cli", 2, 1)
	if healthy := f.start(t, otherAttemptProject, next); healthy.State != "preparing" {
		t.Fatal("damaged candidate blocked independent work", healthy)
	}
}
