package reviewexec

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/githubhook"
)

func TestCIWatchCanceledCompletionRetainsLateSHAHintDemand(t *testing.T) {
	for _, mode := range []string{"single", "duplicate", "overflow", "no owners", "no hints"} {
		t.Run(mode, func(t *testing.T) {
			f, s, clock, _, retained, register := ciWatchDemandFixture(t)
			entry := retained.entries[0]
			original := entry.result
			wanted, version := entry.wanted, entry.version
			gone := register()
			claim, err := f.e.claimCIWatch(t.Context(), s.readers, s)
			if err != nil || claim == nil {
				t.Fatal("freshness-only owner did not start a read", claim, err)
			}
			s.unregisterCIWatch(gone)
			if claim.watch.ctx.Err() == nil {
				t.Fatal("departure did not cancel the undemanded read")
			}
			// Order is deliberate: cancellation, SHA-only hint, completion.
			if mode != "no hints" {
				hint := githubhook.Hint{RepositoryID: 801, HeadSHA: strings.Repeat("c", 40), CI: true}
				s.hintCIWatches(hint)
				if mode == "duplicate" || mode == "overflow" {
					for i := 0; i < 40; i++ {
						if mode == "overflow" {
							hint.HeadSHA = fmt.Sprintf("%040x", i+1)
						}
						s.hintCIWatches(hint)
					}
				}
				if len(entry.inflightSHAHints) == 0 || len(entry.inflightSHAHints) > 32 || entry.wanted != wanted {
					t.Fatal("fixture did not retain an unmatched bounded SHA hint")
				}
			}
			if mode == "no owners" {
				s.unregisterCIWatch(retained)
			}
			if err := f.e.observeCIWatch(t.Context(), *claim, s); err != nil {
				t.Fatal(err)
			}
			refresh := mode != "no owners" && mode != "no hints"
			want := wanted
			if refresh {
				want++
			}
			if entry.wanted != want || entry.inflight || len(entry.inflightSHAHints) != 0 || entry.inflightSHAOverflow || entry.version != version || !reflect.DeepEqual(entry.result, original) {
				t.Fatal("canceled completion lost bounded demand or replaced facts", entry.wanted, want, entry)
			}
			clock.Add(5 * time.Second)
			next, err := f.e.claimCIWatch(t.Context(), s.readers, s)
			if err != nil || (next != nil) != refresh {
				t.Fatal("retained hint did not schedule exactly the needed refresh", next, err)
			}
			if next != nil {
				if err := f.e.observeCIWatch(t.Context(), *next, s); err != nil {
					t.Fatal(err)
				}
				if entry.wanted != entry.done {
					t.Fatal("replacement collection did not consume retained demand")
				}
			}
		})
	}
}
