package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

type externalTestReader struct {
	sourceCITestReader
	snapshot     reviewprovider.HostedPublicationSnapshot
	observe      func(context.Context) (reviewprovider.HostedPublicationSnapshot, error)
	archiveCalls int
}

func (r *externalTestReader) TrustedHostedActor() reviewprovider.Author {
	return reviewprovider.Author{ID: 202, Login: "display-only", Kind: "Bot"}
}
func (r *externalTestReader) ObserveHosted(ctx context.Context, _ string, _ int64) (reviewprovider.HostedPublicationSnapshot, error) {
	r.archiveCalls++
	if r.observe != nil {
		return r.observe(ctx)
	}
	return r.snapshot, nil
}
func externalSnapshot(n int, head string) reviewprovider.HostedPublicationSnapshot {
	at := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	bot := reviewprovider.Author{ID: 202, Login: "renamed-provider", Kind: "Bot"}
	snapshot := reviewprovider.HostedPublicationSnapshot{Complete: true, ObservedAt: at.Add(time.Hour), IssueComments: []reviewprovider.IssueComment{}, Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}}
	for i := 0; i < n; i++ {
		snapshot.Reviews = append(snapshot.Reviews, reviewprovider.HostedReview{ID: int64(401 + i), Author: bot, HeadSHA: head, State: "commented", SubmittedAt: at.Add(time.Duration(i) * time.Second), Body: fmt.Sprintf("**Actionable comments posted: 0**\n\n<!-- coderabbit-review-publication v1 publication=%08x-1111-1111-1111-111111111111 attempt=22222222-2222-2222-2222-222222222222 batch=1/1 -->", i+1)})
	}
	return snapshot
}
func externalReader(snapshot reviewprovider.HostedPublicationSnapshot) *externalTestReader {
	return &externalTestReader{sourceCITestReader: sourceCITestReader{refreshTestReader: refreshTestReader{pull: func(context.Context, string, int64) (reviewprovider.Pull, error) { return lifecyclePull(2), nil }}}, snapshot: snapshot}
}
func externalArchiveCount(t *testing.T, f *attemptFixture) int {
	t.Helper()
	return attemptCount(t, f.store, `SELECT count(DISTINCT id) FROM review_run_versions WHERE source='github-hosted-observer'`)
}
func TestExternalHostedSyncPausedReplayRestartOldHeadAndNoActions(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	old := strings.Repeat("d", 40)
	r := externalReader(externalSnapshot(2, old))
	before := map[string]int{}
	for _, table := range []string{"review_operations", "review_execution_attempts", "receipts", "review_source_refreshes"} {
		before[table] = attemptCount(t, f.store, "SELECT count(*) FROM "+table)
	}
	r.observe = func(context.Context) (reviewprovider.HostedPublicationSnapshot, error) {
		if attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_discovery_state`) != 1 {
			t.Fatal("frontier not committed before provider I/O")
		}
		return r.snapshot, nil
	}
	lifecycleTick(t, f, s, r)
	if got := externalArchiveCount(t, f); got != 2 {
		t.Fatal("independent runs not archived", got)
	}
	var detail reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &detail)
	if len(detail.Items) != 2 {
		t.Fatal("missing run history", detail)
	}
	for _, run := range detail.Items {
		if run.HeadSHA != old || run.Finalized || run.Accepted != 0 || run.Found != 0 || !run.Complete || run.StartedAt != run.FinishedAt {
			t.Fatal("invented current coverage/adjudication/runtime", run)
		}
	}
	snapshots := attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`)
	clock.Add(time.Minute)
	lifecycleTick(t, f, s, r)
	restarted := newLifecycleSchedule(s.options)
	lifecycleTick(t, f, restarted, r)
	if externalArchiveCount(t, f) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != snapshots {
		t.Fatal("replay/restart appended duplicate")
	}
	for table, count := range before {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != count {
			t.Fatal("passive observer created agent/review action", table)
		}
	}
}
func TestExternalHostedSyncUnprovenAndOutagesKeepLifecycleAlive(t *testing.T) {
	for _, failure := range []string{"partial", "parse", "outage", "oversized"} {
		t.Run(failure, func(t *testing.T) {
			f, s, clock := lifecycleFixture(t)
			r := externalReader(externalSnapshot(1, strings.Repeat("a", 40)))
			r.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
				p := lifecyclePull(2)
				p.Title = "Lifecycle survives " + failure
				return p, nil
			}
			switch failure {
			case "partial":
				r.snapshot.Complete = false
			case "parse":
				r.snapshot.Reviews = nil
			case "outage":
				r.observe = func(context.Context) (reviewprovider.HostedPublicationSnapshot, error) {
					return reviewprovider.HostedPublicationSnapshot{}, errors.New("PRIVATE provider token")
				}
			case "oversized":
				r.snapshot.Reviews[0].Body = strings.Replace(r.snapshot.Reviews[0].Body, "posted: 0", "posted: 101", 1)
				for i := 0; i < 101; i++ {
					at := r.snapshot.Reviews[0].SubmittedAt
					r.snapshot.ReviewComments = append(r.snapshot.ReviewComments, reviewprovider.ReviewComment{ID: int64(1000 + i), ReviewID: 401, Author: r.TrustedHostedActor(), HeadSHA: strings.Repeat("a", 40), OriginalHeadSHA: strings.Repeat("a", 40), Path: "synthetic.go", CreatedAt: at, UpdatedAt: at, Body: fmt.Sprintf("_⚠️ Potential issue_ | _🟠 Major_\n\n**Independent finding.**\n\nSource explanation.\n\n<!-- cr-comment:v1:%024x -->", i+1)})
				}
			}
			lifecycleTick(t, f, s, r)
			if lifecycleRead(t, f).Title != "Lifecycle survives "+failure || externalArchiveCount(t, f) != 0 {
				t.Fatal("archive failure hid metadata or fabricated archive")
			}
			if len(s.accounts) != 1 {
				t.Fatal("missing account cooldown")
			}

			diagnostic := s.due[lifecycleKey{attemptProject, 2}].lastErrorCode
			if diagnostic == "" {
				t.Fatal("missing per-PR held diagnostic")
			}
			for _, a := range s.accounts {
				if failure != "outage" {
					if !a.until.IsZero() || a.backoff != 0 {
						t.Fatal("local inventory held account", a)
					}
				} else if a.lastErrorCode == "" || a.until.IsZero() {
					t.Fatal("provider failure lost account cooldown", a)
				}
			}
			clock.Add(time.Minute)
			r.observe = nil
			r.snapshot = externalSnapshot(1, strings.Repeat("a", 40))
			lifecycleTick(t, f, s, r)
			if externalArchiveCount(t, f) != 1 {
				t.Fatal("later complete source not recovered")
			}
		})
	}
}
func TestExternalHostedSyncMoreThanOneHundredGroupsAndBadGroupIsolation(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	r := externalReader(externalSnapshot(102, strings.Repeat("a", 40)))
	r.snapshot.Reviews[0].Body = strings.Replace(r.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
	lifecycleTick(t, f, s, r)
	if got := externalArchiveCount(t, f); got == 0 || got > 100 {
		t.Fatal("bounded groups made no progress or exceeded the append ceiling", got)
	}
	// A slow/instrumented writer may reduce its batch to keep the existing
	// transaction deadline. Every committed tick must still make progress.
	for got := externalArchiveCount(t, f); got < 101; {
		clock.Add(time.Minute)
		lifecycleTick(t, f, s, r)
		next := externalArchiveCount(t, f)
		if next <= got || next-got > 100 {
			t.Fatalf("bounded tick did not progress: %d -> %d", got, next)
		}
		got = next
	}
	if externalArchiveCount(t, f) != 101 {
		t.Fatal("next bounded tick starved later groups")
	}
	// Validator-level text/size errors are isolated by savepoint and never stop metadata.
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		candidate := reviews.RunInput{SourceRunID: "publication:eeeeeeee-1111-1111-1111-111111111111", HeadSHA: strings.Repeat("a", 40), Channel: "hosted", Scope: "external publication", Outcome: "completed", Complete: true, StartedAt: "2026-07-08T09:10:11Z", FinishedAt: "2026-07-08T09:10:11Z", Findings: []reviews.Finding{{SourceFindingID: "bad", Title: strings.Repeat("x", 501), Severity: "unknown", Disposition: "unadjudicated"}}, Found: 1}
		held, err := publishExternalHostedInputs(ctx, tx, lifecycleTarget{project: attemptProject, repository: 801, number: 2}, []reviews.RunInput{candidate})
		if !held {
			t.Fatal("invalid provider candidate not held")
		}
		return err
	})
	if err != nil {
		t.Fatal("provider validation became fatal persistence", err)
	}
}
func TestExternalHostedNativeWindowsAndCapturedExactIdentity(t *testing.T) {
	for _, mode := range []string{"active", "uncertain", "unresolved", "foreign", "completed", "conflicting", "missing-marker"} {
		t.Run(mode, func(t *testing.T) {
			f, a, in := hostedPublicationFixture(t, true)
			run := reviewprovider.ExternalHostedRun{SourceID: "publication:" + in.Hosted.PublicationID, HeadSHA: a.Anchor.CandidateSHA, ReviewIDs: append([]int64(nil), in.Hosted.ReviewIDs...), NativeOperationID: a.OperationID, TriggerCommentID: 101, Inventory: reviewprovider.HostedFindingInventory{Complete: true, Publication: reviewprovider.HostedPublication{PublicationID: in.Hosted.PublicationID, ProviderAttemptID: in.Hosted.ProviderAttemptID}}}
			if mode == "unresolved" {
				admissionSQL(t, f, `UPDATE review_execution_attempts SET external_id='' WHERE id=?`, a.ID)
			}
			if mode == "uncertain" {
				admissionSQL(t, f, `UPDATE review_execution_attempts SET state='uncertain' WHERE id=?`, a.ID)
			}
			if mode == "foreign" {
				run.NativeOperationID = "review_operation_" + strings.Repeat("f", 32)
			}
			if mode == "completed" || mode == "conflicting" || mode == "missing-marker" {
				if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in); err != nil {
					t.Fatal(err)
				}
				if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured", ""); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "conflicting" {
				run.Inventory.Publication.PublicationID = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
			}
			if mode == "missing-marker" {
				run.NativeOperationID = ""
				run.TriggerCommentID = 0
			}
			err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
				target := lifecycleTarget{project: attemptProject, repository: a.Anchor.RepositoryID, number: 2}
				captures, held, err := externalNativeCaptures(ctx, tx, target, run)
				if err != nil || held {
					t.Fatal("native capture lookup", held, err)
				}
				owned, held := externalCapturedIdentity(captures, run)
				if !owned && !held {
					owned, held, err = externalHostedNativeBinding(ctx, tx, target, run)
				}
				switch mode {
				case "foreign":
					if owned || held {
						t.Fatal("pasted marker suppressed external")
					}
				case "unresolved", "conflicting":
					if owned || !held {
						t.Fatal("unproven native identity not held")
					}
				default:
					if !owned || held {
						t.Fatal("exact native identity duplicated", mode, owned, held)
					}
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestExternalHostedOldNativeStandingEditAfterFrontierAndUnrelatedCommand(t *testing.T) {
	f, a, in := standingEvidenceFixture(t)
	if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured", ""); err != nil {
		t.Fatal(err)
	}
	// Simulate discovery after a later unrelated command changed window attribution.
	run := reviewprovider.ExternalHostedRun{SourceID: "run:" + in.Hosted.StandingSummary.ProviderRunID, ProviderRunID: in.Hosted.StandingSummary.ProviderRunID, HeadSHA: a.Anchor.CandidateSHA, FinishedAt: time.Now().Add(time.Hour), ReviewIDs: []int64{}, Inventory: reviewprovider.HostedFindingInventory{Complete: true}, NativeOperationID: "review_operation_" + strings.Repeat("f", 32), TriggerCommentID: 999}
	err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		captures, held, err := externalNativeCaptures(ctx, tx, lifecycleTarget{project: attemptProject, repository: a.Anchor.RepositoryID, number: 2}, run)
		if err != nil || held {
			t.Fatal(held, err)
		}
		owned, held := externalCapturedIdentity(captures, run)
		if !owned || held {
			t.Fatal("edited old native zero imported as new external")
		}
		run.ProviderRunID = "44444444-4444-4444-4444-444444444444"
		owned, held = externalCapturedIdentity(captures, run)
		if owned || held {
			t.Fatal("unrelated manual zero suppressed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExternalHostedNativeIdentityIndexesAndConflictingOriginal(t *testing.T) {
	f, a, in := hostedPublicationFixture(t, true)
	if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`EXPLAIN QUERY PLAN SELECT id FROM review_execution_evidence INDEXED BY review_evidence_hosted_publication WHERE project_id=? AND json_extract(body_json,'$.capture.hosted.publication_id')=? LIMIT 3`,
		`EXPLAIN QUERY PLAN SELECT id FROM review_execution_evidence INDEXED BY review_evidence_hosted_run WHERE project_id=? AND coalesce(json_extract(body_json,'$.capture.hosted.submitted_review.provider_run_id'),json_extract(body_json,'$.capture.hosted.standing_summary.provider_run_id'))=? LIMIT 3`,
	} {
		rows, err := f.store.DB().Query(query, attemptProject, in.Hosted.PublicationID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			found = found || strings.Contains(detail, "SEARCH review_execution_evidence USING INDEX review_evidence_hosted_") && !strings.Contains(detail, "SCAN")
		}
		rows.Close()
		if !found {
			t.Fatal("exact provider UUID lookup does not use bounded identity index")
		}
	}
	// The UUID lookup must not infer ownership from a conflicting retained PR.
	run := reviewprovider.ExternalHostedRun{HeadSHA: a.Anchor.CandidateSHA, ReviewIDs: in.Hosted.ReviewIDs, Inventory: reviewprovider.HostedFindingInventory{Publication: reviewprovider.HostedPublication{PublicationID: in.Hosted.PublicationID, ProviderAttemptID: in.Hosted.ProviderAttemptID}}}
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, held, err := externalNativeCaptures(ctx, tx, lifecycleTarget{project: attemptProject, repository: a.Anchor.RepositoryID, number: 4}, run)
		if !held {
			t.Fatal("same UUID on foreign PR treated external")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_evidence'`).Scan(&version); err != nil || version != "4" {
		t.Fatal("latest schema was not used", version, err)
	}
}

func TestExternalHostedSyncPositiveInventoryAndRetirementDuringRead(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(fmt.Sprint(retire), func(t *testing.T) {
			f, s, _ := lifecycleFixture(t)
			r := externalReader(externalSnapshot(1, strings.Repeat("a", 40)))
			r.snapshot.Reviews[0].Body = strings.Replace(r.snapshot.Reviews[0].Body, "posted: 0", "posted: 1", 1)
			at := r.snapshot.Reviews[0].SubmittedAt
			r.snapshot.ReviewComments = []reviewprovider.ReviewComment{{ID: 501, ReviewID: 401, Author: r.TrustedHostedActor(), HeadSHA: strings.Repeat("a", 40), OriginalHeadSHA: strings.Repeat("a", 40), Path: "synthetic.go", CreatedAt: at, UpdatedAt: at, Body: "_⚠️ Potential issue_ | _🟠 Major_\n\n**Independent finding.**\n\nExact original detail.\n\n<!-- cr-comment:v1:111111111111111111111111 -->"}}
			if retire {
				r.observe = func(context.Context) (reviewprovider.HostedPublicationSnapshot, error) {
					prev := int64(1)
					w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/queue/edits", f.admin, "retire-in-archive", encoded(reviews.QueueEditRequest{ExpectedRevision: &prev, RepositoryID: 801, Source: "fixture", Queue: []reviews.QueuePullInput{}, Retired: []reviews.RetiredPull{{Number: 2, Reason: "superseded"}}}))
					if w.Code != 201 {
						t.Fatal(w.Code, w.Body.String())
					}
					return r.snapshot, nil
				}
			}
			lifecycleTick(t, f, s, r)
			if retire {
				if externalArchiveCount(t, f) != 0 {
					t.Fatal("retired target reactivated")
				}
				return
			}
			var runs reviews.RunsResponse
			projectionRead(t, f, "/pulls/2/runs", &runs)
			if len(runs.Items) != 1 || runs.Items[0].Found != 1 || runs.Items[0].Finalized {
				t.Fatal("positive source lost", runs)
			}
			var detail reviews.RunDetailResponse
			projectionRead(t, f, "/runs/"+runs.Items[0].ID, &detail)
			if len(detail.Findings) != 1 || detail.Findings[0].Disposition != "unadjudicated" || !strings.Contains(detail.Findings[0].Detail, "Exact original detail.") {
				t.Fatal("original finding changed", detail)
			}
		})
	}
}

func TestExternalHostedCollectionCancellationJoinsObserver(t *testing.T) {
	f, _, _ := lifecycleFixture(t)
	r := externalReader(externalSnapshot(1, strings.Repeat("a", 40)))
	entered := make(chan struct{})
	r.observe = func(ctx context.Context) (reviewprovider.HostedPublicationSnapshot, error) {
		close(entered)
		<-ctx.Done()
		return reviewprovider.HostedPublicationSnapshot{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, err := f.e.StartLifecycleSync(ctx, map[string]TargetReader{"test-capability": r}, LifecycleSyncOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("passive collection not started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not join canceled source read")
	}
	if externalArchiveCount(t, f) != 0 {
		t.Fatal("canceled collection fabricated archive")
	}
}

func TestExternalHostedRejectedCandidatesDoNotConsumeAppendBudget(t *testing.T) {
	f, _, _ := lifecycleFixture(t)
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := reviews.InitializeExternalHostedFrontier(ctx, tx, attemptProject, 801, 2); err != nil {
			return err
		}
		runs := []reviews.RunInput{}
		for i := 0; i < 101; i++ {
			runs = append(runs, reviews.RunInput{SourceRunID: fmt.Sprintf("publication:%08x-3333-3333-3333-333333333333", i+1), HeadSHA: strings.Repeat("a", 40), Channel: "hosted", Scope: "external publication", Outcome: "completed", Complete: true, StartedAt: "2026-07-08T09:10:11Z", FinishedAt: "2026-07-08T09:10:11Z", Found: 1, Findings: []reviews.Finding{{SourceFindingID: "source", Title: strings.Repeat("x", 501), Severity: "unknown", Disposition: "unadjudicated"}}})
		}
		good := runs[0]
		good.SourceRunID = "publication:ffffffff-3333-3333-3333-333333333333"
		good.Found = 0
		good.Findings = []reviews.Finding{}
		runs = append(runs, good)
		held, err := publishExternalHostedInputs(ctx, tx, lifecycleTarget{project: attemptProject, repository: 801, number: 2}, runs)
		if !held {
			t.Fatal("invalid sources not marked held")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if externalArchiveCount(t, f) != 1 {
		t.Fatal("invalid early groups starved later complete result")
	}
}

func TestExternalHostedConflictingNativeUUIDIsHeld(t *testing.T) {
	f, a, in := hostedPublicationFixture(t, true)
	if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in); err != nil {
		t.Fatal(err)
	}
	op := "review_operation_" + strings.Repeat("f", 32)
	attempt := "review_attempt_" + strings.Repeat("f", 32)
	admissionSQL(t, f, `INSERT INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision,allow_out_of_order) SELECT ?,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,?,created_at,updated_at,capability_fingerprint,policy_revision,allow_out_of_order FROM review_operations WHERE id=?`, op, attempt, a.OperationID)
	admissionSQL(t, f, `INSERT INTO review_execution_attempts(id,operation_id,project_id,provider,account,state,revision,anchor_json,intent,external_id) SELECT ?,?,project_id,provider,account,state,revision,anchor_json,intent,external_id FROM review_execution_attempts WHERE id=?`, attempt, op, a.ID)
	admissionSQL(t, f, `INSERT INTO review_execution_evidence(id,project_id,attempt_id,operation_id,body_json,fingerprint) SELECT ?,project_id,?,?,body_json,fingerprint FROM review_execution_evidence WHERE attempt_id=?`, "review_evidence_"+strings.Repeat("f", 32), attempt, op, a.ID)
	run := reviewprovider.ExternalHostedRun{HeadSHA: a.Anchor.CandidateSHA, Inventory: reviewprovider.HostedFindingInventory{Publication: reviewprovider.HostedPublication{PublicationID: in.Hosted.PublicationID}}}
	if err := f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		captures, held, err := externalNativeCaptures(ctx, tx, lifecycleTarget{project: attemptProject, repository: a.Anchor.RepositoryID, number: 2}, run)
		if !held || len(captures) != 0 {
			t.Fatal("conflicting duplicate provider UUID inferred ownership")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExternalHostedSyncRespectsEarlierCIRateLimit(t *testing.T) {
	f, s, clock := lifecycleFixture(t)
	r := externalReader(externalSnapshot(1, strings.Repeat("a", 40)))
	r.ci = func(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 120}
	}
	r.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
		p := lifecyclePull(2)
		p.Title = "Lifecycle before CI rate limit"
		return p, nil
	}
	lifecycleTick(t, f, s, r)
	if r.archiveCalls != 0 || lifecycleRead(t, f).Title != "Lifecycle before CI rate limit" {
		t.Fatal("CI cooldown ignored or lifecycle hidden")
	}
	clock.Add(time.Minute)
	if lifecycleTick(t, f, s, r) {
		t.Fatal("account cooldown ignored")
	}
}

func TestExternalHostedLocalInventoryDoesNotHoldSharedAccountTargets(t *testing.T) {
	for _, otherProject := range []bool{false, true} {
		for _, failureCode := range []string{"external_hosted_identity_unproven", "provider_invalid_response", "provider_rate_limited"} {
			t.Run(fmt.Sprintf("other-project=%t/failure=%s", otherProject, failureCode), func(t *testing.T) {
				rateLimited := failureCode == "provider_rate_limited"
				f, s, clock := lifecycleFixture(t)
				nextProject, nextNumber := attemptProject, int64(4)
				if otherProject {
					lifecycleProject(t, f, otherAttemptProject, "test-capability", 802)
					nextProject, nextNumber = otherAttemptProject, 2
				} else {
					previous := int64(2)
					if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "two-local-inventory-targets", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, Paused: true}); err != nil {
						t.Fatal(err)
					}
				}
				r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
				var selectedRepository, selectedNumber int64
				r.repository = func(_ context.Context, id int64) (reviewprovider.Repository, error) {
					selectedRepository = id
					return reviewprovider.Repository{ID: id, FullName: fmt.Sprintf("owner/repo%d", id), DefaultBranch: "main"}, nil
				}
				r.pull = func(_ context.Context, _ string, n int64) (reviewprovider.Pull, error) {
					selectedNumber = n
					p := lifecyclePull(n)
					p.RepositoryID = selectedRepository
					return p, nil
				}
				r.observe = func(context.Context) (reviewprovider.HostedPublicationSnapshot, error) {
					if selectedRepository == 801 && selectedNumber == 2 {
						if rateLimited {
							return reviewprovider.HostedPublicationSnapshot{}, &reviewprovider.Error{Code: "provider_rate_limited", RetryAfterSeconds: 120}
						}
						if failureCode == "provider_invalid_response" {
							return reviewprovider.HostedPublicationSnapshot{}, &reviewprovider.Error{Code: failureCode}
						}
						snapshot := r.snapshot
						snapshot.Complete = false
						return snapshot, nil
					}
					return r.snapshot, nil
				}
				readers := map[string]TargetReader{"test-capability": r}
				claim := func() *lifecycleTarget {
					t.Helper()
					target, err := f.e.claimLifecycle(t.Context(), readers, s)
					if err != nil {
						t.Fatal(err)
					}
					return target
				}
				observe := func(target *lifecycleTarget) {
					t.Helper()
					if target == nil {
						t.Fatal("missing eligible target")
					}
					if err := f.e.observeLifecycle(t.Context(), *target, s); err != nil {
						t.Fatal(err)
					}
				}
				first := claim()
				if first == nil || first.project != attemptProject || first.number != 2 {
					t.Fatal("unexpected initial target", first)
				}
				observe(first)
				for _, account := range s.accounts {
					if rateLimited {
						if account.backoff != 2*time.Minute || account.until != clock.Now().Add(2*time.Minute) {
							t.Fatal("real rate limit not shared", account)
						}
					} else if account.backoff != 0 || !account.until.IsZero() {
						t.Fatal("local inventory delayed shared account", account)
					}
				}
				if rateLimited {
					if claim() != nil {
						t.Fatal("rate limit allowed other shared-account target")
					}
					clock.Add(time.Minute)
					if claim() != nil {
						t.Fatal("account RetryAfter expired early")
					}
					clock.Add(time.Minute)
				}
				second := claim()
				if second == nil || second.project != nextProject || second.number != nextNumber {
					t.Fatal("other target starved by inventory", second)
				}
				observe(second)
				if rateLimited {
					return
				}
				if s.due[lifecycleKey{attemptProject, 2}].lastErrorCode != failureCode {
					t.Fatal("another target cleared local diagnostic")
				}
				if claim() != nil {
					t.Fatal("incomplete source retried before normal cadence")
				}
				clock.Add(59 * time.Second)
				if claim() != nil {
					t.Fatal("incomplete source retried early")
				}
				clock.Add(time.Second)
				again := claim()
				if again == nil || again.project != attemptProject || again.number != 2 {
					t.Fatal("incomplete source did not retry normally", again)
				}
				observe(again)
				for _, account := range s.accounts {
					if account.backoff != 0 || !account.until.IsZero() {
						t.Fatal("repeated incomplete inventory escalated provider backoff", account)
					}
				}
			})
		}
	}
}

func TestExternalHostedLocalDiagnosticPreservesRealProviderError(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
	r.snapshot.Complete = false
	r.ci = func(context.Context, reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
		return reviewprovider.CIObservation{}, &reviewprovider.Error{Code: "provider_unavailable"}
	}
	lifecycleTick(t, f, s, r)
	for _, account := range s.accounts {
		if account.lastErrorCode != "provider_unavailable" || account.backoff != 30*time.Second || account.until.IsZero() {
			t.Fatal("local evidence masked genuine provider failure", account)
		}
	}
	if delay := lifecycleProviderDelay(externalHostedUnproven(), time.Minute); delay != 0 {
		t.Fatal("local evidence has provider delay", delay)
	}
	if delay := lifecycleProviderDelay(&reviewprovider.Error{Code: "external_hosted_identity_unproven", RetryAfterSeconds: 120}, 0); delay != 2*time.Minute {
		t.Fatal("explicit RetryAfter was ignored", delay)
	}
}

func TestExternalHostedSyncLargeInventoryAndCapacityKeepLifecycle(t *testing.T) {
	for _, huge := range []bool{false, true} {
		t.Run(fmt.Sprint(huge), func(t *testing.T) {
			f, s, _ := lifecycleFixture(t)
			r := externalReader(externalSnapshot(2, strings.Repeat("a", 40)))
			count, text := 20, strings.Repeat("x", 14<<10)
			if huge {
				count, text = 100, strings.Repeat("<", 14<<10)
			}
			r.snapshot.Reviews[0].Body = strings.Replace(r.snapshot.Reviews[0].Body, "posted: 0", fmt.Sprintf("posted: %d", count), 1)
			at := r.snapshot.Reviews[0].SubmittedAt
			for i := 0; i < count; i++ {
				r.snapshot.ReviewComments = append(r.snapshot.ReviewComments, reviewprovider.ReviewComment{ID: int64(501 + i), ReviewID: 401, Author: r.TrustedHostedActor(), HeadSHA: strings.Repeat("a", 40), OriginalHeadSHA: strings.Repeat("a", 40), Path: "synthetic.go", CreatedAt: at, UpdatedAt: at, Body: fmt.Sprintf("_⚠️ Potential issue_ | _🟠 Major_\n\n**Independent original finding.**\n\n%s\n\n<!-- cr-comment:v1:%024x -->", text, i+1)})
			}
			expected, err := reviewprovider.DiscoverExternalHosted(r.snapshot, r.TrustedHostedActor())
			if err != nil || len(expected) != 2 || !expected[0].Inventory.Complete || len(expected[0].Inventory.Findings) != count {
				t.Fatal("fixture must be a supported complete provider inventory", err)
			}
			r.pull = func(context.Context, string, int64) (reviewprovider.Pull, error) {
				p := lifecyclePull(2)
				p.Title = "Metadata retained through large external inventory"
				return p, nil
			}
			lifecycleTick(t, f, s, r)
			if lifecycleRead(t, f).Title != "Metadata retained through large external inventory" {
				t.Fatal("capacity failure hid lifecycle metadata")
			}
			want := 2
			if huge {
				want = 1
			}
			if externalArchiveCount(t, f) != want {
				t.Fatal("large group lost or subsequent valid group blocked", externalArchiveCount(t, f))
			}
			var runs reviews.RunsResponse
			projectionRead(t, f, "/pulls/2/runs", &runs)
			for _, run := range runs.Items {
				if run.SourceRunID == expected[0].SourceID {
					if huge {
						t.Fatal("overcapacity inventory was truncated into a retained run")
					}
					var detail reviews.RunDetailResponse
					body := projectionRead(t, f, "/runs/"+run.ID, &detail)
					if len(body) <= 256<<10 || detail.Run.Found != 20 || len(detail.Findings) != 20 {
						t.Fatal("large inventory truncated", len(body), detail.Run, len(detail.Findings))
					}
					for i, finding := range detail.Findings {
						if finding.Detail != expected[0].Inventory.Findings[i].Detail {
							t.Fatal("original large finding text changed", i)
						}
					}
				}
			}
			if huge {
				if s.due[lifecycleKey{attemptProject, 2}].lastErrorCode != "external_hosted_identity_unproven" {
					t.Fatal("capacity source missing local held diagnostic")
				}
				for _, account := range s.accounts {
					if account.backoff != 0 || !account.until.IsZero() {
						t.Fatal("capacity rejection delayed shared provider account")
					}
				}
			}
		})
	}
}

func TestExternalHostedAuthoritativeCapacitySavepointPreservesMetadataAndNextRun(t *testing.T) {
	f, s, _ := lifecycleFixture(t)
	r := externalReader(externalSnapshot(0, strings.Repeat("a", 40)))
	target, err := f.e.claimLifecycle(t.Context(), map[string]TargetReader{"test-capability": r}, s)
	if err != nil || target == nil {
		t.Fatal(target, err)
	}
	run := reviews.RunInput{SourceRunID: "publication:aaaaaaaa-1111-1111-1111-111111111111", HeadSHA: strings.Repeat("a", 40), Channel: "hosted", Scope: "external publication", Outcome: "completed", Complete: true, StartedAt: "2026-07-08T09:10:11Z", FinishedAt: "2026-07-08T09:10:11Z", Found: 100, CoverageLimits: []string{}, Findings: []reviews.Finding{}}
	for i := 0; i < 100; i++ {
		run.Findings = append(run.Findings, reviews.Finding{SourceFindingID: fmt.Sprintf("bounded-%d", i), Title: "Original finding", Severity: "medium", Disposition: "unadjudicated"})
	}
	body, err := json.Marshal([]reviews.RunInput{run})
	if err != nil {
		t.Fatal(err)
	}
	per := ((4 << 20) - 16 - len(body)) / 600
	for i := range run.Findings {
		run.Findings[i].Detail = strings.Repeat("<", per)
	}
	body, err = json.Marshal([]reviews.RunInput{run})
	if err != nil {
		t.Fatal(err)
	}
	run.Findings[0].Detail += strings.Repeat("x", (4<<20)-16-len(body))
	body, err = json.Marshal([]reviews.RunInput{run})
	if err != nil || len(body) != (4<<20)-16 {
		t.Fatal("candidate must pass initial trusted encoding guard", len(body), err)
	}
	good := run
	good.SourceRunID = "publication:bbbbbbbb-1111-1111-1111-111111111111"
	good.Found = 0
	good.Findings = []reviews.Finding{}
	beforeSnapshots := attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`)
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		display := sourceDisplayPull(SourcePlan{Pull: lifecyclePull(2), ObservedAt: stamp()})
		display.Title = "Metadata survives authoritative detail rejection"
		if _, err := reviews.PublishSourceStatus(ctx, tx, core.Scope{ProjectID: attemptProject, PrincipalID: "service-lifecycle-sync"}, 801, target.pins, []reviews.SourceStatus{{Pull: display, Mergeability: "mergeable"}}); err != nil {
			return err
		}
		held, err := publishExternalHostedInputs(ctx, tx, *target, []reviews.RunInput{run, good})
		if !held {
			t.Fatal("authoritative capacity not held")
		}
		return err
	})
	if err != nil {
		t.Fatal("authoritative capacity stopped observer", err)
	}
	if lifecycleRead(t, f).Title != "Metadata survives authoritative detail rejection" || externalArchiveCount(t, f) != 1 {
		t.Fatal("capacity rollback lost metadata or next result")
	}
	if after := attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`); after != beforeSnapshots+2 {
		t.Fatal("failed candidate leaked snapshot", beforeSnapshots, after)
	}
	if count := attemptCount(t, f.store, `SELECT count(*) FROM review_finding_versions`); count != 0 {
		t.Fatal("failed source left findings", count)
	}
}
