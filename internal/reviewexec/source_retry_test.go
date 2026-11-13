package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func sourceFailure(at time.Time) error {
	return PreparationFailureAt("source_resolution", &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, RetryTiming: reviewprovider.RetryTimingEvidence{State: "proven", ObservedAt: at, NotBefore: at.Add(time.Minute)}})
}

func sourceFixture(t *testing.T) (*attemptFixture, Receipt, *time.Time) {
	t.Helper()
	f, r := stoppedQueuedFixture(t, "cli")
	now := time.Now().UTC().Add(time.Second)
	f.e.schedulingNow = func() time.Time { return now }
	f.e.attemptNow = func() time.Time { return now }
	f.e.sourceReadGroups = map[string]string{"test-capability": strings.Repeat("a", 64)}
	return f, r, &now
}

func scheduleSourceFixture(t *testing.T, f *attemptFixture, r Receipt, now time.Time) Attempt {
	t.Helper()
	a := f.start(t, attemptProject, r)
	scheduled, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, sourceFailure(now))
	if err != nil || !scheduled {
		t.Fatal("schedule", scheduled, err)
	}
	return a
}

func TestSourceRetryRetainsBoundedIntentAndRestarts(t *testing.T) {
	f, r, now := sourceFixture(t)
	var first Attempt
	for i := 1; i <= 3; i++ {
		a := scheduleSourceFixture(t, f, r, *now)
		if i == 1 {
			first = a
		}
		o := cliWaitRead(t, f, r.ID)
		if o.State != "queued" || o.AttemptID != "" || o.Wait == nil || o.Wait.SourceRetry == nil || o.Wait.SourceRetry.Sequence != i || o.Wait.SourceRetry.RetryNotBefore != now.Add(time.Minute).Format(time.RFC3339Nano) || o.Wait.BaselineAttemptID != "" || o.Wait.CLIAllowance != nil {
			t.Fatal(o)
		}
		if completionNoticeCount(t, f) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
			t.Fatal("wait stopped notice or reserved")
		}
		// Exact local replay cannot consume another retry or change the deadline.
		if ok, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, sourceFailure(*now)); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if attemptCount(t, f.store, `SELECT count(*) FROM review_source_retries`) != i {
			t.Fatal("duplicate evidence")
		}
		if a, err := f.e.StartAttempt(t.Context(), attemptProject, r.ID); err != nil || a.ID != "" {
			t.Fatal("early claim", a, err)
		}
		if err := f.e.SourceReadGate(t.Context(), strings.Repeat("a", 64)); err == nil {
			t.Fatal("source gate open early")
		}
		if err := f.e.SourceReadGate(t.Context(), strings.Repeat("b", 64)); err != nil {
			t.Fatal("unrelated credential blocked", err)
		}
		if err := f.e.RecoverInterrupted(t.Context()); err != nil {
			t.Fatal(err)
		}
		after := cliWaitRead(t, f, r.ID)
		if after.Wait.SourceRetry.RetryNotBefore != o.Wait.SourceRetry.RetryNotBefore {
			t.Fatal("restart reset floor")
		}
		tx, err := f.store.DB().BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = ValidateSourceRetriesSnapshot(t.Context(), tx)
		tx.Rollback()
		if err != nil {
			t.Fatal("snapshot", err)
		}
		*now = now.Add(time.Minute)
		if ok, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, sourceFailure(now.Add(-time.Minute))); err != nil || !ok {
			t.Fatal("expired exact settlement replay lost retained outcome", ok, err)
		}
	}
	a := f.start(t, attemptProject, r)
	if ok, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, sourceFailure(*now)); err != nil || ok {
		t.Fatal("fourth retry", ok, err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "preparation_source_resolution_provider_rate_limited", ""); err != nil {
		t.Fatal(err)
	}
	if completionNoticeCount(t, f) != 1 {
		t.Fatal("exhausted wait not actionable")
	}
	// A historical terminal operation cannot be resurrected by a new timer.
	if ok, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, first.ID, first.Revision, sourceFailure(*now)); err == nil && ok {
		t.Fatal("reopened historical wait")
	}
}

func TestSourceRetrySupervisorDoesNotDispatchOrDuplicate(t *testing.T) {
	f, r, now := sourceFixture(t)
	d := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		return Preparation{}, sourceFailure(*now)
	}}
	drivers := map[string]Driver{"test-capability": d}
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := f.e.processNext(t.Context(), drivers); err != nil {
			t.Fatal(err)
		}
	}
	if d.prepares.Load() != 1 || d.calls.Load() != 0 {
		t.Fatal("early network or dispatch")
	}
	*now = now.Add(time.Minute)
	d.prepare = func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		return Preparation{}, reviewHoldForSourceTest()
	}
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	if o := cliWaitRead(t, f, r.ID); o.State != "held" || o.Reason != "target_changed" {
		t.Fatal("fresh source guard lost", o)
	}
	if d.prepares.Load() != 2 || d.calls.Load() != 0 {
		t.Fatal("source failure dispatched")
	}
}

func TestRoundProgressShowsOnlyCurrentSourceRetryOnOptIn(t *testing.T) {
	f, r, now := sourceFixture(t)
	a := scheduleSourceFixture(t, f, r, *now)
	path := roundProgressPath(attemptProject) + "?pull_numbers=2&view=stages"
	read := func(query string) RoundProgressChannel {
		t.Helper()
		response := versionWaitRead(t, f, path+query, "5")
		var progress RoundProgress
		if err := json.Unmarshal(response.Body.Bytes(), &progress); err != nil || len(progress.Items) != 1 {
			t.Fatalf("invalid progress: %s", response.Body.String())
		}
		return progress.Items[0].Channels[1]
	}
	if got := read("&include_activity_evidence=true"); got.SourceRetry != nil {
		t.Fatal("source retry escaped opt-in", got)
	}
	if got := read("&include_source_retry=true"); got.SourceRetry == nil || got.SourceRetry.AttemptID != a.ID ||
		got.SourceRetry.RetryNotBefore != now.Add(time.Minute).Format(time.RFC3339Nano) || got.Used != 0 {
		t.Fatal("missing current retry or changed accounting", got)
	}
	*now = now.Add(time.Minute)
	f.start(t, attemptProject, r)
	if got := read("&include_source_retry=true"); got.SourceRetry != nil {
		t.Fatal("old retry masked a newer attempt", got)
	}
}

func TestHostedCooldownKeepsOriginalRequestThroughGitHubSourceRefreshLimit(t *testing.T) {
	f, original, cooldown, now := queuedCooldown(t)
	completionNoticeSchema(t, f)
	f.e.schedulingNow = func() time.Time { return *now }
	f.e.attemptNow = func() time.Time { return *now }
	f.e.sourceReadGroups = map[string]string{"test-capability": strings.Repeat("a", 64)}
	*now = cooldown.Observation.RetryNotBefore
	originalDeadline := now.Format(time.RFC3339Nano)
	driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		return Preparation{}, PreparationFailureAt("source_refresh", &reviewprovider.Error{
			Code: "provider_rate_limited", Status: http.StatusTooManyRequests,
			RetryTiming: reviewprovider.RetryTimingEvidence{State: "proven", ObservedAt: *now, NotBefore: now.Add(time.Minute)},
		})
	}}
	drivers := map[string]Driver{"test-capability": driver}
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	scope := f.scope(t, attemptProject, f.writer, false)
	o := bindingOperation(t, f, scope, original.OperationID)
	sourceDeadline := now.Add(time.Minute).Format(time.RFC3339Nano)
	if o.State != "queued" || o.Reason != sourceRetryReason || o.AttemptID != "" || o.Wait == nil || o.Wait.BaselineAttemptID != original.ID || o.Wait.RetryNotBefore != originalDeadline || o.Wait.SourceRetry == nil || o.Wait.SourceRetry.RetryNotBefore != sourceDeadline {
		t.Fatalf("original request or retry clocks changed: %+v", o)
	}
	for _, version := range []string{"1", "3", "4", "5", "6"} {
		projected := operationWaitForResponse(o.Wait, "hosted", version)
		if projected.RetryNotBefore != sourceDeadline || projected.SourceRetry != nil || projected.BaselineAttemptID != original.ID {
			t.Fatalf("version %s lost the effective retry deadline: %+v", version, projected)
		}
	}
	if cause, _ := f.e.queuedDiagnosticDetails(t.Context(), scope, o.ID, o.Revision); cause != "cooldown" {
		t.Fatalf("queued diagnostic ignored the GitHub deadline: %s", cause)
	}
	for _, action := range []string{"pause", "resume"} {
		receipt := timerControl(t, f, o.ID, action, "github-source-wait-"+action)
		if receipt.RetryNotBefore != sourceDeadline {
			t.Fatalf("%s lost the effective retry deadline: %+v", action, receipt)
		}
	}
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	if driver.prepares.Load() != 1 || driver.calls.Load() != 0 || completionNoticeCount(t, f) != 0 {
		t.Fatal("rate limit caused early preparation, review dispatch, or a terminal notice")
	}
	if attemptCount(t, f.store, "SELECT count(*) FROM review_execution_reservations") != 0 {
		t.Fatal("source wait held a review reservation")
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = ValidateSourceRetriesSnapshot(t.Context(), tx)
	tx.Rollback()
	if err != nil {
		t.Fatal("cannot restore the combined wait", err)
	}
	*now = now.Add(time.Minute)
	driver.prepare = func(ctx context.Context, o Operation, _ Configuration, _ Attempt) (Preparation, error) {
		baseline, err := f.e.RetryBaseline(ctx, attemptProject, o.ID)
		if err != nil || baseline == nil || *baseline != cooldown.Anchor {
			t.Fatal("source wait lost the original comparison", baseline, err)
		}
		return Preparation{}, &core.Error{Code: "review_held", Status: 409, Details: map[string]string{"reason": "source_changed"}}
	}
	if err := f.e.processNext(t.Context(), drivers); err != nil {
		t.Fatal(err)
	}
	o = bindingOperation(t, f, scope, original.OperationID)
	if o.State != "held" || o.Reason != "source_changed" || driver.prepares.Load() != 2 || driver.calls.Load() != 0 {
		t.Fatal("changed comparison escaped its ordinary hold", o)
	}
}

func reviewHoldForSourceTest() error {
	return PreparationFailureAt("source_resolution", &core.Error{Code: "review_held", Status: 409, Details: map[string]string{"reason": "target_changed"}})
}

func TestSourceRetryRefusesUnprovenAndSideEffectFailures(t *testing.T) {
	for _, name := range []string{"unknown", "unsupported", "expired", "future observation", "old observation", "too long", "authorization", "forbidden", "uncertain", "wrong stage", "wrong channel", "missing group", "preflight"} {
		t.Run(name, func(t *testing.T) {
			f, r, now := sourceFixture(t)
			a := f.start(t, attemptProject, r)
			p := &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, RetryTiming: reviewprovider.RetryTimingEvidence{State: "proven", ObservedAt: *now, NotBefore: now.Add(time.Minute)}}
			stage := "source_resolution"
			switch name {
			case "unknown", "unsupported":
				p.RetryTiming.State = name
			case "expired":
				p.RetryTiming.NotBefore = *now
			case "old observation":
				p.RetryTiming.ObservedAt = now.Add(-time.Second)
			case "future observation":
				p.RetryTiming.ObservedAt = now.Add(time.Second)
			case "too long":
				p.RetryTiming.NotBefore = now.Add(25 * time.Hour)
			case "forbidden":
				p.Code = "provider_unauthorized"
				p.Status = 403
			case "authorization":
				p.Code = "provider_unauthorized"
				p.Status = 401
			case "uncertain":
				p.OutcomeUnknown = true
			case "wrong stage":
				stage = "source_refresh"
			case "wrong channel":
				if _, err := f.store.DB().Exec(`UPDATE review_operations SET channel='hosted' WHERE id=?`, r.ID); err != nil {
					t.Fatal(err)
				}
			case "missing group":
				f.e.sourceReadGroups = nil
			case "preflight":
				if _, err := f.store.DB().Exec(`INSERT INTO review_cli_preflight VALUES(?,?,?)`, a.ID, attemptProject, "{}"); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, PreparationFailureAt(stage, p)); err != nil || ok {
				t.Fatal(ok, err)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_source_retries`) != 0 {
				t.Fatal("retained unsafe retry")
			}
		})
	}
}

func TestSourceRetryPauseCancelAndVersionedReaders(t *testing.T) {
	f, r, now := sourceFixture(t)
	scheduleSourceFixture(t, f, r, *now)
	base := "/api/v1/projects/" + attemptProject + "/reviews/operations/" + r.ID
	for _, version := range []string{"", "1", "2", "3", "4", "5", "6"} {
		for _, path := range []string{base, base + "/status"} {
			response := versionWaitRead(t, f, path, version)
			var op OperationStatus
			if err := json.Unmarshal(response.Body.Bytes(), &op); err != nil {
				t.Fatal(err)
			}
			if version == "4" || version == "6" {
				if op.Wait == nil || op.Wait.SourceRetry == nil {
					t.Fatal("new reader lost source floor")
				}
			} else if strings.Contains(response.Body.String(), "source_retry") {
				t.Fatal("old helper received unknown field")
			}
			if !legacyOperationWaitAccepts(op.Wait, "cli", "queued", "") {
				t.Fatal("legacy wait shape changed")
			}
			if strings.Contains(response.Body.String(), strings.Repeat("a", 64)) {
				t.Fatal("private credential group leaked")
			}
		}
	}
	o := cliWaitRead(t, f, r.ID)
	scope := followingScope(t, f, "requester")
	if _, err := f.e.ControlOperation(t.Context(), scope, "source-pause", r.ID, OperationControl{ExpectedRevision: o.Revision, Action: "pause"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, r.ID); err != nil || a.ID != "" {
		t.Fatal("paused wait claimed", a, err)
	}
	o = cliWaitRead(t, f, r.ID)
	if _, err := f.e.ControlOperation(t.Context(), scope, "source-cancel", r.ID, OperationControl{ExpectedRevision: o.Revision, Action: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if o = cliWaitRead(t, f, r.ID); o.State != "cancelled" {
		t.Fatal(o)
	}
	if response := f.request(http.MethodGet, base, f.reader, "", ""); response.Code != 200 {
		t.Fatal(response.Body.String())
	}
}

func TestSourceRetryRechecksAuthorityAndConfiguration(t *testing.T) {
	for _, change := range []string{"revoked", "configuration"} {
		t.Run(change, func(t *testing.T) {
			f, r, now := sourceFixture(t)
			scheduleSourceFixture(t, f, r, *now)
			*now = now.Add(time.Minute)
			want := "authorization_changed"
			if change == "revoked" {
				if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
					t.Fatal(err)
				}
			} else {
				f.configure(t, attemptProject, "other-account", 1, false)
				want = "configuration_changed"
			}
			a, err := f.e.StartAttempt(t.Context(), attemptProject, r.ID)
			if err != nil || a.ID != "" {
				t.Fatal(a, err)
			}
			if o := cliWaitRead(t, f, r.ID); o.State != "held" || o.Reason != want {
				t.Fatal(o)
			}
		})
	}
}

func TestSourceRetrySharedCredentialAndFreshEngine(t *testing.T) {
	f, r, now := sourceFixture(t)
	scheduleSourceFixture(t, f, r, *now)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	sibling := f.enqueue(t, otherAttemptProject, "shared-source-request", "cli", 2, 1)
	f.e.sourceReadGroups["other-account"] = strings.Repeat("a", 64)
	if _, ready, err := f.e.pendingCandidate(t.Context(), pendingCandidateID{otherAttemptProject, sibling.ID}); err != nil || ready {
		t.Fatal("shared credential escaped floor", ready, err)
	}
	f.e.sourceReadGroups["other-account"] = strings.Repeat("b", 64)
	if _, ready, err := f.e.pendingCandidate(t.Context(), pendingCandidateID{otherAttemptProject, sibling.ID}); err != nil || !ready {
		t.Fatal("unrelated credential blocked", ready, err)
	}
	fresh, err := New(f.store, attemptCapabilities, WithSourceReadGroups(map[string]string{"test-capability": strings.Repeat("a", 64)}), WithExecutionClock(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	if a, err := fresh.StartAttempt(t.Context(), attemptProject, r.ID); err != nil || a.ID != "" {
		t.Fatal("fresh engine shortened deadline", a, err)
	}
	if err := fresh.SourceReadGate(t.Context(), strings.Repeat("a", 64)); err == nil {
		t.Fatal("fresh engine lost source floor")
	}
	*now = now.Add(time.Minute)
	if err := fresh.SourceReadGate(t.Context(), strings.Repeat("a", 64)); err != nil {
		t.Fatal("floor did not expire", err)
	}
}

func TestSourceRetryConcurrentSettlementAndSuccessfulFreshDispatch(t *testing.T) {
	f, r, now := sourceFixture(t)
	a := f.start(t, attemptProject, r)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			ok, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, sourceFailure(*now))
			if !ok && err == nil {
				err = conflict()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_source_retries`) != 1 {
		t.Fatal("duplicate retained retry")
	}
	*now = now.Add(time.Minute)
	d := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		anchor := attemptAnchor(attemptProject, 2)
		return Preparation{Anchor: anchor}, nil
	}}
	if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": d}); err != nil {
		t.Fatal(err)
	}
	if d.prepares.Load() != 1 || d.calls.Load() != 1 {
		t.Fatal("fresh dispatch count", d.prepares.Load(), d.calls.Load())
	}
	if o := cliWaitRead(t, f, r.ID); o.State != "completed" {
		t.Fatal(o)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ValidateSourceRetriesSnapshot(t.Context(), tx); err != nil {
		t.Fatal("completed source history invalid", err)
	}
}

func TestSourceRetryClassified403AndSingleConnectionScheduling(t *testing.T) {
	f, r, now := sourceFixture(t)
	a := f.start(t, attemptProject, r)
	failure := sourceFailure(*now).(*preparationFailure)
	failure.err.(*reviewprovider.Error).Status = 403
	if ok, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, a.ID, a.Revision, failure); err != nil || !ok {
		t.Fatal("classified GitHub rate-limit 403", ok, err)
	}
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	sibling := f.enqueue(t, otherAttemptProject, "single-connection-sibling", "cli", 2, 1)
	f.e.sourceReadGroups["other-account"] = strings.Repeat("a", 64)
	f.store.DB().SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, ready, err := f.e.pendingCandidate(ctx, pendingCandidateID{otherAttemptProject, sibling.ID}); err != nil || ready {
		t.Fatal("single-connection gate", ready, err)
	}
}

func TestSourceRetryFiltersThrottledPrefixBeforeCandidateLimit(t *testing.T) {
	f, r, now := sourceFixture(t)
	scheduleSourceFixture(t, f, r, *now)
	seed := f.enqueue(t, attemptProject, "source-prefix-seed", "cli", 4, 1)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	ready := f.enqueue(t, otherAttemptProject, "ready-independent-source", "cli", 2, 1)
	f.e.sourceReadGroups["other-account"] = strings.Repeat("b", 64)
	// Public admission is smaller; imported/legacy history must still make progress.
	for i := range 299 {
		_, err := f.store.DB().Exec(`INSERT INTO review_operations(id,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision) SELECT ?,project_id,actor_id,worker_id,configuration_revision,pull_number,channel,head_sha,state,revision,reason,attempt_id,created_at,updated_at,capability_fingerprint,policy_revision FROM review_operations WHERE id=?`, fmt.Sprintf("source-prefix-%03d", i), seed.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := f.store.DB().QueryContext(t.Context(), `EXPLAIN QUERY PLAN SELECT DISTINCT source_group FROM review_source_retries INDEXED BY review_source_retries_due WHERE julianday(retry_not_before)>julianday(?)`, now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		indexed = indexed || strings.Contains(detail, "review_source_retries_due")
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !indexed {
		t.Fatal("future-group discovery lost deadline index", indexed, err)
	}
	ids, err := f.e.pendingCandidates(t.Context())
	if err != nil || len(ids) != 1 || ids[0].operation != ready.ID {
		t.Fatal("throttled prefix starved independent credential", len(ids), ids, err)
	}
	if _, eligible, err := f.e.pendingCandidate(t.Context(), ids[0]); err != nil || !eligible {
		t.Fatal("independent source not eligible", eligible, err)
	}
}

// A corrupt/missing deadline index must fail closed, never fall back to an
// unbounded history scan or admit a provider request.
func TestSourceRetryMissingDeadlineIndexRefusesCandidateDiscovery(t *testing.T) {
	f, _, _ := sourceFixture(t)
	if _, err := f.store.DB().Exec(`DROP INDEX review_source_retries_due`); err != nil {
		t.Fatal(err)
	}
	if ids, err := f.e.pendingCandidates(t.Context()); err == nil || len(ids) != 0 {
		t.Fatal("missing deadline index did not fail closed", ids, err)
	}
}

func TestSourceRetrySharedFloorKeepsNanosecondPrecision(t *testing.T) {
	f, r, now := sourceFixture(t)
	*now = now.Truncate(time.Second).Add(time.Second)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	sibling := f.enqueue(t, otherAttemptProject, "precise-sibling", "cli", 2, 1)
	f.e.sourceReadGroups["other-account"] = strings.Repeat("a", 64)
	first := f.start(t, attemptProject, r)
	second := f.start(t, otherAttemptProject, sibling)
	early := now.Add(time.Second)
	later := early.Add(time.Nanosecond)
	for i, a := range []Attempt{first, second} {
		due := early
		if i == 1 {
			due = later
		}
		failure := PreparationFailureAt("source_resolution", &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, RetryTiming: reviewprovider.RetryTimingEvidence{State: "proven", ObservedAt: *now, NotBefore: due}})
		if ok, err := f.e.ScheduleSourceRetry(t.Context(), a.ProjectID, a.ID, a.Revision, failure); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	*now = early
	if err := f.e.SourceReadGate(t.Context(), strings.Repeat("a", 64)); err == nil {
		t.Fatal("shared floor rounded away final nanosecond")
	}
	*now = later
	if err := f.e.SourceReadGate(t.Context(), strings.Repeat("a", 64)); err != nil {
		t.Fatal("exact latest floor not released", err)
	}
}

func TestSourceRetryPreservesIndependentQuotaBaseline(t *testing.T) {
	f, a, _, now := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	quota := cliWaitRead(t, f, a.OperationID)
	*now = mustWaitTime(quota.Wait.RetryNotBefore)
	f.e.attemptNow = func() time.Time { return *now }
	f.e.sourceReadGroups = map[string]string{"test-capability": strings.Repeat("a", 64)}
	source := f.start(t, attemptProject, Receipt{ID: a.OperationID})
	if ok, err := f.e.ScheduleSourceRetry(t.Context(), attemptProject, source.ID, source.Revision, sourceFailure(*now)); err != nil || !ok {
		t.Fatal(ok, err)
	}
	retained := cliWaitRead(t, f, a.OperationID)
	if retained.Wait == nil || retained.Wait.SourceRetry == nil || retained.Wait.BaselineAttemptID != a.ID || retained.Wait.RetryNotBefore != quota.Wait.RetryNotBefore || retained.Wait.CLIAllowance == nil {
		t.Fatal("source wait overwrote quota evidence", retained)
	}
	assertCombinedRetainedWaitHTTP(t, f, retained)
	baseline, err := f.e.RetryBaseline(t.Context(), attemptProject, a.OperationID)
	if err != nil || baseline == nil {
		t.Fatal("source wait lost original quota comparison", baseline, err)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ValidateSourceRetriesSnapshot(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOperationWaitsSnapshot(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
}
