package reviewruntime

import (
	"context"
	"testing"
	"time"
)

func TestHostedDriverRetriesAllowancePersistenceWithoutProviderReplay(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "persistent"}[permanent], func(t *testing.T) {
			f := newHostedDriverFixture(t)
			op, a, c := f.startHosted(t)
			if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_hosted_allowance BEFORE INSERT ON review_hosted_preflight BEGIN SELECT RAISE(ABORT,'synthetic storage interruption'); END`); err != nil {
				t.Fatal(err)
			}
			waits := 0
			f.hosted.wait = func(ctx context.Context, d time.Duration) error {
				waits++
				if d != 200*time.Millisecond {
					t.Fatalf("unexpected wait %v", d)
				}
				if !permanent {
					if _, err := f.store.DB().Exec(`DROP TRIGGER reject_hosted_allowance`); err != nil {
						t.Fatal(err)
					}
				}
				return ctx.Err()
			}
			_, err := f.hosted.Prepare(t.Context(), op, c, a)
			if permanent {
				if err == nil || waits != 2 {
					t.Fatalf("persistent failure err=%v waits=%d", err, waits)
				}
			} else if err != nil || waits != 1 {
				t.Fatalf("transient failure err=%v waits=%d", err, waits)
			}
			want := 1
			if permanent {
				want = 0
			}
			if got := cliCount(t, f.store, `SELECT count(*) FROM review_hosted_preflight`); got != want {
				t.Fatalf("preflight count=%d want%d", got, want)
			}
			if f.provider.queries != 1 || f.provider.reviews != 0 || f.provider.histories != 1 {
				t.Fatalf("provider replay queries=%d reviews=%d histories=%d", f.provider.queries, f.provider.reviews, f.provider.histories)
			}
		})
	}
}
