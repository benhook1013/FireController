package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type driverHostedProvider struct {
	*cliDriverReader
	t                                                                *testing.T
	actor, bot                                                       reviewprovider.Author
	comments                                                         []reviewprovider.IssueComment
	queries, reviews, histories, baselines, observations, activities int
	delay                                                            int
	reply, activity                                                  string
	partial, lost, malformed                                         bool
	postHook                                                         func()
	alterReturn                                                      func(*reviewprovider.IssueComment)
	baselineHook                                                     func(context.Context, *reviewprovider.HostedPublicationSnapshot) error
}

func (p *driverHostedProvider) CurrentActor(ctx context.Context) (reviewprovider.Author, error) {
	return p.actor, ctx.Err()
}
func (p *driverHostedProvider) ObserveHostedActivity(ctx context.Context, repo, head string, selector reviewprovider.HostedActivitySelector) (reviewprovider.HostedActivity, error) {
	p.activities++
	if repo != "owner/repo" || head != p.pulls[2].HeadSHA || selector.ProviderActorID != p.bot.ID {
		p.t.Fatal("wrong activity scope")
	}
	return reviewprovider.HostedActivity{State: p.activity, ObservedAt: time.Now().UTC()}, ctx.Err()
}
func (p *driverHostedProvider) PostIssueComment(ctx context.Context, repo string, number int64, body string) (reviewprovider.IssueComment, error) {
	if repo != "owner/repo" || number != 2 {
		p.t.Fatal("wrong post scope")
	}
	if err := ctx.Err(); err != nil {
		return reviewprovider.IssueComment{}, err
	}
	id := int64(100)
	if strings.HasPrefix(body, "@coderabbitai rate limit\n\n") {
		p.queries++
	} else if strings.HasPrefix(body, "@coderabbitai full review\n\n") {
		p.reviews++
		id = 200
	} else {
		p.t.Fatal("unexpected command", body)
	}
	at := time.Now().UTC()
	comment := reviewprovider.IssueComment{ID: id, Body: body, Author: p.actor, CreatedAt: at, UpdatedAt: at}
	p.comments = append(p.comments, comment)
	if p.postHook != nil {
		p.postHook()
	}
	if id == 200 && p.lost {
		return reviewprovider.IssueComment{}, errors.New("synthetic response lost after post")
	}
	if id == 200 && p.malformed {
		comment.Body = "different command"
	}
	if id == 200 && p.alterReturn != nil {
		p.alterReturn(&comment)
	}
	return comment, nil
}
func (p *driverHostedProvider) IssueComments(ctx context.Context, repo string, number int64) ([]reviewprovider.IssueComment, error) {
	p.histories++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if repo != "owner/repo" || number != 2 {
		p.t.Fatal("wrong history scope")
	}
	if p.histories > p.delay && len(p.comments) == 1 {
		at := time.Now().UTC()
		p.comments = append(p.comments, reviewprovider.IssueComment{ID: 101, Body: p.reply, Author: p.bot, CreatedAt: at, UpdatedAt: at})
	}
	return append([]reviewprovider.IssueComment(nil), p.comments...), nil
}
func (p *driverHostedProvider) ObserveHosted(ctx context.Context, repo string, number int64) (reviewprovider.HostedPublicationSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return reviewprovider.HostedPublicationSnapshot{}, err
	}
	if repo != "owner/repo" || number != 2 {
		p.t.Fatal("wrong observation scope")
	}
	var trigger reviewprovider.IssueComment
	for _, comment := range p.comments {
		if comment.ID == 200 {
			trigger = comment
		}
	}
	if trigger.ID == 0 {
		p.baselines++
		snapshot := reviewprovider.HostedPublicationSnapshot{Complete: true, ObservedAt: time.Now().UTC(), IssueComments: append([]reviewprovider.IssueComment(nil), p.comments...), Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}}
		if p.baselineHook != nil {
			if err := p.baselineHook(ctx, &snapshot); err != nil {
				return snapshot, err
			}
		}
		return snapshot, nil
	}
	p.observations++
	at := time.Now().UTC()
	body := "**Actionable comments posted: 1**\n\n" + hostedRetainScope(p.pulls[2].BaseSHA, p.pulls[2].HeadSHA) + "\n\n" + hostedRetainPublicationMarker()
	if p.partial {
		body = "Provider is still collecting evidence."
	}
	line := int64(1)
	return reviewprovider.HostedPublicationSnapshot{Complete: true, ObservedAt: time.Now().UTC(), IssueComments: append([]reviewprovider.IssueComment(nil), p.comments...), Reviews: []reviewprovider.HostedReview{{ID: 300, Body: body, State: "commented", HeadSHA: p.pulls[2].HeadSHA, Author: p.bot, SubmittedAt: at}}, ReviewComments: []reviewprovider.ReviewComment{{ID: 301, ReviewID: 300, Body: hostedRetainInline(1, "Synthetic retained finding."), Author: p.bot, Path: "reviewed.txt", OriginalLine: &line, Side: "RIGHT", HeadSHA: p.pulls[2].HeadSHA, OriginalHeadSHA: p.pulls[2].HeadSHA, CreatedAt: at, UpdatedAt: at}}}, nil
}

type hostedDriverFixture struct {
	*cliDriverFixture
	hosted         *HostedDriver
	provider       *driverHostedProvider
	hostedCaptures *HostedCaptureStore
}

func newHostedDriverFixture(t *testing.T) *hostedDriverFixture {
	t.Helper()
	f := &hostedDriverFixture{cliDriverFixture: newCLIDriverFixture(t)}
	f.capability.Hosted = &HostedSettings{ProviderActorID: 20, ProviderLogin: "synthetic-provider", ProgressMode: "commit_status"}
	var err error
	f.capability.Fingerprint, err = capabilityFingerprint(f.capability)
	if err != nil {
		t.Fatal(err)
	}
	f.engine, err = reviewexec.New(f.store, []reviewexec.Capability{{ID: f.capability.ID, Provider: "coderabbit", Account: f.capability.Account, Channels: f.capability.Channels, Fingerprint: f.capability.Fingerprint}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	f.hostedCaptures, err = NewHostedCaptureStore(filepath.Join(t.TempDir(), "hosted-artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	f.provider = &driverHostedProvider{cliDriverReader: f.reader, t: t, actor: reviewprovider.Author{ID: 10, Login: "synthetic-poster", Kind: "User"}, bot: reviewprovider.Author{ID: 20, Login: "synthetic-provider", Kind: "Bot"}, reply: runtimeAllowanceAvailable, activity: "idle"}
	f.hosted, err = NewHostedDriver(f.store, f.engine, f.capability, f.manager, f.hostedCaptures, f.provider)
	if err != nil {
		t.Fatal(err)
	}
	f.hosted.fetch = f.driver.fetch
	f.hosted.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return f
}
func (f *hostedDriverFixture) startHosted(t *testing.T) (reviewexec.Operation, reviewexec.Attempt, reviewexec.Configuration) {
	t.Helper()
	raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "hosted-driver-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.headSHA})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	a, err := f.engine.StartAttempt(t.Context(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.engine.ExecutionContext(t.Context(), f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return current.Operation, a, current.Configuration
}
func (f *hostedDriverFixture) prepareHosted(t *testing.T) (reviewexec.Operation, reviewexec.Attempt) {
	t.Helper()
	op, a, c := f.startHosted(t)
	prepared, err := f.hosted.Prepare(t.Context(), op, c, a)
	if err != nil {
		t.Fatal("prepare", err)
	}
	if f.provider.queries != 1 || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("query created review effects")
	}
	a, err = f.engine.Admit(t.Context(), f.project, a.ID, a.Revision, prepared.Anchor, prepared.Intent)
	if err != nil {
		t.Fatal("admit", err)
	}
	return op, a
}
func (f *hostedDriverFixture) callback(t *testing.T, a *reviewexec.Attempt) func(context.Context, string) error {
	t.Helper()
	return func(ctx context.Context, id string) error {
		next, err := f.engine.Advance(ctx, f.project, a.ID, a.Revision, "active", "trigger_posted", id)
		if err == nil {
			*a = next
		}
		return err
	}
}
func hostedDriverJournal(t *testing.T, f *hostedDriverFixture, id string) string {
	t.Helper()
	var state string
	if err := f.store.DB().QueryRow(`SELECT state FROM review_execution_attempts WHERE id=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestHostedDriverQueryAdmitPublishThenSupervisorSettlement(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, a := f.prepareHosted(t)
	if hostedDriverJournal(t, f, a.ID) != "posting" {
		t.Fatal("admission state")
	}
	result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || result.State != "completed" || result.ExternalID != "200" || f.provider.queries != 1 || f.provider.reviews != 1 || f.provider.observations != 1 || f.provider.baselines != 1 {
		t.Fatal("happy path", result, err, f.provider.queries, f.provider.reviews, f.provider.observations)
	}
	if hostedDriverJournal(t, f, a.ID) != "active" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("driver settled supervisor-owned journal")
	}
	var evidenceID string
	if err := f.store.DB().QueryRow(`SELECT id FROM review_execution_evidence WHERE attempt_id=?`, a.ID).Scan(&evidenceID); err != nil {
		t.Fatal(err)
	}
	evidence, err := f.engine.ReadReviewEvidence(t.Context(), f.scopeWithAccess(t, f.readerToken(), false, "read"), evidenceID)
	if err != nil || evidence.Capture.Hosted == nil || len(evidence.Capture.Review.Findings) != 1 {
		t.Fatal("hosted capture lost", err)
	}
	var complete, finalized, found int
	if err := f.store.DB().QueryRow(`SELECT r.complete,r.finalized,r.found FROM review_runs r JOIN review_workspace w ON w.project_id=r.project_id AND w.snapshot_id=r.snapshot_id WHERE r.source_run_id=?`, evidenceID).Scan(&complete, &finalized, &found); err != nil || complete != 1 || finalized != 0 || found != 1 {
		t.Fatal("native source publication", complete, finalized, found, err)
	}
	repeated, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || repeated.State != "failed" || f.provider.reviews != 1 {
		t.Fatal("dispatch replay posted", repeated, err)
	}
	if _, err = f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, result.State, result.Reason, result.ExternalID); err != nil {
		t.Fatal(err)
	}
	if hostedDriverJournal(t, f, a.ID) != "completed" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("supervisor settlement failed")
	}
}
func (f *hostedDriverFixture) readerToken() string { return f.cliEvidenceFixture.reader }

func TestHostedDriverFinalGuardsPreventReviewPOST(t *testing.T) {
	for _, mode := range []string{"head", "base", "configuration", "policy", "revocation", "actor", "activity", "workspace", "permit-expired", "allowance-stale", "passed-intent", "passed-external", "passed-revision"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostedDriverFixture(t)
			op, a := f.prepareHosted(t)
			switch mode {
			case "head":
				p := f.provider.pulls[2]
				p.HeadSHA = strings.Repeat("9", 40)
				f.provider.pulls[2] = p
			case "base":
				p := f.provider.pulls[2]
				p.BaseSHA = strings.Repeat("9", 40)
				f.provider.pulls[2] = p
			case "configuration":
				f.configure(t, "independent", []int64{2, 4})
			case "policy":
				one := int64(1)
				_, err := f.engine.ConfigurePolicy(t.Context(), f.scope(t, f.admin, true), "pause-hosted", reviewexec.PolicyConfiguration{ExpectedRevision: &one, RequiredChannels: []string{"hosted"}, PausedChannels: []string{"hosted"}})
				if err != nil {
					t.Fatal(err)
				}
			case "revocation":
				if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE principal_id='runtime-evidence-writer-principal'`); err != nil {
					t.Fatal(err)
				}
			case "actor":
				f.provider.actor.ID++
			case "activity":
				f.provider.activity = "active"
			case "workspace":
				path := filepath.Join(f.hosted.prepared[a.ID].workspace.Directory, "reviewed.txt")
				if err := os.WriteFile(path, []byte("changed synthetic source\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "permit-expired":
				f.hosted.prepared[a.ID].expires = time.Now().Add(-time.Second)
			case "allowance-stale":
				staleHostedDriverAllowance(t, f, a)
			case "passed-intent":
				a.Intent = "@coderabbitai review --use-credits"
			case "passed-external":
				a.ExternalID = "999"
			case "passed-revision":
				a.Revision++
			}
			result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
			if err != nil || result.State != "failed" || result.Reason == "" || f.provider.reviews != 0 || f.provider.queries != 1 || hostedDriverJournal(t, f, a.ID) != "posting" {
				t.Fatal("final gate posted or advanced", result, err, f.provider.reviews)
			}
		})
	}
}

func TestHostedDriverActivityAndFileCeilingHoldBeforeQuery(t *testing.T) {
	for _, mode := range []string{"activity", "file-ceiling"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostedDriverFixture(t)
			if mode == "activity" {
				f.provider.activity = "active"
			} else {
				one := int64(1)
				_, err := f.engine.ConfigurePolicy(t.Context(), f.scope(t, f.admin, true), "file-ceiling", reviewexec.PolicyConfiguration{ExpectedRevision: &one, RequiredChannels: []string{"hosted"}, PausedChannels: []string{}, MaxReviewFiles: 1})
				if err != nil {
					t.Fatal(err)
				}
			}
			op, a, c := f.startHosted(t)
			_, err := f.hosted.Prepare(t.Context(), op, c, a)
			if err == nil || f.provider.queries != 0 || f.provider.reviews != 0 {
				t.Fatal("early hold consumed provider action", err)
			}
		})
	}
}

func TestHostedDriverLostPostRecoveryDoesNotRepost(t *testing.T) {
	f := newHostedDriverFixture(t)
	op, a := f.prepareHosted(t)
	f.provider.lost = true
	result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || result.State != "uncertain" || f.provider.reviews != 1 || hostedDriverJournal(t, f, a.ID) != "posting" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal(result, err)
	}
	if _, err = f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, "uncertain", result.Reason, ""); err != nil {
		t.Fatal(err)
	}
	recovered, err := RecoverHosted(t.Context(), f.provider, f.hostedCaptures, f.engine, f.project, a.ID, f.capability.ID, f.provider.actor, f.provider.bot)
	if err != nil || recovered.State != "completed" || recovered.TriggerCommentID != 200 || f.provider.reviews != 1 || f.provider.queries != 1 {
		t.Fatal("recovery reposted or lost evidence", recovered, err)
	}
}

func TestHostedDriverIncompleteObservationAndCancellationStayUncertain(t *testing.T) {
	for _, mode := range []string{"partial", "cancelled", "malformed-return"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostedDriverFixture(t)
			op, a := f.prepareHosted(t)
			f.provider.partial = mode == "partial"
			f.provider.malformed = mode == "malformed-return"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				f.provider.postHook = cancel
			}
			f.hosted.wait = func(context.Context, time.Duration) error { return context.Canceled }
			start := time.Now()
			result, err := f.hosted.Dispatch(ctx, op, a, f.callback(t, &a))
			if time.Since(start) > 3*time.Second || err != nil || result.State != "uncertain" || f.provider.reviews != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("partial execution gained completion", result, err)
			}
		})
	}
}

func TestHostedDriverDelayedAndUnavailableAllowanceNeverRepostsQuery(t *testing.T) {
	for _, mode := range []string{"delayed", "cooldown", "unknown", "unsupported", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostedDriverFixture(t)
			f.provider.delay = 1
			if mode == "cooldown" {
				f.provider.delay = 0
				f.provider.reply = strings.Replace(runtimeAllowanceAvailable, "Reviews are available now.", "More reviews will be available in 3 minutes.", 1)
			}
			if mode == "unknown" {
				f.provider.delay = 1000 // No attributed reply arrives in these eight passes.
			}
			if mode == "unsupported" {
				f.provider.delay = 0
				f.provider.reply = "Status is not available."
			}
			op, a, c := f.startHosted(t)
			waits := 0
			f.hosted.wait = func(ctx context.Context, d time.Duration) error {
				waits++
				if d != 2*time.Second {
					t.Fatal("wrong allowance wait")
				}
				if mode == "cancelled" {
					return context.Canceled
				}
				return ctx.Err()
			}
			_, err := f.hosted.Prepare(t.Context(), op, c, a)
			if (err == nil) != (mode == "delayed") || f.provider.queries != 1 || f.provider.reviews != 0 || hostedDriverJournal(t, f, a.ID) != "preparing" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("allowance loop result", mode, err, f.provider.queries, f.provider.histories, waits)
			}
			if mode == "delayed" && (f.provider.histories != 2 || waits != 1) {
				t.Fatal("delayed reply retried query or skipped observation")
			}
			if mode == "unknown" && (f.provider.histories != 8 || waits != 7) {
				t.Fatal("unknown allowance exceeded observation budget")
			}
			observations, readErr := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID)
			expectedObservations := f.provider.histories
			if mode == "delayed" {
				expectedObservations++ // One full history phase after allowance is available.
			}
			if f.provider.baselines != expectedObservations-f.provider.histories {
				t.Fatal("full baseline fetched during pending query or repeated", f.provider.observations)
			}
			if readErr != nil || len(observations) != expectedObservations {
				t.Fatal("query capture orphaned before interpretation", len(observations), f.provider.histories, readErr)
			}
			for _, observation := range observations {
				if observation.Input.Kind != "allowance_query" || observation.Input.Intent == "" {
					t.Fatal("query source misclassified")
				}
				reference := CaptureReference{ID: observation.Input.CaptureID, SHA256: observation.Input.CaptureSHA256, Bytes: observation.Input.CaptureBytes}
				retained, captureErr := f.hostedCaptures.Read(t.Context(), a.ID, reference)
				if captureErr != nil || !retained.Snapshot.Complete || len(retained.Snapshot.IssueComments) == 0 || retained.Snapshot.IssueComments[0].ID != 100 {
					t.Fatal("indexed query source not readable", captureErr)
				}
			}
			if _, stopErr := f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, "held", "allowance_fixture_stopped", ""); stopErr != nil {
				t.Fatal(stopErr)
			}
			eventCount := cliCount(t, f.store, `SELECT count(*) FROM events`)
			for _, observation := range observations {
				receipt, replayErr := f.engine.RecordHostedObservation(t.Context(), f.project, a.ID, observation.Input)
				if replayErr != nil || receipt.ID != observation.ID {
					t.Fatal("held query capture replay lost", replayErr)
				}
				reference := CaptureReference{ID: observation.Input.CaptureID, SHA256: observation.Input.CaptureSHA256, Bytes: observation.Input.CaptureBytes}
				if _, captureErr := f.hostedCaptures.Read(t.Context(), a.ID, reference); captureErr != nil {
					t.Fatal("held raw source unavailable", captureErr)
				}
			}
			if cliCount(t, f.store, `SELECT count(*) FROM events`) != eventCount || hostedDriverJournal(t, f, a.ID) != "held" || f.provider.queries != 1 || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("reading/replaying held query source changed execution or provider state")
			}
		})
	}
}

func staleHostedDriverAllowance(t *testing.T, f *hostedDriverFixture, a reviewexec.Attempt) {
	t.Helper()
	in, err := f.engine.ReadHostedAllowance(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	oldResponse := time.Now().Add(-2 * time.Minute).UTC()
	query, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	query.EarliestPosting = oldResponse.Add(-time.Minute).Format(time.RFC3339Nano)
	query.Comment.CreatedAt, query.Comment.UpdatedAt = oldResponse.Add(-time.Second), oldResponse.Add(-time.Second)
	encodedQuery, err := json.Marshal(query)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.DB().Exec(`UPDATE review_hosted_allowance_queries SET body_json=? WHERE attempt_id=?`, string(encodedQuery), a.ID); err != nil {
		t.Fatal(err)
	}
	in.Observation.QueryCreatedAt = query.Comment.CreatedAt
	in.Observation.ResponseCreatedAt = oldResponse
	changed, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	if err = f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_preflight WHERE attempt_id=?`, a.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	replacement := strings.Replace(body, string(old), string(changed), 1)
	if replacement == body {
		t.Fatal("fixture did not update its retained allowance input")
	}
	if _, err = f.store.DB().Exec(`UPDATE review_hosted_preflight SET body_json=? WHERE attempt_id=?`, replacement, a.ID); err != nil {
		t.Fatal(err)
	}
	// Confirm this remains a valid retained record, rather than a corrupt-body
	// refusal. The final action guard must reject the actual old response, even
	// though the retained fetch and freshly rechecked source remain current.
	retained, err := f.engine.ReadHostedAllowance(t.Context(), f.project, a.ID)
	if err != nil || !retained.Observation.ResponseCreatedAt.Equal(oldResponse) {
		t.Fatal("stale fixture invalid", err)
	}
	assertCoreErrorCode(t, f.engine.VerifyHostedAction(t.Context(), f.project, a.ID, a.Revision, f.hosted.prepared[a.ID].plan), "review_held")
}

func TestHostedDriverCallbackMustPersistTriggerBeforeCompletion(t *testing.T) {
	for _, mode := range []string{"nil", "noop", "error"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostedDriverFixture(t)
			op, a := f.prepareHosted(t)
			var callback func(context.Context, string) error
			if mode == "noop" {
				callback = func(context.Context, string) error { return nil }
			}
			if mode == "error" {
				callback = func(context.Context, string) error { return errors.New("synthetic durable write refused") }
			}
			result, err := f.hosted.Dispatch(t.Context(), op, a, callback)
			want, posts := "uncertain", 1
			if mode == "nil" {
				want, posts = "failed", 0
			}
			if err != nil || result.State != want || f.provider.reviews != posts || hostedDriverJournal(t, f, a.ID) != "posting" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
				t.Fatal("callback gained completion/adoption without persisted trigger", result, err)
			}
		})
	}
}

func TestHostedDriverConstructorRejectsMisboundResourcesAndCopiesSettings(t *testing.T) {
	f := newHostedDriverFixture(t)
	for _, mode := range []string{"missing-hosted", "wrong-channel", "different-binding", "nil-provider"} {
		t.Run(mode, func(t *testing.T) {
			c := f.capability
			settings := *c.Hosted
			c.Hosted = &settings
			provider := HostedProvider(f.provider)
			switch mode {
			case "missing-hosted":
				c.Hosted = nil
			case "wrong-channel":
				c.Channels = []string{"cli"}
			case "different-binding":
				c.Hosted.ProviderActorID++
				var err error
				c.Fingerprint, err = capabilityFingerprint(c)
				if err != nil {
					t.Fatal(err)
				}
			case "nil-provider":
				var p *driverHostedProvider
				provider = p
			}
			if _, err := NewHostedDriver(f.store, f.engine, c, f.manager, f.hostedCaptures, provider); err == nil {
				t.Fatal("misbound resource accepted")
			}
		})
	}
	f.capability.Hosted.ProviderActorID = 999
	f.capability.Channels[0] = "untrusted-change"
	if f.hosted.providerActor().ID != 20 || f.hosted.capability.Channels[0] == "untrusted-change" || f.provider.queries != 0 || f.provider.reviews != 0 {
		t.Fatal("constructor retained caller-owned mutable settings or activated provider")
	}
}

func TestHostedDriverInvalidPostReceiptNeverRecordsTrigger(t *testing.T) {
	for _, mode := range []string{"actor", "body", "future-time", "missing-id"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostedDriverFixture(t)
			op, a := f.prepareHosted(t)
			f.provider.alterReturn = func(comment *reviewprovider.IssueComment) {
				switch mode {
				case "actor":
					comment.Author.ID++
				case "body":
					comment.Body = "@coderabbitai review"
				case "future-time":
					comment.CreatedAt = time.Now().Add(time.Minute).UTC()
					comment.UpdatedAt = comment.CreatedAt
				case "missing-id":
					comment.ID = 0
				}
			}
			callbacks := 0
			result, err := f.hosted.Dispatch(t.Context(), op, a, func(context.Context, string) error { callbacks++; return nil })
			if err != nil || result.State != "uncertain" || callbacks != 0 || f.provider.reviews != 1 || hostedDriverJournal(t, f, a.ID) != "posting" || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("invalid returned receipt gained trigger identity", mode, result, err, callbacks)
			}
		})
	}
}
