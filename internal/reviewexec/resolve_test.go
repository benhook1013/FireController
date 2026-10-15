package reviewexec_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type sourceReader struct {
	repository reviewprovider.Repository
	pulls      map[int64]reviewprovider.Pull
	calls      []string
	deadlines  []time.Time
	onCall     func(context.Context, string) error
}

var _ reviewexec.TargetReader = (*sourceReader)(nil)

func (r *sourceReader) record(ctx context.Context, call string) error {
	r.calls = append(r.calls, call)
	deadline, _ := ctx.Deadline()
	r.deadlines = append(r.deadlines, deadline)
	if r.onCall != nil {
		return r.onCall(ctx, call)
	}
	return nil
}

func (r *sourceReader) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	if err := r.record(ctx, fmt.Sprintf("repository:%d", id)); err != nil {
		return reviewprovider.Repository{}, err
	}
	return r.repository, nil
}

func (r *sourceReader) Pull(ctx context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	if err := r.record(ctx, fmt.Sprintf("pull:%s#%d", repository, number)); err != nil {
		return reviewprovider.Pull{}, err
	}
	pull, exists := r.pulls[number]
	if !exists {
		return reviewprovider.Pull{}, errors.New("unconfigured synthetic pull read")
	}
	return pull, nil
}

func sourceFixture() (*sourceReader, reviewexec.Configuration, reviewexec.Request) {
	reader := &sourceReader{
		repository: reviewprovider.Repository{ID: 701, FullName: "owner/canonical", DefaultBranch: "main"},
		pulls: map[int64]reviewprovider.Pull{9: {
			Number: 9, RepositoryID: 701, State: "open", HeadSHA: strings.Repeat("a", 40),
			BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 12,
		}},
	}
	configuration := reviewexec.Configuration{Mode: "independent", Queue: []int64{1, 9, 42}, Channels: []string{"cli", "hosted"}}
	request := reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 9, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
	return reader, configuration, request
}

func sourcePull(number int64, head string, merged bool) reviewprovider.Pull {
	state := "open"
	if merged {
		state = "closed"
	}
	return reviewprovider.Pull{Number: number, RepositoryID: 701, State: state, Merged: merged, HeadSHA: head, BaseSHA: strings.Repeat("c", 40), BaseRef: "main", ChangedFiles: 3}
}

func requireSourceCalls(t *testing.T, reader *sourceReader, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(reader.calls, want) {
		t.Fatalf("provider read scope: got %v want %v", reader.calls, want)
	}
}

func requireSourceError(t *testing.T, plan reviewexec.SourcePlan, err error, code, reason string) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != code || !reflect.DeepEqual(plan, reviewexec.SourcePlan{}) {
		t.Fatalf("hold/error returned partial plan or wrong error: %+v %v", plan, err)
	}
	if reason != "" {
		details, ok := failure.Details.(map[string]string)
		if !ok || details["reason"] != reason {
			t.Fatalf("source hold reason: %+v, want %s", failure.Details, reason)
		}
	}
}

func TestResolveSourceIndependentReadsOnlyAssertedTargetAndSharesFreshnessBudget(t *testing.T) {
	reader, configuration, request := sourceFixture()
	before := time.Now()
	plan, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
	after := time.Now()
	if err != nil || plan.Repository != reader.repository || plan.Pull != reader.pulls[9] || plan.ParentPullNumber != 0 || plan.ParentSHA != reader.pulls[9].BaseSHA {
		t.Fatalf("independent source plan: %+v %v", plan, err)
	}
	requireSourceCalls(t, reader, "repository:701", "pull:owner/canonical#9")
	observed, parseErr := time.Parse(time.RFC3339Nano, plan.ObservedAt)
	if parseErr != nil || observed.Before(before) || observed.After(after) {
		t.Fatal("source observation was not stamped during these reads")
	}
	for _, deadline := range reader.deadlines {
		if deadline.IsZero() || deadline.Before(before.Add(14*time.Second)) || deadline.After(after.Add(15*time.Second)) || !deadline.Equal(reader.deadlines[0]) {
			t.Fatal("source reads did not share one finite fifteen-second context")
		}
	}
	// No policy/front/allowance fields are supplied: this is source observation,
	// including a selected non-front target, rather than dispatch authorization.
	reader.calls, reader.deadlines = nil, nil
	if err := reviewexec.RecheckSource(context.Background(), reader, 701, configuration, request, plan); err != nil {
		t.Fatal(err)
	}
	requireSourceCalls(t, reader, "repository:701", "pull:owner/canonical#9")
}

func TestResolveSourceStackedNearestUnmergedAndDefaultBase(t *testing.T) {
	t.Run("nearest unmerged skips only merged predecessors", func(t *testing.T) {
		reader, configuration, request := sourceFixture()
		configuration.Mode, configuration.Queue = "stacked", []int64{1, 2, 3, 9, 42}
		reader.pulls[3] = sourcePull(3, strings.Repeat("d", 40), true)
		reader.pulls[2] = sourcePull(2, reader.pulls[9].BaseSHA, false)
		plan, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
		if err != nil || plan.ParentPullNumber != 2 || plan.ParentSHA != reader.pulls[2].HeadSHA {
			t.Fatalf("effective parent: %+v %v", plan, err)
		}
		requireSourceCalls(t, reader, "repository:701", "pull:owner/canonical#9", "pull:owner/canonical#3", "pull:owner/canonical#2")
	})
	t.Run("first unmerged uses default comparison", func(t *testing.T) {
		reader, configuration, request := sourceFixture()
		configuration.Mode, configuration.Queue = "stacked", []int64{1, 2, 9, 42}
		reader.pulls[1], reader.pulls[2] = sourcePull(1, strings.Repeat("c", 40), true), sourcePull(2, strings.Repeat("d", 40), true)
		plan, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
		if err != nil || plan.ParentPullNumber != 0 || plan.ParentSHA != reader.pulls[9].BaseSHA {
			t.Fatalf("default comparison: %+v %v", plan, err)
		}
		requireSourceCalls(t, reader, "repository:701", "pull:owner/canonical#9", "pull:owner/canonical#2", "pull:owner/canonical#1")
	})
	t.Run("closed unmerged is a hold with no fallback", func(t *testing.T) {
		reader, configuration, request := sourceFixture()
		configuration.Mode, configuration.Queue = "stacked", []int64{1, 2, 9, 42}
		parent := sourcePull(2, strings.Repeat("b", 40), false)
		parent.State = "closed"
		reader.pulls[2] = parent
		plan, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
		requireSourceError(t, plan, err, "review_held", "parent_unavailable")
		requireSourceCalls(t, reader, "repository:701", "pull:owner/canonical#9", "pull:owner/canonical#2")
	})
}

func TestResolveSourceRejectsInvalidInputsBeforeProviderDispatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*reviewexec.Configuration, *reviewexec.Request)
	}{
		{"empty queue", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Queue = nil }},
		{"oversized queue", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Queue = make([]int64, 101) }},
		{"duplicate queue", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Queue = []int64{9, 9} }},
		{"zero queue number", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Queue = []int64{0, 9} }},
		{"negative queue number", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Queue = []int64{-1, 9} }},
		{"oversized queue number", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Queue = []int64{2147483648, 9} }},
		{"invalid mode", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Mode = "unknown" }},
		{"paused", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Paused = true }},
		{"no channels", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Channels = nil }},
		{"invalid configured channel", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Channels = []string{"cli", "unknown"} }},
		{"duplicate configured channel", func(c *reviewexec.Configuration, _ *reviewexec.Request) { c.Channels = []string{"cli", "cli"} }},
		{"disabled requested channel", func(c *reviewexec.Configuration, r *reviewexec.Request) {
			c.Channels = []string{"hosted"}
			r.Channel = "cli"
		}},
		{"invalid requested channel", func(_ *reviewexec.Configuration, r *reviewexec.Request) { r.Channel = "manual" }},
		{"missing head", func(_ *reviewexec.Configuration, r *reviewexec.Request) { r.HeadSHA = "" }},
		{"noncanonical head", func(_ *reviewexec.Configuration, r *reviewexec.Request) { r.HeadSHA = strings.Repeat("A", 40) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, configuration, request := sourceFixture()
			test.change(&configuration, &request)
			plan, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
			requireSourceError(t, plan, err, "validation_failed", "")
			if len(reader.calls) != 0 {
				t.Fatal("invalid input dispatched provider reads")
			}
		})
	}
	for _, number := range []int64{0, -1, 2147483648, 42} {
		reader, configuration, request := sourceFixture()
		configuration.Queue = []int64{9}
		request.PullNumber = number
		plan, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
		requireSourceError(t, plan, err, "review_not_configured", "")
		if len(reader.calls) != 0 {
			t.Fatal("unconfigured assertion dispatched provider reads")
		}
	}
	reader, configuration, request := sourceFixture()
	for _, repositoryID := range []int64{0, -1} {
		plan, err := reviewexec.ResolveSource(context.Background(), reader, repositoryID, configuration, request)
		requireSourceError(t, plan, err, "validation_failed", "")
	}
	plan, err := reviewexec.ResolveSource(context.Background(), nil, 701, configuration, request)
	requireSourceError(t, plan, err, "validation_failed", "")
	if len(reader.calls) != 0 {
		t.Fatal("invalid repository dispatched provider reads")
	}
	configuration.Queue, request.PullNumber = []int64{2147483647}, 2147483647
	pull := reader.pulls[9]
	pull.Number = request.PullNumber
	reader.pulls[request.PullNumber] = pull
	if _, err = reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request); err != nil {
		t.Fatal("maximum valid PR number rejected", err)
	}
}

func TestResolveSourceHoldsIdentityLifecycleAndComparisonFailures(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		change       func(*sourceReader, *reviewexec.Configuration)
		calls        []string
	}{
		{"repository identity", "repository_changed", func(r *sourceReader, _ *reviewexec.Configuration) { r.repository.ID++ }, []string{"repository:701"}},
		{"repository name", "repository_changed", func(r *sourceReader, _ *reviewexec.Configuration) { r.repository.FullName = "bad/path/escape" }, []string{"repository:701"}},
		{"default branch unknown", "repository_changed", func(r *sourceReader, _ *reviewexec.Configuration) { r.repository.DefaultBranch = "" }, []string{"repository:701"}},
		{"target repository", "target_changed", func(r *sourceReader, _ *reviewexec.Configuration) { p := r.pulls[9]; p.RepositoryID++; r.pulls[9] = p }, nil},
		{"target number", "target_changed", func(r *sourceReader, _ *reviewexec.Configuration) { p := r.pulls[9]; p.Number = 1; r.pulls[9] = p }, nil},
		{"candidate moved", "target_changed", func(r *sourceReader, _ *reviewexec.Configuration) {
			p := r.pulls[9]
			p.HeadSHA = strings.Repeat("d", 40)
			r.pulls[9] = p
		}, nil},
		{"unknown base", "target_changed", func(r *sourceReader, _ *reviewexec.Configuration) { p := r.pulls[9]; p.BaseSHA = ""; r.pulls[9] = p }, nil},
		{"closed target", "target_changed", func(r *sourceReader, _ *reviewexec.Configuration) {
			p := r.pulls[9]
			p.State = "closed"
			r.pulls[9] = p
		}, nil},
		{"merged target", "target_changed", func(r *sourceReader, _ *reviewexec.Configuration) {
			p := r.pulls[9]
			p.State, p.Merged = "closed", true
			r.pulls[9] = p
		}, nil},
		{"zero change", "target_changed", func(r *sourceReader, _ *reviewexec.Configuration) {
			p := r.pulls[9]
			p.ChangedFiles = 0
			r.pulls[9] = p
		}, nil},
		{"default base moved", "default_base_changed", func(r *sourceReader, _ *reviewexec.Configuration) {
			p := r.pulls[9]
			p.BaseRef = "other"
			r.pulls[9] = p
		}, nil},
		{"parent repository", "parent_identity_changed", func(r *sourceReader, c *reviewexec.Configuration) {
			c.Mode, c.Queue = "stacked", []int64{1, 9}
			p := sourcePull(1, strings.Repeat("b", 40), false)
			p.RepositoryID++
			r.pulls[1] = p
		}, []string{"repository:701", "pull:owner/canonical#9", "pull:owner/canonical#1"}},
		{"parent moved", "parent_moved", func(r *sourceReader, c *reviewexec.Configuration) {
			c.Mode, c.Queue = "stacked", []int64{1, 9}
			r.pulls[1] = sourcePull(1, strings.Repeat("d", 40), false)
		}, []string{"repository:701", "pull:owner/canonical#9", "pull:owner/canonical#1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, configuration, request := sourceFixture()
			test.change(reader, &configuration)
			plan, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
			requireSourceError(t, plan, err, "review_held", test.reason)
			want := test.calls
			if want == nil {
				want = []string{"repository:701", "pull:owner/canonical#9"}
			}
			requireSourceCalls(t, reader, want...)
		})
	}
}

func TestResolveSourceProviderFailuresAndCancellationReturnNoPartialPlan(t *testing.T) {
	for _, failCall := range []string{"repository:701", "pull:owner/canonical#9", "pull:owner/canonical#1"} {
		t.Run(failCall, func(t *testing.T) {
			reader, configuration, request := sourceFixture()
			configuration.Mode, configuration.Queue = "stacked", []int64{1, 9}
			reader.pulls[1] = sourcePull(1, strings.Repeat("b", 40), false)
			sentinel := errors.New("synthetic provider failure")
			reader.onCall = func(_ context.Context, call string) error {
				if call == failCall {
					return sentinel
				}
				return nil
			}
			plan, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
			if !errors.Is(err, sentinel) || !reflect.DeepEqual(plan, reviewexec.SourcePlan{}) {
				t.Fatal("provider failure yielded a partial plan", plan, err)
			}
			if reader.calls[len(reader.calls)-1] != failCall {
				t.Fatal("resolution continued after provider failure")
			}
		})
	}
	for _, ignoreCancellation := range []bool{false, true} {
		t.Run(fmt.Sprintf("provider-ignores-cancel=%t", ignoreCancellation), func(t *testing.T) {
			reader, configuration, request := sourceFixture()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			reader.onCall = func(readCtx context.Context, call string) error {
				if call == "repository:701" {
					return nil
				}
				select {
				case <-readCtx.Done():
					if ignoreCancellation {
						return nil
					}
					return readCtx.Err()
				case <-time.After(250 * time.Millisecond):
					return errors.New("synthetic external bound reached")
				}
			}
			before := time.Now()
			plan, err := reviewexec.ResolveSource(ctx, reader, 701, configuration, request)
			if time.Since(before) > 500*time.Millisecond {
				t.Fatal("bounded provider wait did not return promptly")
			}
			if ignoreCancellation {
				requireSourceError(t, plan, err, "review_held", "observation_incomplete")
			} else if !errors.Is(err, context.DeadlineExceeded) || !reflect.DeepEqual(plan, reviewexec.SourcePlan{}) {
				t.Fatal("provider timeout yielded a partial plan", plan, err)
			}
			for _, deadline := range reader.deadlines {
				expected, _ := ctx.Deadline()
				if !deadline.Equal(expected) {
					t.Fatal("shorter parent deadline was extended")
				}
			}
		})
	}
}

func TestRecheckSourceDetectsFinalBoundaryMovement(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		change       func(*sourceReader, *reviewexec.Configuration)
	}{
		{"candidate moved", "target_changed", func(r *sourceReader, _ *reviewexec.Configuration) {
			p := r.pulls[9]
			p.HeadSHA = strings.Repeat("d", 40)
			r.pulls[9] = p
		}},
		{"base object moved", "comparison_changed", func(r *sourceReader, _ *reviewexec.Configuration) {
			p := r.pulls[9]
			p.BaseSHA = strings.Repeat("d", 40)
			r.pulls[9] = p
		}},
		{"repository renamed", "comparison_changed", func(r *sourceReader, _ *reviewexec.Configuration) { r.repository.FullName = "owner/renamed" }},
		{"default base renamed", "comparison_changed", func(r *sourceReader, _ *reviewexec.Configuration) {
			r.repository.DefaultBranch = "develop"
			p := r.pulls[9]
			p.BaseRef = "develop"
			r.pulls[9] = p
		}},
		{"target closed", "target_changed", func(r *sourceReader, _ *reviewexec.Configuration) {
			p := r.pulls[9]
			p.State = "closed"
			r.pulls[9] = p
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader, configuration, request := sourceFixture()
			prepared, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
			if err != nil {
				t.Fatal(err)
			}
			reader.calls = nil
			test.change(reader, &configuration)
			err = reviewexec.RecheckSource(context.Background(), reader, 701, configuration, request, prepared)
			requireSourceError(t, reviewexec.SourcePlan{}, err, "review_held", test.reason)
			if len(reader.calls) != 2 || reader.calls[0] != "repository:701" {
				t.Fatal("final boundary did not reread repository and selected target")
			}
		})
	}
	for _, aligned := range []bool{false, true} {
		t.Run(fmt.Sprintf("stacked-parent-and-base-align=%t", aligned), func(t *testing.T) {
			reader, configuration, request := sourceFixture()
			configuration.Mode, configuration.Queue = "stacked", []int64{1, 9}
			reader.pulls[1] = sourcePull(1, strings.Repeat("b", 40), false)
			prepared, err := reviewexec.ResolveSource(context.Background(), reader, 701, configuration, request)
			if err != nil {
				t.Fatal(err)
			}
			parent := reader.pulls[1]
			parent.HeadSHA = strings.Repeat("d", 40)
			reader.pulls[1] = parent
			if aligned {
				p := reader.pulls[9]
				p.BaseSHA = parent.HeadSHA
				reader.pulls[9] = p
			}
			reader.calls = nil
			err = reviewexec.RecheckSource(context.Background(), reader, 701, configuration, request, prepared)
			reason := "parent_moved"
			if aligned {
				reason = "comparison_changed"
			}
			requireSourceError(t, reviewexec.SourcePlan{}, err, "review_held", reason)
			requireSourceCalls(t, reader, "repository:701", "pull:owner/canonical#9", "pull:owner/canonical#1")
		})
	}
}
