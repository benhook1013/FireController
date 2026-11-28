package reviewexec

import "testing"

func TestSchedulingSnapshotRetainsPauseAuthority(t *testing.T) {
	for _, corrupt := range []string{"", "unpaused-row", "missing-event", "duplicate-event", "malformed-event", "blob-event", "blob-actor", "wrong-pull", "wrong-actor", "newer-event"} {
		t.Run(corrupt, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingPause(t, f, 2, 0, true)
			switch corrupt {
			case "unpaused-row":
				admissionSQL(t, f, `UPDATE review_pull_scheduling SET paused=0,reason='' WHERE pull_number=2`)
			case "missing-event":
				admissionSQL(t, f, `DELETE FROM events WHERE event_type='review.pull_scheduling_updated'`)
			case "duplicate-event":
				admissionSQL(t, f, `INSERT INTO events(project_id,event_type,resource_id,revision,actor_id,payload_json,created_at) SELECT project_id,event_type,resource_id,revision,actor_id,payload_json,created_at FROM events WHERE event_type='review.pull_scheduling_updated'`)
			case "malformed-event":
				admissionSQL(t, f, `UPDATE events SET payload_json='{}' WHERE event_type='review.pull_scheduling_updated'`)
			case "blob-event":
				admissionSQL(t, f, `UPDATE events SET payload_json=CAST(payload_json AS BLOB) WHERE event_type='review.pull_scheduling_updated'`)
			case "blob-actor":
				admissionSQL(t, f, `UPDATE events SET actor_id=CAST(actor_id AS BLOB) WHERE event_type='review.pull_scheduling_updated'`)
			case "wrong-pull":
				admissionSQL(t, f, `UPDATE events SET payload_json='{"pull_number":4,"paused":true}' WHERE event_type='review.pull_scheduling_updated'`)
			case "wrong-actor":
				admissionSQL(t, f, `UPDATE events SET actor_id='exec-reader-principal' WHERE event_type='review.pull_scheduling_updated'`)
			case "newer-event":
				admissionSQL(t, f, `UPDATE events SET revision=2 WHERE event_type='review.pull_scheduling_updated'`)
			}
			err := validateSchedulingSnapshotForTest(t, f)
			if corrupt != "" {
				attemptCode(t, err, "storage_unavailable")
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSchedulingSnapshotAcceptsPauseThenResumeHistory(t *testing.T) {
	f := schedulingFixture(t)
	schedulingPause(t, f, 2, 0, true)
	schedulingPause(t, f, 2, 1, false)
	if err := validateSchedulingSnapshotForTest(t, f); err != nil {
		t.Fatal(err)
	}
}
