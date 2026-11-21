package reviewexec

import (
	"context"
	"database/sql"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func recovery0030Claim(t *testing.T, f *attemptFixture, id string) {
	t.Helper()
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		r, err := readRun(ctx, tx, attemptProject, id)
		if err != nil {
			return err
		}
		return updateRun(ctx, tx, r, "selecting", "", "")
	}); err != nil {
		t.Fatal(err)
	}
}

func recovery0030EnableBoth(t *testing.T, f *attemptFixture) {
	t.Helper()
	revision, on := int64(1), true
	_, err := f.e.ConfigureScheduling(t.Context(), selectionScope(t, f, attemptProject, f.writer, "write"), core.NewID("recovery"), SchedulingRequest{ExpectedRevision: &revision, AutomaticHosted: &on, AutomaticCLI: &on})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecovery0030InterruptedInitialReadKeepsChannelContinuity(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, duringRead := range []bool{false, true} {
			for _, interruptions := range []int{1, 2, 4} {
				t.Run(channel+"/"+map[bool]string{false: "before-claim", true: "during-read"}[duringRead]+"/"+string(rune('0'+interruptions)), func(t *testing.T) {
					f := schedulingFixtureWithQueue(t, []int64{2})
					if channel == "hosted" {
						schedulingEnable(t, f, 0, true)
						schedulingGenerate(t, f, true)
					} else {
						schedulingCLIEnable(t, f, 0, true)
						schedulingCLIGenerate(t, f, true)
					}
					original := schedulingLatest(t, f)
					for range interruptions {
						recovery0030Claim(t, f, original.ID)
						if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					reader := &refreshTestReader{}
					if duringRead {
						reader.repository = func(context.Context, int64) (reviewprovider.Repository, error) {
							recovery0030EnableBoth(t, f)
							return reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, nil
						}
					} else {
						recovery0030EnableBoth(t, f)
					}
					selectionNext(t, f, reader)
					got := selectionRead(t, f, original.ID)
					if got.State != "selected" || got.OperationID == "" || got.ActorID != original.ActorID || got.WorkerID != original.WorkerID || got.Revision != int64(3+2*interruptions) {
						t.Fatal("interrupted initial intent lost continuity", got)
					}
					if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 1 {
						t.Fatal("original intent duplicated")
					}
				})
			}
		}
	}
}

func TestRecovery0030CompletedSourceHoldRequiresCurrentSchedulingRevision(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		for _, changeOtherChannel := range []bool{false, true} {
			interruptionsToTest := []int{1}
			if changeOtherChannel {
				interruptionsToTest = []int{0, 1, 2, 4}
			}
			for _, interruptions := range interruptionsToTest {
				revisionCase := "unchanged-revision"
				if changeOtherChannel {
					revisionCase = "changed-other-channel"
				}
				t.Run(channel+"/"+revisionCase+"/"+string(rune('0'+interruptions)), func(t *testing.T) {
					var f *attemptFixture
					var original Run
					if channel == "hosted" {
						f, original = sourceRetryFixture(t)
					} else {
						f, original = cliSourceRetryFixture(t)
					}
					ageSourceRetry(t, f, original.ID)
					if channel == "hosted" {
						schedulingGenerate(t, f, true)
					} else {
						schedulingCLIGenerate(t, f, true)
					}
					for range interruptions {
						recovery0030Claim(t, f, original.ID)
						if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					if changeOtherChannel {
						// Keep the selected channel on, but enable the other channel.
						// Its distinct scheduling basis does not preserve the shared
						// authorization revision of a completed source hold.
						recovery0030EnableBoth(t, f)
					}
					reader := &refreshTestReader{}
					selectionNext(t, f, reader)
					got := selectionRead(t, f, original.ID)
					if got.ID != original.ID || got.ActorID != original.ActorID || got.WorkerID != original.WorkerID ||
						attemptCount(t, f.store, `SELECT count(*) FROM review_selections`) != 1 ||
						attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
						t.Fatal("retained selection changed, duplicated or attempted provider work", got)
					}
					operations := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`)
					if changeOtherChannel {
						if got.State != "held" || got.Reason != "automation_authorization_changed" ||
							got.OperationID != "" || operations != 0 || reader.calls.Load() != 0 {
							t.Fatal("stale shared revision must stop before source read or child operation", got)
						}
					} else if got.State != "selected" || got.OperationID == "" || operations != 1 || reader.calls.Load() == 0 {
						t.Fatal("current shared revision must resume the original bounded retry", got)
					}
				})
			}
		}
	}
}

func TestRecovery0030InitialReadRequiresCompleteTransitionEvidence(t *testing.T) {
	for _, fault := range []string{"missing", "malformed", "held", "duplicate", "storage"} {
		t.Run(fault, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2})
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			r := schedulingLatest(t, f)
			recovery0030Claim(t, f, r.ID)
			if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
				t.Fatal(err)
			}
			recovery0030Claim(t, f, r.ID)
			r = selectionRead(t, f, r.ID)
			recovery0030EnableBoth(t, f)
			switch fault {
			case "missing":
				admissionSQL(t, f, `DELETE FROM events WHERE resource_id=? AND event_type='review.selection_updated' AND revision=2`, r.ID)
			case "malformed":
				admissionSQL(t, f, `UPDATE events SET payload_json='null' WHERE resource_id=? AND event_type='review.selection_updated' AND revision=2`, r.ID)
			case "held":
				admissionSQL(t, f, `UPDATE events SET payload_json='{"state":"held","reason":"source_fetch_timeout"}' WHERE resource_id=? AND event_type='review.selection_updated' AND revision=2`, r.ID)
			case "duplicate":
				if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					return core.AppendEvent(ctx, tx, core.Scope{ProjectID: r.ProjectID, PrincipalID: r.ActorID, WorkerID: r.WorkerID}, "review.selection_updated", r.ID, 2, map[string]any{"state": "selecting", "reason": ""})
				}); err != nil {
					t.Fatal(err)
				}
			case "storage":
				admissionSQL(t, f, `ALTER TABLE events RENAME TO unavailable_events`)
			}
			var reason string
			err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				var err error
				reason, err = automaticSourceRecoveryFence(ctx, tx, r)
				return err
			})
			if fault == "storage" {
				if err == nil {
					t.Fatal("shared storage failure suppressed")
				}
			} else if err != nil || reason != "automation_authorization_changed" {
				t.Fatal("unproven initial history permitted continuity", reason, err)
			}
		})
	}
}

func TestRecovery0030InitialReadRequiresAlternationAndMatchingFinalState(t *testing.T) {
	f := schedulingFixtureWithQueue(t, []int64{2})
	schedulingEnable(t, f, 0, true)
	schedulingGenerate(t, f, true)
	r := schedulingLatest(t, f)
	recovery0030Claim(t, f, r.ID)
	if err := f.e.RecoverReviewRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	recovery0030Claim(t, f, r.ID)
	r = selectionRead(t, f, r.ID)
	assertInitial := func(run Run, want bool) {
		t.Helper()
		err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			got, err := automaticSourceInitialRead(ctx, tx, run)
			if err != nil {
				return err
			}
			if got != want {
				t.Errorf("transition proof=%t want=%t", got, want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertInitial(r, true)
	wrongFinal := r
	wrongFinal.State = "queued"
	assertInitial(wrongFinal, false)
	admissionSQL(t, f, `UPDATE events SET payload_json='{"state":"queued","reason":""}' WHERE resource_id=? AND event_type='review.selection_updated' AND revision=2`, r.ID)
	assertInitial(r, false)
}
