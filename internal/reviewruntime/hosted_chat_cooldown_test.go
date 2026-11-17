package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"os"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func TestHostedChatCooldownDriverRetainsAndBlocksFurtherQueries(t *testing.T) {
	f := newHostedDriverFixture(t)
	f.provider.reply = "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n### Rate Limit Exceeded\n\n`@synthetic-poster` have exceeded the limit for the number of chat messages per hour. Please wait **44 minutes and 5 seconds** before sending another message."
	op, a, c := f.startHosted(t)
	_, err := f.hosted.Prepare(t.Context(), op, c, a)
	var hold *core.Error
	if !errors.As(err, &hold) || hold.Details.(map[string]string)["reason"] != "hosted_chat_cooldown" {
		t.Fatal("chat limit not retained distinctly", err)
	}
	if _, err = f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, "held", "hosted_chat_cooldown", ""); err != nil {
		t.Fatal(err)
	}
	original, err := f.engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), op.ID)
	if err != nil || original.Allowance == nil || original.Allowance.Kind != "hosted_chat_cooldown" || original.Wait != nil {
		t.Fatal("chat projection lost/no automatic wait", original, err)
	}
	// A new explicit request is still held without another provider command.
	raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "chat-second-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.headSHA})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	next, err := f.engine.StartAttempt(t.Context(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.engine.ExecutionContext(t.Context(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.hosted.Prepare(t.Context(), current.Operation, current.Configuration, next)
	if !errors.As(err, &hold) || hold.Details.(map[string]string)["reason"] != "hosted_chat_cooldown" {
		t.Fatal("shared gate lost", err)
	}
	if f.provider.queries != 1 || f.provider.reviews != 0 || f.provider.histories != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_hosted_allowance_queries`) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_operation_waits`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("chat limit caused another command, observation, retry, or review credit")
	}
}

func TestHostedChatCooldownSupervisorDoesNotAutomaticallyRetry(t *testing.T) {
	f := newHostedDriverFixture(t)
	f.provider.reply = "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n### Rate Limit Exceeded\n\n`@synthetic-poster` have exceeded the limit for the number of chat messages per hour. Please wait **52 minutes and 17 seconds** before sending another message."
	raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "chat-supervisor-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.headSHA})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.engine.Start(ctx, map[string]reviewexec.Driver{f.capability.ID: f.hosted})
	if err != nil {
		t.Fatal(err)
	}
	result, terminal, err := f.engine.WaitOperation(t.Context(), f.scope(t, f.writer, false), receipt.ID, 5)
	cancel()
	workerErr := <-done
	if err != nil || workerErr != nil || !terminal || result.State != "held" || result.Reason != "hosted_chat_cooldown" || result.Wait != nil || result.Allowance == nil || result.Allowance.Kind != "hosted_chat_cooldown" {
		t.Fatal(result, terminal, err, workerErr)
	}
	if f.provider.queries != 1 || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_operation_waits`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("chat hold retained reservation or scheduled a retry")
	}
	if err = f.engine.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	replay, err := f.engine.ReadOperation(t.Context(), f.scope(t, f.writer, false), receipt.ID)
	if err != nil || !reflect.DeepEqual(result, replay) {
		t.Fatal("restart reopened or changed chat hold", err)
	}
}

// Two real drivers occupy the normal supervisor slots. The first completes
// positive preflight, then the sibling retains a chat throttle before its final
// action boundary. No synthetic mutation of production records is involved.
type chatThrottleBetweenPrepareAndDispatch struct {
	first, sibling      *HostedDriver
	prepared, throttled chan struct{}
}

func (d *chatThrottleBetweenPrepareAndDispatch) Prepare(ctx context.Context, o reviewexec.Operation, c reviewexec.Configuration, a reviewexec.Attempt) (reviewexec.Preparation, error) {
	if o.PullNumber == 2 {
		p, err := d.first.Prepare(ctx, o, c, a)
		close(d.prepared)
		if err != nil {
			return p, err
		}
		select {
		case <-ctx.Done():
			return p, ctx.Err()
		case <-d.throttled:
			return p, nil
		}
	}
	select {
	case <-ctx.Done():
		return reviewexec.Preparation{}, ctx.Err()
	case <-d.prepared:
	}
	p, err := d.sibling.Prepare(ctx, o, c, a)
	close(d.throttled)
	return p, err
}
func (d *chatThrottleBetweenPrepareAndDispatch) Dispatch(ctx context.Context, o reviewexec.Operation, a reviewexec.Attempt, record func(context.Context, string) error) (reviewexec.Result, error) {
	if o.PullNumber == 2 {
		return d.first.Dispatch(ctx, o, a, record)
	}
	return d.sibling.Dispatch(ctx, o, a, record)
}

func TestHostedChatCooldownFinalDispatchSupervisorPreservesReason(t *testing.T) {
	f := newHostedDriverFixture(t)
	local := handoffProject(t, f.cliEvidenceFixture, f.engine, "chat-sibling-project", f.capability.ID, "hosted", 4)
	cache, err := f.manager.CachePath(903)
	if err != nil {
		t.Fatal(err)
	}
	originalCache, err := f.manager.CachePath(901)
	if err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, t.TempDir(), "clone", "--bare", "--template=", originalCache, cache)
	if err := os.Chmod(cache, 0700); err != nil {
		t.Fatal(err)
	}
	pull := f.reader.pulls[4]
	pull.RepositoryID = 903
	reader := &cliDriverReader{repository: reviewprovider.Repository{ID: 903, FullName: "owner/repo4", DefaultBranch: "main"}, pulls: map[int64]reviewprovider.Pull{4: pull}, calls: map[int64]int{}}
	provider := &driverHostedProvider{cliDriverReader: reader, t: t, pullNumber: 4, actor: f.provider.actor, bot: f.provider.bot, activity: "idle", reply: "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n### Rate Limit Exceeded\n\n`@synthetic-poster` have exceeded the limit for the number of chat messages per hour. Please wait **44 minutes and 5 seconds** before sending another message."}
	sibling, err := NewHostedDriver(f.store, f.engine, f.capability, f.manager, f.hostedCaptures, &schedulerRepositoryProvider{provider})
	if err != nil {
		t.Fatal(err)
	}
	sibling.fetch = func(ctx context.Context, id int64, repository, _, _, token string) error {
		if id != 903 || repository != "owner/repo4" || token != f.capability.GitHubCredential() {
			return errors.New("wrong synthetic sibling identity")
		}
		return ctx.Err()
	}
	sibling.wait = f.hosted.wait
	var receipts []reviewexec.Receipt
	for _, number := range []int64{2, 4} {
		scope := f.scope(t, f.writer, false)
		if number == 4 {
			scope = local.scope(t, local.writer, false)
		}
		raw, err := f.engine.Enqueue(t.Context(), scope, fmt.Sprintf("chat-final-%d", number), reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: number, Channel: "hosted", HeadSHA: f.headSHA, AllowOutOfOrder: number == 4})
		if err != nil {
			t.Fatal(err)
		}
		var receipt reviewexec.Receipt
		if err = json.Unmarshal(raw, &receipt); err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, receipt)
	}
	driver := &chatThrottleBetweenPrepareAndDispatch{first: f.hosted, sibling: sibling, prepared: make(chan struct{}), throttled: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.engine.Start(ctx, map[string]reviewexec.Driver{f.capability.ID: driver})
	if err != nil {
		t.Fatal(err)
	}
	result, terminal, err := f.engine.WaitOperation(t.Context(), f.scope(t, f.writer, false), receipts[0].ID, 10)
	cancel()
	workerErr := <-done
	if err != nil || workerErr != nil || !terminal || result.State != "failed" || result.Reason != "hosted_chat_cooldown" || result.Wait != nil {
		t.Fatal("final chat refusal lost through dispatch/supervisor", result, terminal, err, workerErr)
	}
	if f.provider.queries != 1 || provider.queries != 1 || f.provider.reviews != 0 || provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_operation_waits`) != 0 {
		t.Fatal("shared final chat gate posted, retained reservations or created review/retry credit")
	}
}
