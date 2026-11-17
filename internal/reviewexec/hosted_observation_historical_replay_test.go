package reviewexec

import (
	"strings"
	"testing"
)

func TestHostedObservationExactReplayAfterCooldownDetachmentAndRetry(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "detached", true: "retry started"}[retry], func(t *testing.T) {
			f, a, cooldown, now := queuedCooldown(t)
			records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
			if err != nil || len(records) != 1 {
				t.Fatal("original observation", records, err)
			}
			original := records[0]
			if retry {
				*now = cooldown.Observation.RetryNotBefore
				next := f.start(t, attemptProject, Receipt{ID: a.OperationID})
				if next.ID == a.ID {
					t.Fatal("retry reused attempt")
				}
			}
			before := hostedObservationState(t, f, a)
			got, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, original.Input)
			if err != nil || got != (Receipt{ID: original.ID, State: "observed", Revision: 1}) {
				t.Fatal("historical exact replay", got, err)
			}
			changed := original.Input
			changed.CaptureBytes++
			_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, changed)
			attemptCode(t, err, "review_conflict")
			fresh := original.Input
			fresh.CaptureSHA256 = strings.Repeat("f", 64)
			fresh.CaptureID = "review_capture_" + fresh.CaptureSHA256[:32]
			_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, fresh)
			attemptCode(t, err, "review_conflict")
			if hostedObservationState(t, f, a) != before {
				t.Fatal("historical replay or rejected append changed journal")
			}
		})
	}
}
