package reviewexec

import "testing"

func TestPartialTerminalFindingsRemainObligations(t *testing.T) {
	for _, terminal := range []string{"failed", "cancelled", "rate_limited"} {
		t.Run(terminal, func(t *testing.T) {
			f, a, id, plan := coverageFixture(t, 1, true)
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, terminal, "capture_retained", ""); err != nil {
				t.Fatal(err)
			}
			check := func(want bool) {
				t.Helper()
				got, err := f.e.Comparison(t.Context(), attemptProject, plan, "cli")
				if err != nil || got.Pending != want || got.Covered {
					t.Fatalf("pending=%v covered=%v want pending=%v err=%v", got.Pending, got.Covered, want, err)
				}
			}
			check(true)
			frame, err := f.e.readRoundProgress(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), []int64{2}, roundProgressView{stages: true, fixes: true})
			if err != nil {
				t.Fatal(err)
			}
			cli := frame.Items[0].Channels[1]
			if !cli.RetainedAdjudicationPending || cli.Pending != 0 || cli.Used != 0 {
				t.Fatalf("partial work changed round occupancy/credit: %+v", cli)
			}
			undecided, fixes, err := retainedCLIObligations(t.Context(), f.store.DB(), attemptProject, 2)
			if err != nil || !undecided || fixes {
				t.Fatalf("retained undecided=%v fixes=%v err=%v", undecided, fixes, err)
			}
			adjudicationDecide(t, f, id, "partial-terminal-accept", 0, 0, "accepted")
			check(true)
			undecided, fixes, err = retainedCLIObligations(t.Context(), f.store.DB(), attemptProject, 2)
			if err != nil || undecided || !fixes {
				t.Fatalf("accepted undecided=%v fixes=%v err=%v", undecided, fixes, err)
			}
			coverageFix(t, f, id, "partial-terminal-fix", 1, 0, "")
			check(false)
			undecided, fixes, err = retainedCLIObligations(t.Context(), f.store.DB(), attemptProject, 2)
			if err != nil || undecided || fixes {
				t.Fatalf("settled undecided=%v fixes=%v err=%v", undecided, fixes, err)
			}
		})
	}
}
