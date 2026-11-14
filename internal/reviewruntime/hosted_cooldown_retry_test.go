package reviewruntime

import (
	"context"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHostedCooldownRetryComparisonPreservesStableSource(t *testing.T) {
	baseline := reviewexec.Anchor{RepositoryID: 901, Repository: "owner/repo", PullNumber: 2, CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("c", 40), ParentPullNumber: 1, BaseRef: "source-1", MergeBaseSHA: strings.Repeat("d", 40), PatchSHA256: strings.Repeat("e", 64), ObservedAt: "2026-10-30T00:00:00Z"}
	for field, mutate := range map[string]func(*reviewexec.Anchor){
		"repository identity": func(a *reviewexec.Anchor) { a.RepositoryID++ },
		"pull":                func(a *reviewexec.Anchor) { a.PullNumber++ },
		"candidate":           func(a *reviewexec.Anchor) { a.CandidateSHA = strings.Repeat("f", 40) },
		"base":                func(a *reviewexec.Anchor) { a.BaseSHA = strings.Repeat("f", 40) },
		"parent":              func(a *reviewexec.Anchor) { a.ParentSHA = strings.Repeat("f", 40) },
		"parent identity":     func(a *reviewexec.Anchor) { a.ParentPullNumber++ },
		"base branch":         func(a *reviewexec.Anchor) { a.BaseRef = "other" },
		"merge base":          func(a *reviewexec.Anchor) { a.MergeBaseSHA = strings.Repeat("f", 40) },
		"patch":               func(a *reviewexec.Anchor) { a.PatchSHA256 = strings.Repeat("f", 64) },
	} {
		t.Run(field, func(t *testing.T) {
			current := baseline
			mutate(&current)
			if sameHostedRetryComparison(baseline, current) {
				t.Fatal("changed stable comparison accepted")
			}
		})
	}
	current := baseline
	current.ObservedAt = "2026-10-30T00:05:00Z"
	current.Repository = "renamed-owner/renamed-repo"
	before := current
	if !sameHostedRetryComparison(baseline, current) || !reflect.DeepEqual(current, before) || baseline.Repository != "owner/repo" || baseline.ObservedAt != "2026-10-30T00:00:00Z" {
		t.Fatal("fresh observation or stable repository rename changed retained identity")
	}
}

func TestHostedCooldownRetentionRetriesOnlyStorageFailures(t *testing.T) {
	for _, code := range []string{"overloaded", "storage_unavailable", "review_held", "review_conflict", "validation_failed"} {
		t.Run(code, func(t *testing.T) {
			calls, waits := 0, 0
			failure := core.Fail(code, "fixture", 503)
			err := retryAllowanceRetention(t.Context(), func(ctx context.Context, duration time.Duration) error {
				waits++
				if duration != 200*time.Millisecond {
					t.Fatal("unexpected retry delay", duration)
				}
				return ctx.Err()
			}, func() error {
				calls++
				if calls == 1 {
					return failure
				}
				return nil
			})
			if code == "overloaded" || code == "storage_unavailable" {
				if err != nil || calls != 2 || waits != 1 {
					t.Fatal("temporary retention failure not reconciled", calls, waits, err)
				}
			} else if err != failure || calls != 1 || waits != 0 {
				t.Fatal("definitive refusal retried", calls, waits, err)
			}
		})
	}
	calls := 0
	failure := core.Fail("overloaded", "fixture", 503)
	err := retryAllowanceRetention(t.Context(), func(context.Context, time.Duration) error { return nil }, func() error { calls++; return failure })
	if calls != 3 || err != failure {
		t.Fatal("retention retry was not bounded", calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	calls = 0
	err = retryAllowanceRetention(ctx, func(context.Context, time.Duration) error { cancel(); return ctx.Err() }, func() error { calls++; return failure })
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatal("retry ignored cancellation", calls, err)
	}
}
