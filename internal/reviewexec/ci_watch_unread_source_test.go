package reviewexec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ciWatchFailingSourceReader struct {
	*ciWatchReader
	fail string
}

func (r *ciWatchFailingSourceReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	if r.fail == "repository" {
		return reviewprovider.Repository{}, errors.New("synthetic repository unavailable")
	}
	return r.ciWatchReader.Repository(ctx, id)
}
func (r *ciWatchFailingSourceReader) Pull(ctx context.Context, repo string, n int64) (reviewprovider.Pull, error) {
	if r.fail == "pull" {
		return reviewprovider.Pull{}, errors.New("synthetic pull unavailable")
	}
	return r.ciWatchReader.Pull(ctx, repo, n)
}

func TestCIWatchUnreadSupersessionSurvivesLaterSourceRefresh(t *testing.T) {
	for _, move := range []string{"head", "explicit base", "implicit base"} {
		for _, later := range []string{"repository", "pull", "restored"} {
			t.Run(move+"/"+later, func(t *testing.T) {
				f, s, clock, base, scope := ciWatchFixture(t)
				r := &ciWatchFailingSourceReader{ciWatchReader: base}
				s.readers["test-capability"] = r
				binding, err := f.e.ciWatchBinding(t.Context(), scope)
				if err != nil {
					t.Fatal(err)
				}
				target := CIWatchTarget{PullNumber: 42, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}
				if move == "implicit base" {
					target.BaseSHA = ""
				}
				sub, err := s.registerCIWatch(scope, binding, []CIWatchTarget{target})
				if err != nil {
					t.Fatal(err)
				}
				defer s.unregisterCIWatch(sub)
				if move == "implicit base" {
					watchTick(t, f, s)
					clock.Add(time.Minute)
				}
				moved := lifecyclePull(42)
				if move == "head" {
					moved.HeadSHA = strings.Repeat("c", 40)
				} else {
					moved.BaseSHA = strings.Repeat("c", 40)
				}
				base.pulls[42] = moved
				watchTick(t, f, s)
				// Do not read this subscriber's result: authorization may still be waiting.
				lateTarget := CIWatchTarget{PullNumber: 42, HeadSHA: moved.HeadSHA, BaseSHA: moved.BaseSHA}
				late, err := s.registerCIWatch(scope, binding, []CIWatchTarget{lateTarget})
				if err != nil {
					t.Fatal(err)
				}
				defer s.unregisterCIWatch(late)
				if got := s.ciWatchResult(late); got.State != "pending" || got.Targets[0].ObservedHeadSHA != "" {
					t.Fatal("cached mismatch leaked into new subscription", got)
				}
				r.fail = later
				if later == "restored" {
					r.fail = ""
					base.pulls[42] = lifecyclePull(42)
				}
				clock.Add(time.Minute)
				watchTick(t, f, s)
				got := s.ciWatchResult(sub)
				if got.State != "superseded" || got.Targets[0].ObservedHeadSHA != moved.HeadSHA || got.Targets[0].ObservedBaseSHA != moved.BaseSHA {
					t.Fatal("unread source movement was overwritten", got)
				}
				if later != "restored" && s.ciWatchResult(late).State != "pending" {
					t.Fatal("another subscriber inherited supersession")
				}
			})
		}
	}
}

func TestCIWatchZeroWaitUsesOnlyTwoAuthorizedReads(t *testing.T) {
	f, s, _, reader, scope := ciWatchFixture(t)
	// With no idle pool, every completed binding transaction closes its connection.
	// This counts real Store.Read transactions without timing or production hooks.
	f.store.DB().SetMaxIdleConns(0)
	before := f.store.DB().Stats().MaxIdleClosed
	got, err := f.e.WatchCI(t.Context(), scope, []CIWatchTarget{{PullNumber: 42, HeadSHA: strings.Repeat("a", 40)}}, 0)
	reads := f.store.DB().Stats().MaxIdleClosed - before
	if err != nil || got.State != "pending" || !got.TimedOut || reads != 2 || reader.calls != 0 || len(s.watchers) != 0 {
		t.Fatal("immediate snapshot added authorization or provider work", got, err, reads, reader.calls)
	}
}
