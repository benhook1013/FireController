package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const runtimeAllowanceAvailable = "<!-- This is an auto-generated reply by CodeRabbit -->\nYour [plan](https://docs.coderabbit.ai/management/plans#fair-usage-limits-policy) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). Reviews are available now."

type runtimeAllowanceProvider struct {
	t          *testing.T
	body       string
	replyBody  string
	poster     reviewprovider.Author
	bot        reviewprovider.Author
	comments   []reviewprovider.IssueComment
	posts      int
	reads      int
	postError  error
	historyErr error
	dropPost   bool
}

func (p *runtimeAllowanceProvider) PostIssueComment(_ context.Context, repository string, number int64, body string) (reviewprovider.IssueComment, error) {
	p.t.Helper()
	p.posts++
	if repository != "renamed/project" || number != 2 || body != p.body || !strings.HasPrefix(body, "@coderabbitai rate limit\n\n") || strings.Contains(body, "full review") {
		p.t.Fatal("unexpected provider target or non-query POST", repository, number, body)
	}
	at := time.Now().UTC()
	query := reviewprovider.IssueComment{ID: 100, Body: body, Author: p.poster, CreatedAt: at, UpdatedAt: at}
	if !p.dropPost {
		// Nanoseconds keep the causal ordering strict without sleeping or placing
		// a reply outside the subsequent real observation window.
		replyAt := at.Add(time.Nanosecond)
		replyBody := p.replyBody
		if replyBody == "" {
			replyBody = runtimeAllowanceAvailable
		}
		p.comments = []reviewprovider.IssueComment{query, {ID: 101, Body: replyBody, Author: p.bot, CreatedAt: replyAt, UpdatedAt: replyAt}}
	}
	if p.postError != nil {
		return reviewprovider.IssueComment{}, p.postError
	}
	return query, nil
}

func (p *runtimeAllowanceProvider) IssueComments(_ context.Context, repository string, number int64) ([]reviewprovider.IssueComment, error) {
	p.t.Helper()
	p.reads++
	if repository != "renamed/project" || number != 2 {
		p.t.Fatal("unexpected history target", repository, number)
	}
	return append([]reviewprovider.IssueComment(nil), p.comments...), p.historyErr
}

type runtimeAllowanceFixture struct {
	f        *cliEvidenceFixture
	a        reviewexec.Attempt
	anchor   reviewexec.Anchor
	repo     reviewprovider.Repository
	captures *HostedCaptureStore
	provider *runtimeAllowanceProvider
}

func newRuntimeAllowanceFixture(t *testing.T) *runtimeAllowanceFixture {
	t.Helper()
	f := newCLIEvidenceFixture(t)
	raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "runtime-hosted-allowance", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.headSHA})
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
	anchor := reviewexec.Anchor{RepositoryID: 901, Repository: "owner/repo", PullNumber: 2, CandidateSHA: f.headSHA, BaseSHA: f.baseSHA, ParentSHA: f.baseSHA, MergeBaseSHA: f.baseSHA, PatchSHA256: strings.Repeat("e", 64), BaseRef: "main", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	body, err := reviewprovider.HostedAllowanceQueryBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: f.headSHA})
	if err != nil {
		t.Fatal(err)
	}
	captures, _ := newHostedTestStore(t)
	p := &runtimeAllowanceProvider{t: t, body: body, poster: reviewprovider.Author{ID: 10, Login: "synthetic-poster", Kind: "User"}, bot: reviewprovider.Author{ID: 20, Login: "synthetic-provider", Kind: "Bot"}}
	return &runtimeAllowanceFixture{f: f, a: a, anchor: anchor, repo: reviewprovider.Repository{ID: 901, FullName: "renamed/project", DefaultBranch: "main"}, captures: captures, provider: p}
}

func (f *runtimeAllowanceFixture) observe(t *testing.T) (ObservedHostedAllowance, error) {
	t.Helper()
	return ObserveHostedAllowance(t.Context(), f.provider, f.captures, f.f.engine, f.a, f.anchor, f.repo, f.provider.poster, f.provider.bot)
}

func (f *runtimeAllowanceFixture) assertNotAdmitted(t *testing.T) {
	t.Helper()
	var state, anchor, intent, external string
	var revision int64
	if err := f.f.store.DB().QueryRow(`SELECT state,revision,anchor_json,intent,external_id FROM review_execution_attempts WHERE id=?`, f.a.ID).Scan(&state, &revision, &anchor, &intent, &external); err != nil {
		t.Fatal(err)
	}
	if state != "preparing" || revision != 1 || anchor != "null" || intent != "" || external != "" {
		t.Fatal("metadata query admitted or advanced review", state, revision, anchor, intent, external)
	}
	for table, want := range map[string]int{"review_execution_reservations": 1, "review_execution_transitions": 1, "review_execution_evidence": 0} {
		var count int
		if err := f.f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatal("query changed review lifecycle", table, count, want, err)
		}
	}
}

func (f *runtimeAllowanceFixture) assertAvailableSource(t *testing.T, got ObservedHostedAllowance) reviewexec.HostedAllowanceQuery {
	t.Helper()
	if got.QueryID == "" || got.Capture.ID == "" || got.Reason != "provider_available_observed" || got.Observation.State != "available_observed" || got.Observation.QueryCommentID != 100 || got.Observation.ResponseCommentID != 101 || !got.Observation.RetryNotBefore.IsZero() {
		t.Fatalf("positive observation/source missing or invented reset: %+v", got)
	}
	query, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
	if err != nil || query.ID != got.QueryID || query.Revision != 2 || query.Comment == nil || *query.Comment != f.provider.comments[0] || query.Anchor != f.anchor || query.Intent != f.provider.body {
		t.Fatal("query not durably confirmed against original anchor", query, err)
	}
	source, err := f.captures.Read(t.Context(), f.a.ID, got.Capture)
	if err != nil || !source.Snapshot.Complete || !reflect.DeepEqual(source.Snapshot.IssueComments, f.provider.comments) || source.Repository != f.anchor.Repository || source.RepositoryID != f.anchor.RepositoryID || source.PullNumber != f.anchor.PullNumber || source.Intent.OperationID != f.a.OperationID || source.Intent.AttemptID != f.a.ID || source.Intent.HeadSHA != f.anchor.CandidateSHA || source.PostingActor != query.PostingActor || source.ProviderActor != query.ProviderActor {
		t.Fatal("returned reference lost exact source or admitted identity", source, err)
	}
	if !got.Observation.QueryCreatedAt.Equal(query.Comment.CreatedAt) || !got.Observation.ResponseCreatedAt.Equal(f.provider.comments[1].CreatedAt) {
		t.Fatal("observation changed provider timing")
	}
	f.assertNotAdmitted(t)
	return query
}

func TestObserveHostedAllowancePostsOneFixedQueryAndRetainsExactSource(t *testing.T) {
	f := newRuntimeAllowanceFixture(t)
	first, err := f.observe(t)
	if err != nil {
		t.Fatal(err)
	}
	query := f.assertAvailableSource(t, first)
	// Renames and refreshed source observation times do not replace query identity.
	f.anchor.Repository = "renamed/project"
	f.anchor.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	second, err := f.observe(t)
	if err != nil || f.provider.posts != 1 || f.provider.reads != 2 || second.QueryID != first.QueryID || second.Observation != first.Observation {
		t.Fatal("repeated pass reposted or changed known observation", second, err, f.provider.posts, f.provider.reads)
	}
	after, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
	if err != nil || !reflect.DeepEqual(after, query) {
		t.Fatal("repeated pass rewrote original query", after, err)
	}
	if _, err := f.captures.Read(t.Context(), f.a.ID, first.Capture); err != nil {
		t.Fatal("later pass lost caller-owned original source", err)
	}
	f.assertNotAdmitted(t)
}

func TestObserveHostedAllowanceLostPostResponseNeverReposts(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		t.Run(map[bool]string{true: "accepted-server-side", false: "absent-uncertain"}[accepted], func(t *testing.T) {
			f := newRuntimeAllowanceFixture(t)
			f.provider.postError = errors.New("synthetic lost POST response")
			f.provider.dropPost = !accepted
			first, err := f.observe(t)
			if err == nil || first.Reason != "query_post_unproven" || first.QueryID == "" || first.Capture != (CaptureReference{}) || f.provider.posts != 1 || f.provider.reads != 0 {
				t.Fatal("lost response was hidden or observation fabricated", first, err)
			}
			before, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
			if err != nil || before.Revision != 1 || before.Comment != nil {
				t.Fatal("lost response forgot intent or fabricated returned ID", before, err)
			}
			for range 2 {
				got, err := f.observe(t)
				if err != nil || f.provider.posts != 1 || got.QueryID != before.ID || got.Capture.ID == "" {
					t.Fatal("uncertain POST retried or source not retained", got, err, f.provider.posts)
				}
				if accepted {
					query := f.assertAvailableSource(t, got)
					if query.EarliestPosting != before.EarliestPosting || query.Anchor != before.Anchor {
						t.Fatal("recovery reset original posting/source window")
					}
				} else if got.Observation.State != "unproven" || got.Observation.QueryCommentID != 0 || got.Observation.ResponseCommentID != 0 {
					t.Fatal("absence became posted query or permission", got)
				}
			}
			if f.provider.reads != 2 {
				t.Fatal("one pass polled internally", f.provider.reads)
			}
			if !accepted {
				after, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("absent query was changed or confirmed", after, err)
				}
			}
			f.assertNotAdmitted(t)
		})
	}
}

func TestObserveHostedAllowancePartialHistoryDoesNotFabricateCapture(t *testing.T) {
	f := newRuntimeAllowanceFixture(t)
	f.provider.historyErr = errors.New("synthetic later page failed")
	for range 2 {
		got, err := f.observe(t)
		if err == nil || got.Reason != "query_history_incomplete" || got.QueryID == "" || got.Capture != (CaptureReference{}) || got.Observation != (reviewprovider.HostedAllowanceObservation{}) || f.provider.posts != 1 {
			t.Fatal("partial history fabricated complete observation or repeated post", got, err)
		}
		query, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
		if err != nil || query.Comment == nil || query.Comment.ID != 100 || query.Revision != 2 {
			t.Fatal("history failure lost exact returned query", query, err)
		}
		f.assertNotAdmitted(t)
	}
	if f.provider.reads != 2 {
		t.Fatal("history failure triggered automatic retry", f.provider.reads)
	}
}

func TestObserveHostedAllowanceConfirmedIdentityCannotBeRecreated(t *testing.T) {
	f := newRuntimeAllowanceFixture(t)
	first, err := f.observe(t)
	if err != nil {
		t.Fatal(err)
	}
	original := f.assertAvailableSource(t, first)
	f.provider.comments[0].ID = 999 // Only a different comment with identical bytes remains.
	got, err := f.observe(t)
	if err != nil || got.Reason != "query_identity_changed" || got.Observation.State != "unproven" || got.Observation.QueryCommentID != 0 || got.Observation.ResponseCommentID != 0 || !got.Observation.RetryNotBefore.IsZero() || got.Capture.ID == "" || f.provider.posts != 1 {
		t.Fatal("recreated query replaced durable provider identity", got, err)
	}
	after, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
	if err != nil || !reflect.DeepEqual(original, after) {
		t.Fatal("conflicting source changed confirmed query", after, err)
	}
	f.assertNotAdmitted(t)
}

func TestObserveHostedAllowanceChangedBindingStopsBeforeProvider(t *testing.T) {
	for _, change := range []string{"candidate", "parent", "patch", "posting actor", "provider actor"} {
		t.Run(change, func(t *testing.T) {
			f := newRuntimeAllowanceFixture(t)
			first, err := f.observe(t)
			if err != nil {
				t.Fatal(err)
			}
			original := f.assertAvailableSource(t, first)
			switch change {
			case "candidate":
				f.anchor.CandidateSHA = strings.Repeat("f", 40)
			case "parent":
				f.anchor.ParentSHA = strings.Repeat("f", 40)
			case "patch":
				f.anchor.PatchSHA256 = strings.Repeat("f", 64)
			case "posting actor":
				f.provider.poster.ID++
			case "provider actor":
				f.provider.bot.ID++
			}
			got, err := f.observe(t)
			if err != nil || got.Reason != "query_binding_changed" || got.Capture != (CaptureReference{}) || got.Observation != (reviewprovider.HostedAllowanceObservation{}) || f.provider.posts != 1 || f.provider.reads != 1 {
				t.Fatal("changed query binding reached provider", got, err, f.provider.posts, f.provider.reads)
			}
			after, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
			if err != nil || !reflect.DeepEqual(original, after) {
				t.Fatal("binding refusal rewrote original query", after, err)
			}
			f.assertNotAdmitted(t)
		})
	}
}

func TestObserveHostedAllowanceRecoversIssuedQueryAfterPreparationCancelled(t *testing.T) {
	f := newRuntimeAllowanceFixture(t)
	f.provider.postError = errors.New("synthetic lost POST response")
	if _, err := f.observe(t); err == nil {
		t.Fatal("expected uncertain post")
	}
	before, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.a, err = f.f.engine.Advance(t.Context(), f.f.project, f.a.ID, f.a.Revision, "cancelled", "synthetic_preparation_stopped", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.observe(t)
	if err != nil || got.Observation.State != "available_observed" || got.Capture.ID == "" || f.provider.posts != 1 || f.provider.reads != 1 {
		t.Fatal("stopped preparation lost issued-query recovery or reposted", got, err)
	}
	after, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
	if err != nil || after.ID != before.ID || after.EarliestPosting != before.EarliestPosting || after.Anchor != before.Anchor || after.Comment == nil || after.Comment.ID != 100 {
		t.Fatal("stopped preparation replaced original query", after, err)
	}
	var state string
	var reservations, evidence int
	if err := f.f.store.DB().QueryRow(`SELECT state FROM review_execution_attempts WHERE id=?`, f.a.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := f.f.store.DB().QueryRow(`SELECT count(*) FROM review_execution_reservations`).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if err := f.f.store.DB().QueryRow(`SELECT count(*) FROM review_execution_evidence`).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" || reservations != 0 || evidence != 0 {
		t.Fatal("metadata observation restarted or admitted cancelled review", state, reservations, evidence)
	}
}

func TestObserveHostedAllowanceBasePlanURLRetainsExactSourceWithoutRepost(t *testing.T) {
	f := newRuntimeAllowanceFixture(t)
	f.provider.replyBody = "<!-- This is an auto-generated reply by CodeRabbit -->\nYour [plan](https://docs.coderabbit.ai/management/plans) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). Reviews are available now."
	first, err := f.observe(t)
	if err != nil {
		t.Fatal(err)
	}
	query := f.assertAvailableSource(t, first)
	firstBody := f.provider.comments[1].Body
	second, err := f.observe(t)
	if err != nil {
		t.Fatal(err)
	}
	f.assertAvailableSource(t, second)
	if f.provider.posts != 1 || f.provider.reads != 2 || second.QueryID != first.QueryID || second.Capture == first.Capture || second.Observation != first.Observation {
		t.Fatal("new observed URL reposted/rebound original query", second)
	}
	retained, err := f.captures.Read(t.Context(), f.a.ID, first.Capture)
	if err != nil || retained.Snapshot.IssueComments[1].Body != firstBody {
		t.Fatal("later observation rewrote original raw capture", err)
	}
	after, err := f.f.engine.ReadHostedAllowanceQuery(t.Context(), f.f.project, f.a.ID)
	if err != nil || !reflect.DeepEqual(after, query) {
		t.Fatal("later observation rewrote query journal", err)
	}
	f.assertNotAdmitted(t)
}
