package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
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

// Every query and reply has a distinct provider identity; previous comments are
// still returned so the actual allowance resolver must attribute each attempt.
type cooldownRetryProvider struct{ *driverHostedProvider }

func (p *cooldownRetryProvider) PostIssueComment(ctx context.Context, repo string, number int64, body string) (reviewprovider.IssueComment, error) {
	comment, err := p.driverHostedProvider.PostIssueComment(ctx, repo, number, body)
	if err == nil && strings.HasPrefix(body, "@coderabbitai rate limit\n\n") {
		comment.ID = 1000 + int64(p.queries)*10
		if p.queries == 1 {
			// Model supported provider clock lag so the first command is
			// already historical when synthetic time advances without a
			// real cooldown sleep. Original retained timestamps never change.
			comment.CreatedAt = comment.CreatedAt.Add(-2 * time.Second)
			comment.UpdatedAt = comment.CreatedAt
		}
		p.comments[len(p.comments)-1] = comment
	}
	return comment, err
}

func (p *cooldownRetryProvider) IssueComments(ctx context.Context, repo string, number int64) ([]reviewprovider.IssueComment, error) {
	p.histories++
	if repo != "owner/repo" || number != p.targetPull() {
		p.t.Fatal("wrong retry query scope")
	}
	if len(p.comments) > 0 {
		last := p.comments[len(p.comments)-1]
		if strings.HasPrefix(last.Body, "@coderabbitai rate limit\n\n") {
			at := time.Now().UTC()
			p.comments = append(p.comments, reviewprovider.IssueComment{ID: last.ID + 1, Body: p.reply, Author: p.bot, CreatedAt: at, UpdatedAt: at})
		}
	}
	return append([]reviewprovider.IssueComment(nil), p.comments...), ctx.Err()
}

func newHostedCooldownRetryFixture(t *testing.T) (*hostedDriverFixture, *time.Time) {
	t.Helper()
	f := newHostedDriverFixture(t)
	now := time.Now().UTC()
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection(), reviewexec.WithExecutionClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	f.engine, f.hosted.engine = engine, engine
	f.hosted.provider = &cooldownRetryProvider{f.provider}
	f.provider.reply = strings.Replace(runtimeAllowanceAvailable, "Reviews are available now.", "More reviews will be available in 1 minute.", 1)
	return f, &now
}

func scheduleRuntimeCooldown(t *testing.T, f *hostedDriverFixture, operation reviewexec.Operation, attempt reviewexec.Attempt, config reviewexec.Configuration) reviewexec.Operation {
	t.Helper()
	_, err := f.hosted.Prepare(t.Context(), operation, config, attempt)
	var refusal *core.Error
	if !errors.As(err, &refusal) || refusal.Code != "review_held" || refusal.Details.(map[string]string)["reason"] != "hosted_allowance_cooldown" {
		t.Fatalf("actual query did not prove cooldown: %v (%#v)", err, refusal)
	}
	if _, err = f.engine.ScheduleHostedCooldown(t.Context(), f.project, attempt.ID, attempt.Revision); err != nil {
		t.Fatal("schedule proven cooldown", err)
	}
	op, err := f.engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), operation.ID)
	if err != nil || op.ID != operation.ID || op.State != "queued" || op.AttemptID != "" || op.Wait == nil || op.Wait.RetryNotBefore == "" || op.Wait.Paused || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("timer wait retained capacity or changed request", op, err)
	}
	return op
}

func dueRuntimeCooldown(t *testing.T, f *hostedDriverFixture, now *time.Time, op reviewexec.Operation) (reviewexec.Operation, reviewexec.Attempt, reviewexec.Configuration) {
	t.Helper()
	due, err := time.Parse(time.RFC3339Nano, op.Wait.RetryNotBefore)
	if err != nil {
		t.Fatal(err)
	}
	*now = due.Add(time.Millisecond)
	attempt, err := f.engine.StartAttempt(t.Context(), f.project, op.ID)
	if err != nil {
		t.Fatal("start due retry", err)
	}
	current, err := f.engine.ExecutionContext(t.Context(), f.project, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return current.Operation, attempt, current.Configuration
}

func TestHostedCooldownRetryFreshAttemptFreshQueryAndSingleDispatch(t *testing.T) {
	f, now := newHostedCooldownRetryFixture(t)
	op, first, config := f.startHosted(t)
	waiting := scheduleRuntimeCooldown(t, f, op, first, config)
	baseline, err := f.engine.RetryBaseline(t.Context(), f.project, op.ID)
	if err != nil || baseline == nil || waiting.Wait.BaselineAttemptID != first.ID {
		t.Fatal("original comparison missing", baseline, err)
	}
	originalQuery, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	originalObservations, err := f.engine.ReadHostedObservations(t.Context(), f.project, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	originalBytes, _ := json.Marshal(originalObservations)
	originalCaptures := make([]HostedCaptureRecord, len(originalObservations))
	for i, observation := range originalObservations {
		in := observation.Input
		originalCaptures[i], err = f.hostedCaptures.Read(t.Context(), first.ID, CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes})
		if err != nil {
			t.Fatal("read original immutable capture", err)
		}
	}
	op, second, config := dueRuntimeCooldown(t, f, now, waiting)
	if first.ID == second.ID || second.OperationID != first.OperationID {
		t.Fatal("retry reused attempt or created a child request")
	}
	// A new positive response is necessary: keep the first attempt's cooldown
	// comments intact, but let the new uniquely attributed query report capacity.
	f.provider.reply = runtimeAllowanceAvailable
	prepared, err := f.hosted.Prepare(t.Context(), op, config, second)
	if err != nil || f.provider.queries != 2 || f.provider.reviews != 0 || !sameHostedRetryComparison(*baseline, prepared.Anchor) {
		t.Fatal("retry did not independently query unchanged source", err, f.provider.queries)
	}
	secondQuery, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, second.ID)
	if err != nil || secondQuery.ID == originalQuery.ID || secondQuery.Comment.ID == originalQuery.Comment.ID {
		t.Fatal("fresh attempt reused old provider query", err)
	}
	second, err = f.engine.Admit(t.Context(), f.project, second.ID, second.Revision, prepared.Anchor, prepared.Intent)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.hosted.Dispatch(t.Context(), op, second, f.callback(t, &second))
	if err != nil || result.State != "completed" || f.provider.reviews != 1 {
		t.Fatal("fresh positive allowance did not dispatch exactly once", result, err)
	}
	if _, err = f.hosted.Dispatch(t.Context(), op, second, f.callback(t, &second)); err != nil || f.provider.reviews != 1 {
		t.Fatal("retry posted twice", err)
	}
	afterQuery, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, first.ID)
	if err != nil || !reflect.DeepEqual(originalQuery, afterQuery) {
		t.Fatal("retry changed original query", err)
	}
	afterObservations, err := f.engine.ReadHostedObservations(t.Context(), f.project, first.ID)
	afterBytes, _ := json.Marshal(afterObservations)
	if err != nil || string(afterBytes) != string(originalBytes) || hostedDriverJournal(t, f, first.ID) != "held" {
		t.Fatal("retry changed original attempt/capture journal", err)
	}
	for i, observation := range originalObservations {
		in := observation.Input
		capture, err := f.hostedCaptures.Read(t.Context(), first.ID, CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes})
		if err != nil || !reflect.DeepEqual(capture, originalCaptures[i]) {
			t.Fatal("retry changed original immutable capture bytes", err)
		}
	}
}

func TestHostedCooldownRetryRepeatedCooldownReleasesCapacityAndKeepsFirstBaseline(t *testing.T) {
	f, now := newHostedCooldownRetryFixture(t)
	op, first, config := f.startHosted(t)
	waiting := scheduleRuntimeCooldown(t, f, op, first, config)
	baseline, err := f.engine.RetryBaseline(t.Context(), f.project, op.ID)
	if err != nil || baseline == nil {
		t.Fatal(err)
	}
	op, second, config := dueRuntimeCooldown(t, f, now, waiting)
	f.provider.reply = strings.Replace(runtimeAllowanceAvailable, "Reviews are available now.", "More reviews will be available in 60 minutes.", 1)
	waiting = scheduleRuntimeCooldown(t, f, op, second, config)
	if waiting.Wait.BaselineAttemptID != first.ID || first.ID == second.ID || f.provider.queries != 2 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 2 || cliCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
		t.Fatal("repeated cooldown forked intent, reused attempt or replaced baseline", waiting)
	}
	after, err := f.engine.RetryBaseline(t.Context(), f.project, op.ID)
	if err != nil || !reflect.DeepEqual(baseline, after) {
		t.Fatal("repeated cooldown rebased original source", err)
	}
	if candidate, err := f.engine.StartAttempt(t.Context(), f.project, op.ID); err != nil || candidate.ID != "" || f.provider.queries != 2 || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("future timer consumed shared capacity or provider work", err)
	}
}

func TestHostedCooldownRetryChangedOrUnusableSourceStopsBeforeFreshQuery(t *testing.T) {
	for _, mode := range []string{"head", "base", "base branch", "closed", "unknown", "authorization", "authorization during preparation", "file policy", "channel closed", "provider active"} {
		t.Run(mode, func(t *testing.T) {
			f, now := newHostedCooldownRetryFixture(t)
			op, first, config := f.startHosted(t)
			waiting := scheduleRuntimeCooldown(t, f, op, first, config)
			op, attempt, config := dueRuntimeCooldown(t, f, now, waiting)
			pull := f.provider.pulls[2]
			switch mode {
			case "head":
				pull.HeadSHA = f.baseSHA
			case "base":
				pull.BaseSHA = f.headSHA
			case "base branch":
				// Both current provider facts are valid. The retained original
				// base branch still prevents quietly changing the retry target.
				pull.BaseRef = "release"
				f.provider.repository.DefaultBranch = "release"
			case "closed":
				pull.State = "closed"
			case "unknown":
				delete(f.provider.pulls, 2)
			case "authorization":
				if _, err := f.store.DB().Exec(`UPDATE grants SET can_write=0,can_manage=0 WHERE project_id=?`, f.project); err != nil {
					t.Fatal(err)
				}
			case "authorization during preparation":
				fetch := f.hosted.fetch
				f.hosted.fetch = func(ctx context.Context, id int64, repository, head, base, token string) error {
					if err := fetch(ctx, id, repository, head, base, token); err != nil {
						return err
					}
					_, err := f.store.DB().Exec(`UPDATE grants SET can_write=0,can_manage=0 WHERE project_id=?`, f.project)
					return err
				}
			case "file policy":
				one := int64(1)
				if _, err := f.engine.ConfigurePolicy(t.Context(), f.scope(t, f.admin, true), "retry-file-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &one, RequiredChannels: []string{"cli", "hosted"}, MaxReviewFiles: 1}); err != nil {
					t.Fatal(err)
				}
			case "channel closed":
				zero := int64(0)
				if _, err := f.engine.ConfigureRoundControl(t.Context(), f.scope(t, f.admin, true), "close-retry-channel", 2, "hosted", reviewexec.RoundControlRequest{ExpectedRevision: &zero, Action: "close"}); err != nil {
					t.Fatal(err)
				}
			case "provider active":
				f.provider.activity = "active"
			}
			if mode != "unknown" {
				f.provider.pulls[2] = pull
			}
			f.provider.reply = runtimeAllowanceAvailable
			prepared, err := f.hosted.Prepare(t.Context(), op, config, attempt)
			if err == nil || !reflect.DeepEqual(prepared, reviewexec.Preparation{}) || f.provider.queries != 1 || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_hosted_allowance_queries`) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("unusable retry source reached fresh allowance/provider action", err, f.provider.queries)
			}
			if mode == "base branch" {
				var refusal *core.Error
				if !errors.As(err, &refusal) || refusal.Details.(map[string]string)["reason"] != "source_changed" {
					t.Fatal("valid new comparison escaped original retry baseline", err)
				}
			}
			if mode == "unknown" {
				var refusal *core.Error
				if !errors.As(err, &refusal) || refusal.Code != "review_held" || refusal.Details.(map[string]string)["reason"] != "source_unavailable" {
					t.Fatal("unknown retry source lost explicit held recovery reason", err)
				}
			}
			if mode == "closed" {
				var refusal *core.Error
				if !errors.As(err, &refusal) || refusal.Details.(map[string]string)["reason"] != "target_changed" {
					t.Fatal("retry rewrote structured source refusal", err)
				}
			}
		})
	}
}

func TestHostedCooldownRetryDoesNotChangeInitialSourceFailure(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, attempt, config := f.startHosted(t)
	delete(f.provider.pulls, 2)
	_, err := f.hosted.Prepare(t.Context(), op, config, attempt)
	var refusal *core.Error
	if err == nil || errors.As(err, &refusal) || err.Error() != "synthetic missing scoped PR" || f.provider.queries != 0 || f.provider.reviews != 0 {
		t.Fatal("initial source lookup gained retry-only behavior", err)
	}
}
