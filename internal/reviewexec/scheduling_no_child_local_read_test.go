package reviewexec

import "testing"

func TestAutomaticNoChildLocalReadDoesNotSettleResumedPull(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume bool
	}{{"unchanged", false}, {"resumed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := schedulingFixtureWithQueue(t, []int64{2, 4})
			schedulingPause(t, f, 2, 0, true)
			schedulingEnable(t, f, 0, true)
			schedulingGenerate(t, f, true)
			old := schedulingLatest(t, f)
			admissionSQL(t, f, `UPDATE review_selections SET state='selecting',revision=2 WHERE id=?`, old.ID)
			old = selectionRead(t, f, old.ID)

			// Inject the bounded-read exhaustion at its delivery boundary.
			choice := runChoice{state: "held", reason: "local_read_unavailable", automaticBasisRevision: 1}
			if tc.resume {
				schedulingPause(t, f, 2, 1, false)
			}
			if worked, err := f.e.finishRunChoice(t.Context(), old, 801, nil, choice); err != nil || !worked {
				t.Fatalf("hold did not finish: worked=%t err=%v", worked, err)
			}
			got := selectionRead(t, f, old.ID)
			var settled string
			if err := f.store.DB().QueryRow(`SELECT settled_basis_sha256 FROM review_automatic_selections WHERE selection_id=?`, old.ID).Scan(&settled); err != nil {
				t.Fatal(err)
			}
			if got.State != "held" || got.Reason != "local_read_unavailable" || got.OperationID != "" || settled != "" {
				t.Fatalf("expected diagnostic-only hold, got %+v, settlement %q", got, settled)
			}
			schedulingGenerate(t, f, tc.resume)
			if tc.resume {
				fresh := schedulingLatest(t, f)
				if fresh.ID == old.ID {
					t.Fatal("resume did not permit a fresh selection")
				}
				selectionNext(t, f, &refreshTestReader{})
				got := selectionRead(t, f, fresh.ID)
				if got.State != "selected" || got.OperationID == "" {
					t.Fatalf("fresh selection failed: %+v", got)
				}
				if op := cliWaitRead(t, f, got.OperationID); op.PullNumber != 2 {
					t.Fatalf("fresh selection did not choose resumed PR: %+v", op)
				}
			}
		})
	}
}
