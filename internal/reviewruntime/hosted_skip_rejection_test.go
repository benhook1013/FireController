package reviewruntime

import (
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func runtimeSkippedCommandReply() string {
	return "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("c", 64) + " -->\n<details>\n<summary>Action not completed</summary>\n<dl>\n<dd>\nReview skipped.\n</dd>\n</dl>\n</details>"
}

func TestHostedSkippedTriggerRecoverySettlesOriginalOnceWithoutCredit(t *testing.T) {
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
			reader.record.Snapshot.IssueComments = []reviewprovider.IssueComment{trigger, {ID: 5002, Author: reader.record.ProviderActor, CreatedAt: at, UpdatedAt: at, Body: runtimeSkippedCommandReply()}}
			got, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil || got.State != "failed" || got.Reason != "hosted_review_skipped" || got.EvidenceID != "" || got.TriggerCommentID != trigger.ID {
				t.Fatal("proven original command skip remained pending or claimed quota/completion", got, err)
			}
			if recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f, "review_execution_evidence") != 0 || recoveryRuntimeCount(t, f, "review_hosted_observations") != 1 || recoveryRuntimeCount(t, f, "review_round_completions") != 0 {
				t.Fatal("skip lost its observation, retained a reservation or gained review credit")
			}
			beforeEvents, beforeCalls := recoveryRuntimeCount(t, f, "events"), len(reader.calls)
			replay, err := recoverRuntime(t, f, captures, a, reader)
			if err != nil || replay.State != "failed" || replay.Reason != got.Reason || len(reader.calls) != beforeCalls || recoveryRuntimeCount(t, f, "events") != beforeEvents {
				t.Fatal("terminal skip replay collected or settled again", replay, err)
			}
			if _, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID); err != nil {
				t.Fatal("skip lost retained source access", err)
			}
		})
	}
}

func TestHostedSkippedTriggerLiveDriverDefersSettlementToSupervisor(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, a := f.prepareHosted(t)
	f.hosted.provider = &rejectingHostedProvider{driverHostedProvider: f.provider, alterSnapshot: func(snapshot *reviewprovider.HostedPublicationSnapshot) {
		at := time.Now().UTC()
		snapshot.Reviews, snapshot.ReviewComments = []reviewprovider.HostedReview{}, []reviewprovider.ReviewComment{}
		snapshot.IssueComments = append(snapshot.IssueComments, reviewprovider.IssueComment{ID: 5002, Author: f.provider.bot, CreatedAt: at, UpdatedAt: at, Body: runtimeSkippedCommandReply()})
		snapshot.ObservedAt = at
	}}
	result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || result.State != "failed" || result.Reason != "hosted_review_skipped" || f.provider.reviews != 1 || f.provider.queries != 1 || f.provider.observations != 1 {
		t.Fatal("proven command skip not returned to original supervisor", result, err)
	}
	if hostedDriverJournal(t, f, a.ID) != "active" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("live observation settled supervisor-owned attempt")
	}
	if _, err := f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, result.State, result.Reason, result.ExternalID); err != nil {
		t.Fatal(err)
	}
	if cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 {
		t.Fatal("skipped trigger retained reservation or gained completion credit")
	}
}

func TestHostedSkippedReplyRetainsQualifiedBotRefreshBoundary(t *testing.T) {
	var old reviewprovider.IssueComment
	f, record := runtimeStandingFixture(t, func(b *HostedCaptureRecord) {
		at := b.Snapshot.ObservedAt.Add(-time.Hour)
		old = reviewprovider.IssueComment{ID: 8001, Author: reviewprovider.Author{ID: 8002, Login: "ci-report[bot]", Kind: "Bot"}, CreatedAt: at, UpdatedAt: at, Body: "CI is running"}
		b.Snapshot.IssueComments = append(b.Snapshot.IssueComments, old)
	}, nil)
	trigger, reply := record.Snapshot.IssueComments[0], record.Snapshot.IssueComments[2]
	trigger.CreatedAt = trigger.CreatedAt.Add(time.Second)
	trigger.UpdatedAt = trigger.CreatedAt
	reply.CreatedAt = reply.CreatedAt.Add(time.Second)
	reply.UpdatedAt, reply.Body = reply.CreatedAt, runtimeSkippedCommandReply()
	old.Body, old.UpdatedAt = "CI finished", record.Snapshot.ObservedAt
	record.Snapshot.IssueComments = []reviewprovider.IssueComment{trigger, reply, old}
	result, err := RetainHosted(t.Context(), f.captures, f.f.engine, f.a, record)
	if err != nil || result.rejectionState != "failed" || result.Complete || result.Reason != "hosted_review_skipped" || result.Receipt.ID != "" {
		t.Fatal("qualified unrelated bot update hid command skip or manufactured review credit", result, err)
	}
}
