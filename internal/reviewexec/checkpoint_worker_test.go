package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type checkpointFake struct {
	repositoryName                     string
	observedRepository                 string
	skew                               time.Duration
	mu                                 sync.Mutex
	comments                           []reviewprovider.IssueComment
	posts, reads, repositories, actors int
	postError                          error
	refuse                             bool
	invalid                            bool
	beforeActor                        func()
	beforePost                         func()
	entered                            chan struct{}
	release                            chan struct{}
}

func (p *checkpointFake) Repository(ctx context.Context, id int64) (reviewprovider.Repository, error) {
	p.mu.Lock()
	p.repositories++
	name := p.repositoryName
	if name == "" {
		name = "renamed/repository"
	}
	p.mu.Unlock()
	return reviewprovider.Repository{ID: id, FullName: name, DefaultBranch: "main"}, nil
}
func (p *checkpointFake) CurrentActor(ctx context.Context) (reviewprovider.Author, error) {
	p.mu.Lock()
	p.actors++
	hook := p.beforeActor
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	return reviewprovider.Author{ID: 71, Kind: "User", Login: "poster"}, nil
}
func (p *checkpointFake) PostIssueComment(ctx context.Context, repo string, pr int64, body string) (reviewprovider.IssueComment, error) {
	p.mu.Lock()
	p.posts++
	hook, entered, release := p.beforePost, p.entered, p.release
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	if entered != nil {
		close(entered)
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return reviewprovider.IssueComment{}, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refuse {
		return reviewprovider.IssueComment{}, &reviewprovider.Error{Code: "provider_refused", OutcomeUnknown: false}
	}
	at := time.Now().UTC().Truncate(time.Second)
	at = at.Add(p.skew)
	c := reviewprovider.IssueComment{ID: int64(800 + len(p.comments)), Body: body, Author: reviewprovider.Author{ID: 71, Kind: "User", Login: "poster"}, CreatedAt: at, UpdatedAt: at}
	p.comments = append(p.comments, c)
	if p.invalid {
		c.Body = "invalid confirmation"
	}
	return c, p.postError
}
func (p *checkpointFake) IssueComments(ctx context.Context, repo string, pr int64) ([]reviewprovider.IssueComment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	p.observedRepository = repo
	return append([]reviewprovider.IssueComment{}, p.comments...), nil
}
func (p *checkpointFake) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.posts, p.reads
}
func checkpointStart(t *testing.T, e *Engine, p CheckpointProvider) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done, err := e.StartCheckpointPublications(ctx, map[string]CheckpointProvider{"test-capability": p})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	return cancel, done
}
func checkpointStop(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop")
	}
}
func checkpointTerminal(t *testing.T, f *attemptFixture, id string) CheckpointPublication {
	t.Helper()
	v, terminal, err := f.e.WaitCheckpointPublication(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id, 3)
	if err != nil || !terminal {
		t.Fatal("no terminal", v, err)
	}
	return v
}
func checkpointRecover(t *testing.T, f *attemptFixture, v CheckpointPublication, key string, retry bool) []byte {
	t.Helper()
	raw, err := f.e.RecoverCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), key, v.ID, CheckpointPublicationRecovery{ExpectedRevision: &v.Revision, RetryUnposted: retry})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestCheckpointWorkerHappyHistoricalPublicationAndNoRepost(t *testing.T) {
	f, a, id := checkpointFixture(t, 1)
	r, raw := checkpointRequest(t, f, id, "happy")
	source := adjudicationRead(t, f, id)
	p := &checkpointFake{}
	cancel, done := checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	v := checkpointTerminal(t, f, r.ID)
	if v.State != "published" || v.CommentID != 800 || v.Counts != (FindingCounts{Found: 1, Accepted: 1}) || v.ProviderAuthorID != 71 || !strings.HasPrefix(v.CommentURL, "https://github.com/renamed/repository/") {
		t.Fatal(v)
	}
	posts, reads := p.counts()
	if posts != 1 || reads != 0 {
		t.Fatal(posts, reads)
	}
	if !adjudicationRead(t, f, id).Finalized || encoded(adjudicationRead(t, f, id)) != encoded(source) {
		t.Fatal("publication rewrote source")
	}
	// Source accepted-unfixed remains pending despite the public historical comment.
	var ev ReviewEvidence
	_ = f.e.store.Read(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		ev, _, err = loadReviewAdjudication(ctx, tx, attemptProject, id)
		return err
	})
	if ev.AttemptID != a.ID {
		t.Fatal("capture replaced")
	}
	replay, err := f.e.RequestCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "happy", CheckpointPublicationRequest{EvidenceID: id, ExpectedRevision: adjudicationRevision(2)})
	if err != nil || string(replay) != string(raw) {
		t.Fatal("lost original receipt", err)
	}
	checkpointRecover(t, f, v, "published-noop", false)
	posts, _ = p.counts()
	if posts != 1 {
		t.Fatal("published recovery reposted")
	}
}
func TestCheckpointWorkerLostPostRecoveryAndRestart(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "lost")
	p := &checkpointFake{postError: errors.New("private transport diagnostic")}
	cancel, done := checkpointStart(t, f.e, p)
	v := checkpointTerminal(t, f, r.ID)
	checkpointStop(t, cancel, done)
	if v.State != "uncertain" {
		t.Fatal(v)
	}
	_, err := f.e.RecoverCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "unsafe-retry", v.ID, CheckpointPublicationRecovery{ExpectedRevision: &v.Revision, RetryUnposted: true})
	attemptCode(t, err, "review_publication_held")
	// Restart the actual SQLite store, retain original capability/actor despite current pause.
	var path string
	if err = f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = core.Open(t.Context(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	f.httpFixture.store = f.store
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	checkpointRecover(t, f, v, "recover-lost", false)
	p.mu.Lock()
	p.postError = nil
	p.comments[0].Author.Login = "renamed-poster"
	p.mu.Unlock()
	cancel, done = checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	v = checkpointTerminal(t, f, r.ID)
	if v.State != "published" || v.CommentID != 800 {
		t.Fatal(v)
	}
	posts, reads := p.counts()
	if posts != 1 || reads != 1 {
		t.Fatal("recovery reposted", posts, reads)
	}
}
func TestCheckpointWorkerKnownRefusalExplicitRetryOnly(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "refusal")
	p := &checkpointFake{refuse: true}
	cancel, done := checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	v := checkpointTerminal(t, f, r.ID)
	if v.State != "held" || v.Reason != "provider_refused" {
		t.Fatal(v)
	}
	before := checkpointSQLState(t, f)
	checkpointRecover(t, f, v, "default-no-retry", false)
	v2 := checkpointRead(t, f, v.ID)
	if v2.Revision != v.Revision {
		t.Fatal("default recovery retried refusal")
	}
	_ = before
	p.mu.Lock()
	p.refuse = false
	p.mu.Unlock()
	raw := checkpointRecover(t, f, v, "explicit-safe-retry", true)
	var receipt Receipt
	json.Unmarshal(raw, &receipt)
	if receipt.Revision != v.Revision+1 {
		t.Fatal("recovery receipt lost current revision")
	}
	v = checkpointTerminal(t, f, r.ID)
	if v.State != "published" {
		t.Fatal(v)
	}
	posts, _ := p.counts()
	if posts != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_intents`) != 2 {
		t.Fatal("safe retry did not preserve intents")
	}
}
func TestCheckpointWorkerZeroMatchesAndConflictingCommentsNoPost(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		t.Run(map[bool]string{false: "zero", true: "conflicting"}[conflicting], func(t *testing.T) {
			f, _, id := checkpointFixture(t, 0)
			r, _ := checkpointRequest(t, f, id, "uncertain")
			p := &checkpointFake{postError: errors.New("lost")}
			cancel, done := checkpointStart(t, f.e, p)
			v := checkpointTerminal(t, f, r.ID)
			checkpointStop(t, cancel, done)
			p.mu.Lock()
			if conflicting {
				p.comments[0].Body = strings.Replace(p.comments[0].Body, "Found 0", "Found 1", 1)
			} else {
				p.comments = nil
			}
			p.mu.Unlock()
			checkpointRecover(t, f, v, "observe", false)
			cancel, done = checkpointStart(t, f.e, p)
			defer checkpointStop(t, cancel, done)
			v = checkpointTerminal(t, f, r.ID)
			if conflicting && v.State != "held" || !conflicting && v.State != "uncertain" {
				t.Fatal(v)
			}
			posts, reads := p.counts()
			if posts != 1 || reads != 1 {
				t.Fatal(posts, reads)
			}
		})
	}
}
func TestCheckpointWorkerLastGuardRevocationConfigRepository(t *testing.T) {
	for _, change := range []string{"revoked", "capability", "repository", "pause"} {
		t.Run(change, func(t *testing.T) {
			f, _, id := checkpointFixture(t, 0)
			r, _ := checkpointRequest(t, f, id, "guard")
			p := &checkpointFake{beforeActor: func() {
				switch change {
				case "revoked":
					admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
				case "capability":
					f.configure(t, attemptProject, "other-account", 1, false)
				case "repository":
					admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
				case "pause":
					f.configure(t, attemptProject, "test-capability", 1, true)
				}
			}}
			cancel, done := checkpointStart(t, f.e, p)
			defer checkpointStop(t, cancel, done)
			v := checkpointTerminal(t, f, r.ID)
			posts, _ := p.counts()
			if change == "pause" {
				if v.State != "published" || posts != 1 {
					t.Fatal(v, posts)
				}
			} else if v.State != "held" || posts != 0 {
				t.Fatal(v, posts)
			}
		})
	}
}
func TestCheckpointWorkerBlockedPostDoesNotHoldCoordinationAndCancellationRetainsIntent(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "blocked")
	p := &checkpointFake{entered: make(chan struct{}), release: make(chan struct{})}
	cancel, done := checkpointStart(t, f.e, p)
	select {
	case <-p.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("post not entered")
	}
	start := time.Now()
	f.configure(t, attemptProject, "test-capability", 1, true)
	if time.Since(start) > time.Second {
		t.Fatal("provider wait held writer")
	}
	checkpointRead(t, f, r.ID)
	checkpointStop(t, cancel, done)
	v := checkpointRead(t, f, r.ID)
	if v.State != "posting" {
		t.Fatal("cancellation claimed no post", v)
	}
	p.mu.Lock()
	p.entered = nil
	p.release = nil
	p.mu.Unlock()
	cancel, done = checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	v = checkpointTerminal(t, f, r.ID)
	if v.State != "uncertain" {
		t.Fatal(v)
	}
	posts, reads := p.counts()
	if posts != 1 || reads != 1 {
		t.Fatal("restart reissued post", posts, reads)
	}
}
func TestCheckpointWorkerConfirmationFailureRecoversExactlyOneComment(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "confirmation")
	p := &checkpointFake{beforePost: func() {
		admissionSQL(t, f, `CREATE TRIGGER checkpoint_fail_confirmation BEFORE INSERT ON review_checkpoint_revisions WHEN json_extract(NEW.body_json,'$.view.state')='published' BEGIN SELECT RAISE(ABORT,'synthetic confirmation failure'); END`)
	}}
	cancel, done := checkpointStart(t, f.e, p)
	select {
	case err := <-done:
		attemptCode(t, err, "checkpoint_publication_persistence_failed")
	case <-time.After(3 * time.Second):
		t.Fatal("fatal confirmation error hidden")
	}
	cancel()
	if checkpointRead(t, f, r.ID).State != "posting" {
		t.Fatal("lost durable posting intent")
	}
	admissionSQL(t, f, `DROP TRIGGER checkpoint_fail_confirmation`)
	p.mu.Lock()
	p.beforePost = nil
	p.mu.Unlock()
	cancel, done = checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	if v := checkpointTerminal(t, f, r.ID); v.State != "published" {
		t.Fatal(v)
	}
	posts, reads := p.counts()
	if posts != 1 || reads != 1 {
		t.Fatal(posts, reads)
	}
}
func TestCheckpointWorkerStartupValidationAndDisabledNoWrites(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	checkpointRequest(t, f, id, "disabled")
	before := checkpointSQLState(t, f)
	done, err := f.e.StartCheckpointPublications(t.Context(), nil)
	if done != nil || err != nil || checkpointSQLState(t, f) != before {
		t.Fatal("disabled worker mutated", err)
	}
	_, err = f.e.StartCheckpointPublications(t.Context(), map[string]CheckpointProvider{"missing": &checkpointFake{}})
	attemptCode(t, err, "validation_failed")
	cancel, done := checkpointStart(t, f.e, &checkpointFake{})
	_, err = f.e.StartCheckpointPublications(t.Context(), map[string]CheckpointProvider{"test-capability": &checkpointFake{}})
	attemptCode(t, err, "review_supervisor_running")
	checkpointStop(t, cancel, done)
}

func TestCheckpointWorkerPreparationRestartAndOriginalRecoveryBinding(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "restart-preparation")
	// Simulate only the committed read-phase claim; no provider action or intent.
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		record, err := readCheckpoint(ctx, tx, attemptProject, r.ID)
		if err != nil {
			return err
		}
		record.View.State = "preparing"
		record.View.Revision++
		record.View.UpdatedAt = sourceStamp()
		return persistCheckpoint(ctx, tx, record, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	p := &checkpointFake{postError: errors.New("unknown response")}
	cancel, done := checkpointStart(t, f.e, p)
	v := checkpointTerminal(t, f, r.ID)
	checkpointStop(t, cancel, done)
	if v.State != "uncertain" {
		t.Fatal(v)
	}
	// Existing issued work remains observable after current execution binding and
	// requester grant change. Original pinned account still owns recovery.
	f.configure(t, attemptProject, "other-account", 1, true)
	checkpointRecover(t, f, v, "schedule-before-revoke", false)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	cancel, done = checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	v = checkpointTerminal(t, f, r.ID)
	if v.State != "published" {
		t.Fatal("issued work was erased by current auth/config", v)
	}
	posts, reads := p.counts()
	if posts != 1 || reads != 1 {
		t.Fatal(posts, reads)
	}
}
func TestCheckpointWorkerChangedOriginalResourceCannotAdoptOtherAccount(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "changed-resource")
	p := &checkpointFake{postError: errors.New("lost")}
	cancel, done := checkpointStart(t, f.e, p)
	v := checkpointTerminal(t, f, r.ID)
	checkpointStop(t, cancel, done)
	checkpointRecover(t, f, v, "schedule", false)
	caps := append([]Capability(nil), attemptCapabilities...)
	caps[0].Account = "replacement-account"
	var err error
	f.e, err = New(f.store, caps)
	if err != nil {
		t.Fatal(err)
	}
	cancel, done = checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	v = checkpointTerminal(t, f, r.ID)
	if v.State != "held" || v.Reason != "capability_changed" {
		t.Fatal(v)
	}
	posts, reads := p.counts()
	if posts != 1 || reads != 0 {
		t.Fatal("replacement account consumed recovery", posts, reads)
	}
}
func TestCheckpointWorkerSourceChangedBeforePostCannotSpend(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "changed-source")
	p := &checkpointFake{beforeActor: func() {
		admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=replace(body_json,?,?) WHERE id=?`, strings.Repeat("1", 64), strings.Repeat("2", 64), id)
		admissionSQL(t, f, `UPDATE review_execution_evidence SET body_json=replace(body_json,?,?) WHERE id=?`, "review_capture_"+strings.Repeat("1", 32), "review_capture_"+strings.Repeat("2", 32), id)
	}}
	cancel, done := checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	v := checkpointTerminal(t, f, r.ID)
	if v.State != "held" || v.Reason != "source_changed" {
		t.Fatal(v)
	}
	posts, _ := p.counts()
	if posts != 0 {
		t.Fatal("changed capture posted")
	}
}
func TestCheckpointWorkerInvalidSuccessRemainsRecoverable(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	r, _ := checkpointRequest(t, f, id, "bad-success")
	p := &checkpointFake{invalid: true}
	cancel, done := checkpointStart(t, f.e, p)
	defer checkpointStop(t, cancel, done)
	v := checkpointTerminal(t, f, r.ID)
	if v.State != "uncertain" || v.Reason != "provider_confirmation_invalid" {
		t.Fatal(v)
	}
	checkpointRecover(t, f, v, "recover-bad-success", false)
	v = checkpointTerminal(t, f, r.ID)
	if v.State != "published" {
		t.Fatal(v)
	}
	posts, reads := p.counts()
	if posts != 1 || reads != 1 {
		t.Fatal(posts, reads)
	}
}

func TestCheckpointWorkerProviderClockSkewDoesNotInvalidateExactProof(t *testing.T) {
	for _, skew := range []time.Duration{-time.Hour, time.Hour} {
		for _, lost := range []bool{false, true} {
			t.Run(skew.String()+map[bool]string{false: "/success", true: "/recovery"}[lost], func(t *testing.T) {
				f, _, id := checkpointFixture(t, 0)
				r, _ := checkpointRequest(t, f, id, "skew")
				p := &checkpointFake{skew: skew}
				if lost {
					p.postError = errors.New("lost")
				}
				cancel, done := checkpointStart(t, f.e, p)
				defer checkpointStop(t, cancel, done)
				v := checkpointTerminal(t, f, r.ID)
				if lost {
					if v.State != "uncertain" {
						t.Fatal(v)
					}
					checkpointRecover(t, f, v, "skew-recovery", false)
					v = checkpointTerminal(t, f, r.ID)
				}
				if v.State != "published" || skew < 0 && v.PublishedAt >= v.CreatedAt || skew > 0 && v.PublishedAt <= v.CreatedAt {
					t.Fatal("provider clock inferred from local clock", v)
				}
				posts, reads := p.counts()
				if posts != 1 || lost && reads != 1 {
					t.Fatal(posts, reads)
				}
			})
		}
	}
}

func TestCheckpointWorkerRecoveryURLFollowsStableRepositoryRename(t *testing.T) {
	f, _, id := checkpointFixture(t, 0)
	receipt, _ := checkpointRequest(t, f, id, "rename-lost-response")
	provider := &checkpointFake{repositoryName: "previous-owner/previous-name", postError: errors.New("successful post response lost")}
	cancel, done := checkpointStart(t, f.e, provider)
	v := checkpointTerminal(t, f, receipt.ID)
	checkpointStop(t, cancel, done)
	if v.State != "uncertain" {
		t.Fatal(v)
	}
	var originalIntent string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_checkpoint_intents WHERE publication_id=?`, receipt.ID).Scan(&originalIntent); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.repositoryName = "new-owner/transferred-name"
	provider.postError = nil
	provider.comments[0].Author.Login = "poster-renamed"
	provider.mu.Unlock()
	recoveryReceipt := checkpointRecover(t, f, v, "rename-observe", false)
	cancel, done = checkpointStart(t, f.e, provider)
	published := checkpointTerminal(t, f, receipt.ID)
	checkpointStop(t, cancel, done)
	if published.State != "published" || published.CommentURL != "https://github.com/new-owner/transferred-name/pull/2#issuecomment-800" {
		t.Fatal("confirmation URL did not follow stable repository ID", published)
	}
	posts, reads := provider.counts()
	provider.mu.Lock()
	observed := provider.observedRepository
	provider.mu.Unlock()
	if posts != 1 || reads != 1 || observed != "new-owner/transferred-name" || published.ProviderAuthorID != 71 || published.Anchor.Repository != "owner/repo" {
		t.Fatal("recovery changed source/actor or reposted", posts, reads, observed, published)
	}
	var retainedIntent string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_checkpoint_intents WHERE publication_id=?`, receipt.ID).Scan(&retainedIntent); err != nil || retainedIntent != originalIntent {
		t.Fatal("recovery rewrote immutable posting intent", err)
	}
	// Close/reopen the actual SQLite store, not merely the Engine object.
	var path string
	if err := f.store.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(t.Context(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.httpFixture.store = reopened
	f.e, err = New(reopened, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if got := checkpointRead(t, f, receipt.ID); got != published {
		t.Fatal("published URL changed on reopen", got, published)
	}
	raw, err := f.e.RecoverCheckpointPublication(t.Context(), f.scope(t, attemptProject, f.writer, false), "rename-observe", receipt.ID, CheckpointPublicationRecovery{ExpectedRevision: &v.Revision})
	if err != nil || string(raw) != string(recoveryReceipt) || checkpointRead(t, f, receipt.ID).CommentURL != published.CommentURL {
		t.Fatal("replay lost confirmed destination", err)
	}
}
