package reviewruntime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type rejectingHostedProvider struct {
	*driverHostedProvider
	alterSnapshot func(*reviewprovider.HostedPublicationSnapshot)
}

func (p *rejectingHostedProvider) ObserveHosted(ctx context.Context, repo string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	snapshot, err := p.driverHostedProvider.ObserveHosted(ctx, repo, number)
	if err == nil {
		p.alterSnapshot(&snapshot)
	}
	return snapshot, err
}

func TestHostedRejectedTriggerSettlesOnceWithoutReviewCredit(t *testing.T) {
	for _, state := range []string{"active", "uncertain", "posting"} {
		t.Run(state, func(t *testing.T) {
			f, captures, a, reader := recoveryRuntimeFixture(t, state)
			var trigger reviewprovider.IssueComment
			for _, comment := range reader.record.Snapshot.IssueComments {
				if comment.ID == 100 {
					trigger = comment
				}
			}
			at := time.Now().UTC()
			reader.record.Snapshot.Reviews = []reviewprovider.HostedReview{}
			reader.record.Snapshot.ReviewComments = []reviewprovider.ReviewComment{}
			reader.record.Snapshot.IssueComments = []reviewprovider.IssueComment{trigger, {
				ID: 5002, Author: reader.record.ProviderActor, CreatedAt: at, UpdatedAt: at,
				Body: "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>⚠️ Action not completed</summary>\n<dl>\n<dd>\nReview rate limited.\n</dd>\n</dl>\n</details>",
			}}
			got, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil || got.State != "rate_limited" || got.Reason != "hosted_review_rate_limited" || got.EvidenceID != "" || got.TriggerCommentID != trigger.ID {
				t.Fatal("original rejection did not settle", got, err)
			}
			if recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f, "review_hosted_observations") != 1 || recoveryRuntimeCount(t, f, "review_round_completions") != 0 {
				t.Fatal("rejection lost raw proof, retained reservation or gained review credit")
			}
			beforeEvents, beforeCalls := recoveryRuntimeCount(t, f, "events"), len(reader.calls)
			replay, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil || replay.State != "rate_limited" || replay.Reason != got.Reason || len(reader.calls) != beforeCalls || recoveryRuntimeCount(t, f, "events") != beforeEvents {
				t.Fatal("terminal recovery replay fetched or settled again", replay, err)
			}
			if _, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID); err != nil {
				t.Fatal("terminal rejection lost retained observation access", err)
			}
		})
	}
}

func TestHostedDriverRateLimitedReplyLeavesSettlementToSupervisor(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, a := f.prepareHosted(t)
	f.hosted.provider = &rejectingHostedProvider{driverHostedProvider: f.provider, alterSnapshot: func(snapshot *reviewprovider.HostedPublicationSnapshot) {
		at := time.Now().UTC()
		snapshot.Reviews, snapshot.ReviewComments = []reviewprovider.HostedReview{}, []reviewprovider.ReviewComment{}
		snapshot.IssueComments = append(snapshot.IssueComments, reviewprovider.IssueComment{ID: 5002, Author: f.provider.bot, CreatedAt: at, UpdatedAt: at,
			Body: "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>⚠️ Action not completed</summary>\n<dl>\n<dd>\nReview rate limited.\n</dd>\n</dl>\n</details>"})
		snapshot.ObservedAt = at
	}}
	result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || result.State != "rate_limited" || result.Reason != "hosted_review_rate_limited" || f.provider.reviews != 1 || f.provider.queries != 1 || f.provider.observations != 1 {
		t.Fatal("issued rejection was not returned to supervisor", result, err)
	}
	if hostedDriverJournal(t, f, a.ID) != "active" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("live driver settled supervisor-owned attempt")
	}
	if _, err = f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, result.State, result.Reason, result.ExternalID); err != nil {
		t.Fatal("supervisor settlement", err)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 {
		t.Fatal("rejected trigger retained reservation or gained credit")
	}
}

func TestHostedRateLimitedRecoveryUsesRetainedPreTriggerBotBaseline(t *testing.T) {
	var old reviewprovider.IssueComment
	f, record := runtimeStandingFixture(t, func(b *HostedCaptureRecord) {
		at := b.Snapshot.ObservedAt.Add(-time.Hour)
		old = reviewprovider.IssueComment{ID: 8001, Author: reviewprovider.Author{ID: 8002, Login: "ci-report[bot]", Kind: "Bot"}, CreatedAt: at, UpdatedAt: at, Body: "CI is running"}
		b.Snapshot.IssueComments = append(b.Snapshot.IssueComments, old)
	}, nil)
	trigger, reply := record.Snapshot.IssueComments[0], record.Snapshot.IssueComments[2]
	// The GitHub command is created in the following second, so the captured
	// baseline is provably before it despite GitHub's second precision.
	trigger.CreatedAt = trigger.CreatedAt.Add(time.Second)
	trigger.UpdatedAt = trigger.CreatedAt
	reply.CreatedAt = reply.CreatedAt.Add(time.Second)
	reply.UpdatedAt = reply.CreatedAt
	reply.Body = strings.Replace(reply.Body, "✅ Action performed", "⚠️ Action not completed", 1)
	reply.Body = strings.Replace(reply.Body, "Full review finished.", "Review rate limited.", 1)
	old.Body, old.UpdatedAt = "CI finished", record.Snapshot.ObservedAt
	record.Snapshot.IssueComments = []reviewprovider.IssueComment{trigger, reply, old}
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || !result.rateLimited || result.Complete || result.Reason != "hosted_review_rate_limited" || result.Receipt.ID != "" {
		t.Fatal("qualified bot update hid rejection or manufactured completion", result, err)
	}
}
