package reviewexec

import (
	"testing"
	"time"
)

func TestObserveSettledCLICaptureNativeBinding(t *testing.T) {
	for _, kind := range []string{"eligible", "recent", "reserved", "active", "missing", "wrong-account", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			f, a, in := evidenceFixture(t, "cli", true)
			if kind != "missing" {
				if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "active" {
				if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "completion", ""); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "reserved" {
				admissionSQL(t, f, `INSERT INTO review_execution_reservations(provider,repository_id,pull_number,action_class,account,attempt_id) VALUES('coderabbit',1,1,'cli','test-account',?)`, a.ID)
			}
			if kind == "corrupt" {
				admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=json_set(body_json,'$.attempt_id','other') WHERE attempt_id=?`, a.ID)
			}
			now := time.Now().Add(time.Hour)
			if kind == "recent" {
				now = time.Now()
			}
			cap := "test-capability"
			if kind == "wrong-account" {
				cap = "other-account"
			}
			got, err := f.e.ObserveSettledCLICapture(t.Context(), cap, a.ID, now)
			if kind == "eligible" {
				if err != nil || got.AttemptID != a.ID || got.SHA256 != in.CaptureSHA256 || got.Bytes != in.CaptureBytes {
					t.Fatal(got, err)
				}
			} else if got != (SettledCLICapture{}) {
				t.Fatal("unsafe candidate", got, err)
			}
		})
	}
}
