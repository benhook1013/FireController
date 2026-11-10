package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests use real requests, allowance evidence and the SQLite journal.
// Capacity waits must retain authorization without earning review completions.
func cliWaitFixture(t *testing.T) (*attemptFixture, Attempt, CLIAllowanceObservation, *time.Time) {
	t.Helper()
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 2, 8, "cli-wait-rounds")
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "cli-wait-request", "cli", 2, 1))
	in := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 0, Limit: 3, FullCapacityInMS: 86400000, RollingWindowMS: 86400000, ChangedFiles: 2}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f.e.schedulingNow = func() time.Time { return now }
	return f, a, in, &now
}

func cliWaitRead(t *testing.T, f *attemptFixture, id string) Operation {
	t.Helper()
	o, err := readOperation(t.Context(), f.store.DB(), attemptProject, id)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func cliWaitDue(t *testing.T, o Operation) time.Time {
	t.Helper()
	if o.State != "queued" || o.AttemptID != "" || o.Wait == nil || o.Wait.BaselineAttemptID == "" || o.Wait.RetryNotBefore == "" {
		t.Fatal("lost durable undispatched wait", o)
	}
	due, err := time.Parse(time.RFC3339Nano, o.Wait.RetryNotBefore)
	if err != nil {
		t.Fatal(err)
	}
	return due
}

func assertCLIWaitSnapshot(t *testing.T, f *attemptFixture) {
	t.Helper()
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ValidateOperationWaitsSnapshot(t.Context(), tx); err != nil {
		t.Fatal("invalid retained waiting intent", err)
	}
}

func assertCLIWaitNoCredit(t *testing.T, f *attemptFixture) {
	t.Helper()
	c := roundRead(t, f, 2, "cli")
	if c.Used != 0 || c.Dry != 0 || c.Tapered || c.Closed {
		t.Fatal("unused preparation changed review progress", c)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("quota wait manufactured review completion or evidence")
	}
}

func TestCLIWaitRetainsIntentThroughRestartAndOriginalEvidence(t *testing.T) {
	f, a, in, now := cliWaitFixture(t)
	original := snapshotTable(t, f, "review_cli_preflight")
	unused, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision)
	if err != nil || unused.State != "held" || unused.ID != a.ID || unused.Anchor != nil || unused.Intent != "" || unused.ExternalID != "" {
		t.Fatal("exhausted preparation settlement", unused, err)
	}
	o := cliWaitRead(t, f, a.OperationID)
	due := cliWaitDue(t, o)
	if delta := due.Sub(*now); delta < 59*time.Second || delta > 61*time.Second {
		t.Fatal("first backoff must be local 60 seconds, not full-capacity time", delta)
	}
	if o.Wait.BaselineAttemptID != a.ID || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("wait lost original pin or retained capacity", o)
	}
	assertCLIWaitNoCredit(t, f)
	assertCLIWaitSnapshot(t, f)
	priorityPending(t, f)
	if early, err := f.e.StartAttempt(t.Context(), attemptProject, o.ID); err != nil || early.ID != "" {
		t.Fatal("early direct claim bypassed backoff", early, err)
	}
	cliAllowanceReopen(t, f)
	f.e.schedulingNow = func() time.Time { return *now }
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := cliWaitRead(t, f, o.ID); !reflect.DeepEqual(o, after) {
		t.Fatal("restart changed queued intent", after)
	}
	if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 10); err != nil {
		t.Fatal("detached exhaustion invalidates backup validation", err)
	}
	*now = due
	priorityPending(t, f, o.ID)
	next := f.start(t, attemptProject, Receipt{ID: o.ID})
	if next.ID == a.ID || next.OperationID != a.OperationID {
		t.Fatal("retry did not retain operation identity", next)
	}
	baseline, err := f.e.RetryBaseline(t.Context(), attemptProject, o.ID)
	if err != nil || baseline == nil || *baseline != in.Anchor {
		t.Fatal("retry lost original comparison pin", baseline, err)
	}
	if disposable, err := f.e.WorkspaceDisposable(t.Context(), "test-capability", a.ID, due.Add(time.Hour)); err != nil || !disposable {
		t.Fatal("detached unused workspace cannot be reclaimed", disposable, err)
	}
	if original != snapshotTable(t, f, "review_cli_preflight") {
		t.Fatal("retry rewrote exhaustion evidence")
	}
	if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal("exact original evidence replay failed after detachment", err)
	}
	if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 10); err != nil {
		t.Fatal("historical allowance invalid after next attempt", err)
	}
	assertCLIWaitNoCredit(t, f)
	assertCLIWaitSnapshot(t, f)
}

func TestCLIWaitBackoffIsBoundedAndFirstComparisonIsRetained(t *testing.T) {
	f, a, in, now := cliWaitFixture(t)
	first := a.ID
	firstAnchor := in.Anchor
	for i, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		if i > 0 {
			in.ObservedAt = stamp()
			in.Anchor.ObservedAt = stamp()
			if err := f.e.RecordCLIExhaustion(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
				t.Fatal("later exhaustion observation", err)
			}
		}
		if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
			t.Fatal("schedule exhausted attempt", i, err)
		}
		o := cliWaitRead(t, f, a.OperationID)
		due := cliWaitDue(t, o)
		if delta := due.Sub(*now); delta < delay-time.Second || delta > delay+time.Second || o.Wait.BaselineAttemptID != first {
			t.Fatal("incorrect bounded backoff or baseline", i, delta, o.Wait)
		}
		if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 10); err != nil {
			t.Fatal("detached historical allowance validation", i, err)
		}
		baseline, err := f.e.RetryBaseline(t.Context(), attemptProject, o.ID)
		if err != nil || baseline == nil || *baseline != firstAnchor {
			t.Fatal("later exhaustion replaced the originally authorized comparison", i, baseline, err)
		}
		*now = due
		a = f.start(t, attemptProject, Receipt{ID: o.ID})
	}
	assertCLIWaitNoCredit(t, f)
	assertCLIWaitSnapshot(t, f)
}

func TestCLIWaitPauseResumeCancelNeverDispatchesOrReopensHistory(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f, a, _, now := cliWaitFixture(t)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
				t.Fatal(err)
			}
			o := cliWaitRead(t, f, a.OperationID)
			due := cliWaitDue(t, o)
			timerControl(t, f, o.ID, action, "cli-wait-"+action)
			*now = due.Add(time.Minute)
			priorityPending(t, f)
			claim, err := f.e.StartAttempt(t.Context(), attemptProject, o.ID)
			if action == "cancel" {
				attemptCode(t, err, "review_conflict")
				if after := cliWaitRead(t, f, o.ID); after.State != "cancelled" || currentOperationWait(after) != nil || after.Wait == nil {
					t.Fatal("cancel did not stop original intent", after)
				}
				cliAllowanceReopen(t, f)
				if err := f.e.RecoverInterrupted(t.Context()); err != nil {
					t.Fatal(err)
				}
				if after := cliWaitRead(t, f, o.ID); after.State != "cancelled" {
					t.Fatal("restart reopened cancelled request", after)
				}
			} else {
				if err != nil || claim.ID != "" {
					t.Fatal("paused direct claim", claim, err)
				}
				timerControl(t, f, o.ID, "resume", "cli-wait-resume")
				resumed := cliWaitRead(t, f, o.ID)
				if resumed.Wait == nil || resumed.Wait.RetryNotBefore != o.Wait.RetryNotBefore || resumed.Wait.BaselineAttemptID != a.ID {
					t.Fatal("resume changed retained timer/pin", resumed)
				}
				priorityPending(t, f, o.ID)
			}
			assertCLIWaitNoCredit(t, f)
			assertCLIWaitSnapshot(t, f)
		})
	}
}

func TestCLIWaitSharedAccountBackoffSurvivesCancelWithoutBlockingOtherChannels(t *testing.T) {
	for _, sameAccount := range []bool{true, false} {
		t.Run(map[bool]string{true: "same-account", false: "different-account"}[sameAccount], func(t *testing.T) {
			f, a, _, now := cliWaitFixture(t)
			capability := "other-account"
			if sameAccount {
				capability = "test-capability"
			}
			f.configure(t, otherAttemptProject, capability, 0, false)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
				t.Fatal(err)
			}
			due := cliWaitDue(t, cliWaitRead(t, f, a.OperationID))
			other := f.enqueue(t, otherAttemptProject, "other-explicit-cli", "cli", 2, 1)
			hosted := f.enqueue(t, otherAttemptProject, "other-explicit-hosted", "hosted", 4, 1)
			// Cancelling the exhausted request must not erase the shared account's
			// recently observed exhaustion or let every queued PR requery at once.
			timerControl(t, f, a.OperationID, "cancel", "cancel-cli-donor")
			if sameAccount {
				priorityPending(t, f, hosted.ID)
				if early, err := f.e.StartAttempt(t.Context(), otherAttemptProject, other.ID); err != nil || early.ID != "" {
					t.Fatal("direct cross-project claim bypassed account backoff", early, err)
				}
			} else {
				priorityPending(t, f, other.ID, hosted.ID)
			}
			*now = due
			priorityPending(t, f, other.ID, hosted.ID)
			f.start(t, otherAttemptProject, other)
			assertCLIWaitNoCredit(t, f)
			assertCLIWaitSnapshot(t, f)
		})
	}
}

func TestCLIWaitRequestsRespectCurrentPRRankWithoutAutomaticDiscovery(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	later := f.enqueue(t, attemptProject, "later-cli-first", "cli", 4, 1)
	head := f.enqueue(t, attemptProject, "earlier-cli-second", "cli", 2, 1)
	hosted := f.enqueue(t, attemptProject, "independent-hosted", "hosted", 4, 1)
	priorityPending(t, f, head.ID, hosted.ID)
	if a, err := f.e.StartAttempt(t.Context(), attemptProject, later.ID); err != nil || a.ID != "" {
		t.Fatal("oldest CLI request bypassed current PR order", a, err)
	}
	timerControl(t, f, head.ID, "pause", "pause-priority-head")
	priorityPending(t, f, later.ID, hosted.ID)
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 3 {
		t.Fatal("priority or waiting generated unsolicited requests")
	}
}

func TestCLIWaitRecoveryRequiresProvenUnusedExhaustion(t *testing.T) {
	f, a, _, now := cliWaitFixture(t)
	cliAllowanceReopen(t, f)
	f.e.schedulingNow = func() time.Time { return *now }
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	o := cliWaitRead(t, f, a.OperationID)
	cliWaitDue(t, o)
	assertCLIWaitNoCredit(t, f)
	assertCLIWaitSnapshot(t, f)
	if err := ValidateAllowanceSnapshot(t.Context(), f.store.DB(), 10); err != nil {
		t.Fatal("recovery lost retained exhaustion evidence", err)
	}
}

func TestCLIWaitDoesNotReopenTerminalQuotaHold(t *testing.T) {
	f, a, _, _ := cliWaitFixture(t)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "allowance_exhausted", ""); err != nil {
		t.Fatal(err)
	}
	before := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts")
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision+1); err == nil {
		t.Fatal("old terminal quota hold reopened without a new authorized request")
	}
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts"); before != after {
		t.Fatal("terminal quota history changed")
	}
	assertCLIWaitNoCredit(t, f)
}

func TestCLIWaitRecoveryDoesNotRetryPositiveUnusedPermit(t *testing.T) {
	f, a, in := cliAllowanceFixture(t)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, in); err != nil {
		t.Fatal(err)
	}
	cliAllowanceReopen(t, f)
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o := cliWaitRead(t, f, a.OperationID); o.State != "cancelled" || o.Wait != nil {
		t.Fatal("startup reused a positive old permit", o)
	}
}

func TestCLIWaitRejectsDuplicateNewKeyButReplaysOriginalRequest(t *testing.T) {
	f, a, _, _ := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	scope := f.scope(t, attemptProject, f.writer, false)
	request := Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
	before := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`)
	if _, err := f.e.Enqueue(t.Context(), scope, "duplicate-cli-new-key", request); err == nil {
		t.Fatal("a new key purchased a second pending CLI round")
	} else {
		attemptCode(t, err, "review_inflight")
	}
	replayed, err := f.e.Enqueue(t.Context(), scope, "cli-wait-request", request)
	if err != nil {
		t.Fatal("original request receipt stopped replaying while queued", err)
	}
	var receipt Receipt
	if err := json.Unmarshal(replayed, &receipt); err != nil || receipt.ID != a.OperationID || receipt.State != "queued" {
		t.Fatal("replay manufactured a different request", receipt, err)
	}
	if after := attemptCount(t, f.store, `SELECT count(*) FROM review_operations`); after != before {
		t.Fatal("duplicate/replay changed operation count", before, after)
	}
	assertCLIWaitNoCredit(t, f)
}

func TestCLIWaitConcurrentAccountClaimsStartOnlyOneAttempt(t *testing.T) {
	f, a, _, now := cliWaitFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	otherRequest := f.enqueue(t, otherAttemptProject, "competing-account-cli", "cli", 2, 1)
	*now = cliWaitDue(t, cliWaitRead(t, f, a.OperationID))
	otherEngine, err := New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	otherEngine.schedulingNow = f.e.schedulingNow
	type result struct {
		attempt Attempt
		err     error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, claim := range []struct {
		engine  *Engine
		project string
		id      string
	}{{f.e, attemptProject, a.OperationID}, {otherEngine, otherAttemptProject, otherRequest.ID}} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			attempt, err := claim.engine.StartAttempt(t.Context(), claim.project, claim.id)
			results <- result{attempt, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	claimed := 0
	for result := range results {
		if result.err != nil {
			attemptCode(t, result.err, "review_busy")
		}
		if result.attempt.ID != "" {
			claimed++
		}
	}
	if claimed != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 1 {
		t.Fatal("quota expiry allowed concurrent shared-account attempts", claimed)
	}
	assertCLIWaitNoCredit(t, f)
}

func TestOperationWaitReadsLaterExecutionSchemaWithoutLosingPause(t *testing.T) {
	f := roundFixture(t)
	receipt := f.enqueue(t, attemptProject, "paused-future-schema", "cli", 2, 1)
	admissionSQL(t, f, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,NULL,'',1)`, receipt.ID)
	admissionSQL(t, f, `UPDATE review_operations SET allow_out_of_order=1,override_file_limit=1 WHERE id=?`, receipt.ID)
	scope := f.scope(t, attemptProject, f.writer, false)
	before, err := f.e.ReadOperation(t.Context(), scope, receipt.ID)
	if err != nil || before.Wait == nil || !before.Wait.Paused || !before.AllowOutOfOrder || !before.OverrideFileLimit {
		t.Fatal("invalid paused operation baseline", before, err)
	}
	for _, version := range []string{"9", "10", "11", "12", "100"} {
		t.Run(version, func(t *testing.T) {
			admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_execution'`, version)
			after, err := f.e.ReadOperation(t.Context(), scope, receipt.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("later execution schema changed retained operation", before, after, err)
			}
			status, err := f.e.ReadOperationStatus(t.Context(), scope, receipt.ID)
			if err != nil || status.ID != receipt.ID || !reflect.DeepEqual(before.Wait, status.Wait) {
				t.Fatal("compact status dropped retained pause", status, err)
			}
		})
	}
}

func TestOperationReadsSchemaFlagBoundaries(t *testing.T) {
	f := roundFixture(t)
	receipt := f.enqueue(t, attemptProject, "schema-flag-boundaries", "cli", 2, 1)
	admissionSQL(t, f, `UPDATE review_operations SET allow_out_of_order=1,override_file_limit=1 WHERE id=?`, receipt.ID)
	scope := f.scope(t, attemptProject, f.writer, false)
	for _, tc := range []struct {
		version      string
		order, limit bool
	}{{"1", false, false}, {"6", false, false}, {"7", true, false}, {"8", true, true}} {
		t.Run(tc.version, func(t *testing.T) {
			admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_execution'`, tc.version)
			o, err := f.e.ReadOperation(t.Context(), scope, receipt.ID)
			if err != nil || o.ID != receipt.ID || o.AllowOutOfOrder != tc.order || o.OverrideFileLimit != tc.limit || o.Wait != nil {
				t.Fatal("incorrect legacy flags", o, err)
			}
		})
	}
}

func TestOperationReadsRejectMalformedExecutionSchema(t *testing.T) {
	f := roundFixture(t)
	receipt := f.enqueue(t, attemptProject, "malformed-execution-schema", "cli", 2, 1)
	scope := f.scope(t, attemptProject, f.writer, false)
	for _, version := range []string{"", "0", "-1", "+11", "011", " 11", "11 ", "11\n", "1.1", "eleven", "999999999999999999999999999999"} {
		t.Run(version, func(t *testing.T) {
			admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_execution'`, version)
			o, err := f.e.ReadOperation(t.Context(), scope, receipt.ID)
			if err == nil || !reflect.DeepEqual(o, Operation{}) {
				t.Fatal("malformed schema returned operation", o, err)
			}
			attemptCode(t, err, "storage_unavailable")
			status, err := f.e.ReadOperationStatus(t.Context(), scope, receipt.ID)
			if err == nil || !reflect.DeepEqual(status, OperationStatus{}) {
				t.Fatal("malformed schema returned compact status", status, err)
			}
			attemptCode(t, err, "storage_unavailable")
		})
	}
}

func TestCLISharedQuotaHTTPDiagnostics(t *testing.T) {
	for _, mode := range []string{"shared", "cancelled-origin", "expired", "other-account", "changed-binding", "changed-fingerprint"} {
		t.Run(mode, func(t *testing.T) {
			f, a, _, now := cliWaitFixture(t)
			capability := "test-capability"
			if mode == "other-account" {
				capability = "other-account"
			}
			f.configure(t, otherAttemptProject, capability, 0, false)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
				t.Fatal(err)
			}
			other := f.enqueue(t, otherAttemptProject, "diagnostic-cli", "cli", 2, 1)
			if mode == "cancelled-origin" {
				timerControl(t, f, a.OperationID, "cancel", "cancel-diagnostic-origin")
			}
			if mode == "expired" {
				*now = cliWaitDue(t, cliWaitRead(t, f, a.OperationID))
			}
			if mode == "changed-binding" {
				f.configure(t, otherAttemptProject, "other-account", 1, false)
			}
			if mode == "changed-fingerprint" {
				changed := f.e.capabilities[capability]
				changed.Fingerprint = strings.Repeat("f", 64)
				f.e.capabilities[capability] = changed
			}
			mux := http.NewServeMux()
			f.e.Register(mux)
			tables := []string{"review_operations", "review_execution_attempts", "review_operation_waits", "review_cli_preflight", "review_execution_reservations", "events", "receipts"}
			before := make(map[string]string)
			for _, table := range tables {
				before[table] = snapshotTable(t, f, table)
			}
			for _, suffix := range []string{"", "/status"} {
				for _, version := range []string{"", "1", "2", "3", "4", "5", "unsupported"} {
					req := httptest.NewRequest("GET", "/api/v1/projects/"+otherAttemptProject+"/reviews/operations/"+other.ID+suffix, nil)
					req.Header.Set("Authorization", "Bearer "+f.other)
					req.Header.Set(operationWaitHeader, version)
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					want := "awaiting_scheduler"
					if mode == "shared" || mode == "cancelled-origin" {
						want = "cli_quota_wait"
					}
					if w.Code != http.StatusOK || w.Header().Get("X-FC-Review-Queued") != want {
						t.Fatalf("%s schema %q: %d %v %s", suffix, version, w.Code, w.Header(), w.Body.String())
					}
					var body map[string]any
					if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if body["state"] != "queued" || body["wait"] != nil || body["allowance"] != nil {
						t.Fatal("diagnostic fabricated operation facts", body)
					}
					req = httptest.NewRequest("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+other.ID+suffix, nil)
					req.Header.Set("Authorization", "Bearer "+f.writer)
					w = httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					if w.Code == http.StatusOK || w.Header().Get("X-FC-Review-Queued") != "" {
						t.Fatal("cross-project diagnostic leaked", w.Code, w.Header())
					}
				}
			}
			for _, table := range tables {
				if before[table] != snapshotTable(t, f, table) {
					t.Fatal("read changed state", table)
				}
			}
		})
	}
}

func TestCLISharedQuotaHTTPDiagnosticsPreferPause(t *testing.T) {
	for _, mode := range []string{"operation", "project"} {
		t.Run(mode, func(t *testing.T) {
			f, a, _, _ := cliWaitFixture(t)
			f.configure(t, otherAttemptProject, "test-capability", 0, false)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
				t.Fatal(err)
			}
			other := f.enqueue(t, otherAttemptProject, "paused-diagnostic-cli", "cli", 2, 1)
			if mode == "operation" {
				admissionSQL(t, f, `INSERT INTO review_operation_waits(operation_id,baseline_attempt_id,retry_not_before,paused) VALUES(?,NULL,'',1)`, other.ID)
			} else {
				f.configure(t, otherAttemptProject, "test-capability", 1, true)
			}
			mux := http.NewServeMux()
			f.e.Register(mux)
			before := snapshotTable(t, f, "review_operation_waits")
			for _, suffix := range []string{"", "/status"} {
				for _, version := range []string{"", "1", "2", "3", "4", "5", "unsupported"} {
					req := httptest.NewRequest("GET", "/api/v1/projects/"+otherAttemptProject+"/reviews/operations/"+other.ID+suffix, nil)
					req.Header.Set("Authorization", "Bearer "+f.other)
					req.Header.Set(operationWaitHeader, version)
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, req)
					if w.Code != http.StatusOK || w.Header().Get(queuedDiagnosticHeader) != mode+"_paused" {
						t.Fatalf("%s schema %q: quota diagnostic hid pause: %d %v", suffix, version, w.Code, w.Header())
					}
				}
			}
			if before != snapshotTable(t, f, "review_operation_waits") {
				t.Fatal("pause diagnostic changed retained wait")
			}
		})
	}
}

// Retained audit history can be invalid without remaining a live account timer.
// The same sampled instant governs candidate selection and the admission guard.
func TestCLIAccountRetryDeadlineFutureHistory(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		offset                time.Duration
		corrupt, otherAccount bool
	}{
		{"before", -time.Nanosecond, false, false},
		{"exact", 0, false, false},
		{"after", time.Nanosecond, false, false},
		{"corrupt-before", -time.Nanosecond, true, false},
		{"corrupt-exact", 0, true, false},
		{"corrupt-after", time.Nanosecond, true, false},
		{"other-account-corrupt-before", -time.Nanosecond, true, true},
		{"other-account-corrupt-after", time.Nanosecond, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, a, _, now := cliWaitFixture(t)
			capability := "test-capability"
			if tc.otherAccount {
				capability = "other-account"
			}
			f.configure(t, otherAttemptProject, capability, 0, false)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
				t.Fatal(err)
			}
			due := cliWaitDue(t, cliWaitRead(t, f, a.OperationID))
			other := f.enqueue(t, otherAttemptProject, "future-history-cli", "cli", 2, 1)
			timerControl(t, f, a.OperationID, "cancel", "cancel-future-history")
			if tc.corrupt {
				admissionSQL(t, f, `UPDATE review_cli_preflight SET body_json='{}' WHERE attempt_id=?`, a.ID)
			}
			*now = due.Add(tc.offset)
			live := tc.offset < 0 && !tc.otherAccount
			invalid := live && tc.corrupt
			tables := []string{"review_operations", "review_execution_attempts", "review_execution_transitions", "review_operation_waits", "review_cli_preflight", "review_execution_reservations", "events", "receipts"}
			before := make(map[string]string)
			for _, table := range tables {
				before[table] = snapshotTable(t, f, table)
			}
			got, err := cliAccountRetryDeadline(t.Context(), f.store.DB(), f.e.capabilities[capability], *now)
			if invalid {
				if !isWaitUnavailable(err) || !got.IsZero() {
					t.Error("invalid live history was accepted", got, err)
				}
			} else if err != nil || live && !got.Equal(due) || !live && !got.IsZero() {
				t.Error("deadline does not describe only future account history", got, due, err)
			}
			pending, err := f.e.pending(t.Context())
			if invalid {
				if !isWaitUnavailable(err) {
					t.Error("pending accepted invalid live history", pending, err)
				}
			} else if err != nil || live && len(pending) != 0 || !live && (len(pending) != 1 || pending[0].ID != other.ID) {
				t.Error("pending used expired or unrelated account history", pending, err)
			}
			scope := f.scope(t, otherAttemptProject, f.other, false)
			if got := f.e.queuedDiagnostic(t.Context(), scope, other.ID, other.Revision) == "cli_quota_wait"; got != (live && !invalid) {
				t.Error("shared quota diagnostic differs from live validated deadline", got)
			}
			for _, table := range tables {
				if before[table] != snapshotTable(t, f, table) {
					t.Fatal("lookup changed retained history", table)
				}
			}
			claim, err := f.e.StartAttempt(t.Context(), otherAttemptProject, other.ID)
			if invalid {
				if !isWaitUnavailable(err) || claim.ID != "" {
					t.Error("claim accepted invalid live history", claim, err)
				}
			} else if err != nil || live && claim.ID != "" || !live && claim.ID == "" {
				t.Error("claim used expired or unrelated account history", claim, err)
			}
			for _, table := range []string{"review_operation_waits", "review_cli_preflight"} {
				if before[table] != snapshotTable(t, f, table) {
					t.Fatal("admission changed retained cooldown history", table)
				}
			}
			if tc.corrupt {
				if _, err := readOperationBase(t.Context(), f.store.DB(), attemptProject, a.OperationID); !isWaitUnavailable(err) {
					t.Fatal("historical read stopped validating the corrupt audit record", err)
				}
			} else {
				assertCLIWaitSnapshot(t, f)
			}
			assertCLIWaitNoCredit(t, f)
		})
	}
}

func TestCLIAccountRetryDeadlineLatestWithinSameMillisecond(t *testing.T) {
	f, a, _, now := cliWaitFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	// Leave room above creation times for two real retained exhaustion records.
	*now = now.Add(10 * time.Second).Truncate(time.Second)
	firstEnded := *now
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	firstDue := cliWaitDue(t, cliWaitRead(t, f, a.OperationID))
	timerControl(t, f, a.OperationID, "cancel", "cancel-first-deadline")
	*now = firstDue
	b := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "second-deadline", "cli", 2, 1))
	in := CLIAllowanceObservation{Anchor: attemptAnchor(otherAttemptProject, 2), ObservedAt: stamp(), Remaining: 0, Limit: 3, FullCapacityInMS: 86400000, RollingWindowMS: 86400000, ChangedFiles: 2}
	if err := f.e.RecordCLIExhaustion(t.Context(), otherAttemptProject, b.ID, b.Revision, in); err != nil {
		t.Fatal(err)
	}
	*now = firstEnded.Add(time.Nanosecond)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), otherAttemptProject, b.ID, b.Revision); err != nil {
		t.Fatal(err)
	}
	second, err := readOperation(t.Context(), f.store.DB(), otherAttemptProject, b.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	secondDue := cliWaitDue(t, second)
	if !secondDue.Equal(firstDue.Add(time.Nanosecond)) {
		t.Fatal("fixture did not retain adjacent deadlines", firstDue, secondDue)
	}
	assertCLIWaitSnapshot(t, f)
	for _, at := range []time.Time{firstDue.Add(-time.Nanosecond), firstDue, secondDue, secondDue.Add(time.Nanosecond)} {
		got, err := cliAccountRetryDeadline(t.Context(), f.store.DB(), f.e.capabilities["test-capability"], at)
		if err != nil || at.Before(secondDue) && !got.Equal(secondDue) || !at.Before(secondDue) && !got.IsZero() {
			t.Error("same-millisecond ordering lost the latest live deadline", at, got, err)
		}
	}
}

type cliRetryFailureQuery struct {
	EvidenceQuery
	match  string
	cancel bool
}

func (q cliRetryFailureQuery) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if strings.Contains(query, q.match) {
		if q.cancel {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			return q.EvidenceQuery.QueryRowContext(canceled, query, args...)
		}
		return q.EvidenceQuery.QueryRowContext(ctx, `SELECT * FROM missing_cli_retry_history`)
	}
	return q.EvidenceQuery.QueryRowContext(ctx, query, args...)
}

func TestCLIAccountRetryDeadlinePreservesLookupErrors(t *testing.T) {
	f, a, _, now := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	for _, match := range []string{"SELECT o.project_id,o.id", "FROM metadata"} {
		for _, canceled := range []bool{false, true} {
			q := cliRetryFailureQuery{EvidenceQuery: f.store.DB(), match: match, cancel: canceled}
			got, err := cliAccountRetryDeadline(t.Context(), q, f.e.capabilities["test-capability"], *now)
			if !got.IsZero() || err == nil || isWaitUnavailable(err) || canceled && !errors.Is(err, context.Canceled) || !canceled && !strings.Contains(err.Error(), "missing_cli_retry_history") {
				t.Error("account lookup concealed a real query failure", match, canceled, got, err)
			}
		}
	}
}

func TestCLIAccountRetryDeadlineRejectsNoncanonicalFuture(t *testing.T) {
	f, a, _, now := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	future := cliWaitDue(t, cliWaitRead(t, f, a.OperationID)).Add(time.Hour).Truncate(time.Second)
	for _, invalid := range []string{
		future.Format("2006-01-02T15:04:05-07:00"),      // UTC offset is not canonical Z.
		future.Format("2006-01-02T15:04:05.000000000Z"), // Redundant fractional zeros.
		future.Format(time.RFC3339Nano) + "Z",           // Invalid timestamp, not an expiry.
	} {
		admissionSQL(t, f, `UPDATE review_operation_waits SET retry_not_before=? WHERE operation_id=?`, invalid, a.OperationID)
		before := snapshotTable(t, f, "review_operation_waits")
		got, err := cliAccountRetryDeadline(t.Context(), f.store.DB(), f.e.capabilities["test-capability"], *now)
		if !got.IsZero() || !isWaitUnavailable(err) {
			t.Error("noncanonical future wait escaped retained validation", invalid, got, err)
		}
		tx, err := f.store.DB().BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = ValidateOperationWaitsSnapshot(t.Context(), tx)
		tx.Rollback()
		if !isWaitUnavailable(err) {
			t.Error("backup accepted noncanonical retained wait", invalid, err)
		}
		if before != snapshotTable(t, f, "review_operation_waits") {
			t.Fatal("validation rewrote invalid history")
		}
	}
}
