package reviewexec

import "testing"

func TestCooldownReferenceMalformedNumericStorageIsLocalDamage(t *testing.T) {
	for _, column := range []string{"position", "capture_bytes"} {
		t.Run(column, func(t *testing.T) {
			f, a := interruptedAllowanceFixture(t, "hosted")
			o := cliWaitRead(t, f, a.OperationID)
			admissionSQL(t, f, "UPDATE review_hosted_observations SET "+column+"=1.5 WHERE attempt_id=?", a.ID)
			var kind string
			if err := f.store.DB().QueryRow("SELECT typeof("+column+") FROM review_hosted_observations WHERE attempt_id=?", a.ID).Scan(&kind); err != nil || kind != "real" {
				t.Fatal("fractional fixture not retained", kind, err)
			}
			if _, err := readHostedAllowanceRecord(t.Context(), f.store.DB(), o, a); !storedAllowanceUnavailable(err) {
				t.Fatal("fractional optional evidence lost local classification", err)
			}
			assertCorruptAllowanceRecovery(t, f, a)
			// Independent work can still prepare after the damaged candidate settles.
			f.configure(t, otherAttemptProject, "other-account", 0, false)
			next := f.enqueue(t, otherAttemptProject, "after-fractional-cooldown", "cli", 2, 1)
			healthy := f.start(t, otherAttemptProject, next)
			if healthy.State != "preparing" {
				t.Fatal("damaged candidate blocked independent work", healthy)
			}
		})
	}
}
