package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func roundProgressPath(project string) string {
	return "/api/v1/projects/" + project + "/reviews/rounds/progress"
}

func readProgress(t *testing.T, f *attemptFixture, numbers ...int64) RoundProgress {
	t.Helper()
	progress, err := f.e.ReadRoundProgress(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), numbers)
	if err != nil {
		t.Fatal(err)
	}
	return progress
}

func TestRoundProgressStoredFactsAndCompactHTTPWithoutCapabilities(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 1, 4, "grant-cli")
	roundGrant(t, f, 2, "hosted", 0, 0, 2, "grant-hosted")
	a, evidence := roundCapture(t, f, 301, false)
	roundFinalize(t, f, a, evidence, 301, false)
	revision := int64(1)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "closed-cli", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	// Current allowance reads require neither an available provider catalog nor
	// a source observation. They must not reconstruct state from browser runs.
	var err error
	f.e, err = New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	before := roundTables(t, f) + policyState(t, f) + attemptState(t, f, a)
	w := f.request(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=4,2,2147483647", f.reader, "", "")
	var result RoundProgress
	var wire map[string]json.RawMessage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || json.Unmarshal(w.Body.Bytes(), &wire) != nil || len(wire) != 3 {
		t.Fatal("invalid compact progress response", w.Code, w.Body.String())
	}
	at, err := time.Parse(time.RFC3339Nano, result.EvaluatedAt)
	if err != nil || at.IsZero() || result.ProjectID != attemptProject || len(result.Items) != 3 {
		t.Fatal("missing local evaluation identity", result, err)
	}
	for i, number := range []int64{4, 2, 2147483647} {
		item := result.Items[i]
		if item.PullNumber != number || len(item.Channels) != 2 || item.Channels[0].Channel != "hosted" || item.Channels[1].Channel != "cli" {
			t.Fatal("request or channel order changed", item)
		}
		for _, channel := range item.Channels {
			var fields map[string]any
			raw, _ := json.Marshal(channel)
			if json.Unmarshal(raw, &fields) != nil || len(fields) != 12 {
				t.Fatal("channel field allowlist changed", string(raw))
			}
			for _, key := range []string{"channel", "configured", "revision", "epoch", "closed", "minimum", "maximum", "taper", "used", "dry", "tapered", "pending"} {
				if _, ok := fields[key]; !ok {
					t.Fatal("required compact field missing", key)
				}
			}
			if number != 2 && !reflect.DeepEqual(channel, RoundProgressChannel{Channel: channel.Channel}) {
				t.Fatal("missing grant gained allowance or pending credit", channel)
			}
		}
	}
	hosted, cli := result.Items[1].Channels[0], result.Items[1].Channels[1]
	if !hosted.Configured || hosted.Maximum != 2 || hosted.Taper != 1 || hosted.Closed || hosted.Pending != 0 {
		t.Fatal("hosted allowance changed", hosted)
	}
	if !cli.Configured || cli.Revision != 2 || cli.Epoch != 1 || !cli.Closed || cli.Minimum != 1 || cli.Maximum != 4 || cli.Taper != 3 || cli.Used != 1 || cli.Dry != 1 || cli.Tapered || cli.Pending != 0 {
		t.Fatal("native consumed/closed allowance changed", cli)
	}
	for _, private := range []string{"actor_id", "origin_", "capability", "evidence", "operation_id", "source", "ready", "coverage"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatal("progress exposed unrelated authority or private provenance", private)
		}
	}
	if after := roundTables(t, f) + policyState(t, f) + attemptState(t, f, a); after != before {
		t.Fatal("progress read changed controls, journal, receipts or events")
	}
}

func TestRoundProgressClosedCurrentEpochCountsAllLiveStates(t *testing.T) {
	for _, state := range []string{"queued", "preparing", "posting", "active", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			f := roundFixture(t)
			channel := "cli"
			if state == "posting" || state == "uncertain" {
				channel = "hosted"
			}
			roundGrant(t, f, 2, channel, 0, 0, 3, "grant")
			receipt := f.enqueue(t, attemptProject, "pending", channel, 2, 1)
			if state != "queued" {
				a := f.start(t, attemptProject, receipt)
				if state != "preparing" {
					intent := ""
					if channel == "hosted" {
						intent = "retained-posting-intent"
					}
					var err error
					a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), intent)
					if err != nil {
						t.Fatal(err)
					}
					if state == "uncertain" {
						if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "ambiguous_post", ""); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			revision := int64(1)
			if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-live", 2, channel, RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
				t.Fatal(err)
			}
			before := roundTables(t, f) + policyState(t, f)
			result := readProgress(t, f, 2)
			for _, actual := range result.Items[0].Channels {
				if actual.Channel == channel {
					if !actual.Closed || actual.Pending != 1 || actual.Epoch != 1 || actual.Revision != 2 || actual.Used != 0 {
						t.Fatal("closure hid live capacity or counted consumed credit", actual)
					}
				} else if actual.Configured || actual.Pending != 0 {
					t.Fatal("pending work crossed channels", actual)
				}
			}
			if before != roundTables(t, f)+policyState(t, f) {
				t.Fatal("pending read changed durable state")
			}
		})
	}
}

func TestRoundProgressEpochProjectPullAndUnboundIsolation(t *testing.T) {
	f := roundFixture(t)
	seedLegacyRoundGrant(t, f, 2, "cli", 0, 0, 4, "epoch-one")
	old := f.enqueue(t, attemptProject, "old-epoch", "cli", 2, 1)
	a := f.start(t, attemptProject, old)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "fixture_stop", ""); err != nil {
		t.Fatal(err)
	}
	seedLegacyRoundGrant(t, f, 2, "cli", 1, 1, 2, "epoch-two")
	current := f.enqueue(t, attemptProject, "current-epoch", "cli", 2, 1)
	// This retained-row fixture makes old-epoch filtering observable; ordinary
	// grant replacement refuses live work and is proved in its owning tests.
	admissionSQL(t, f, `UPDATE review_operations SET state='uncertain' WHERE id=?`, old.ID)
	if roundOperation(t, f, current.ID).State != "queued" {
		t.Fatal("current operation fixture is not pending")
	}
	// A legacy single-comparison request has no grant binding and must not be
	// counted as current-epoch work merely because its PR appears in the batch.
	f.enqueue(t, attemptProject, "unbound-hosted", "hosted", 4, 1)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	zero, minimum, maximum := int64(0), int64(0), int64(9)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, otherAttemptProject, f.admin, true), "other-grant", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "grant", Minimum: &minimum, Maximum: &maximum}); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, otherAttemptProject, "other-operation", "cli", 2, 1)
	before := roundTables(t, f) + policyState(t, f)
	result := readProgress(t, f, 4, 2)
	if c := result.Items[1].Channels[1]; c.Epoch != 2 || c.Maximum != 2 || c.Minimum != 1 || c.Pending != 1 || c.Used != 0 || c.Dry != 0 {
		t.Fatal("another epoch/project/PR changed current grant progress", c)
	}
	if c := result.Items[0].Channels[0]; c.Configured || c.Pending != 0 {
		t.Fatal("unbound native work fabricated a current allowance", c)
	}
	other, err := f.e.ReadRoundProgress(t.Context(), evidenceReadScope(t, f, otherAttemptProject, f.other), []int64{2})
	if err != nil || other.Items[0].Channels[1].Maximum != 9 || other.Items[0].Channels[1].Pending != 1 {
		t.Fatal("other project lost independent grant", other, err)
	}
	if before != roundTables(t, f)+policyState(t, f) {
		t.Fatal("isolated reads changed stored state")
	}
}

func TestRoundProgressStrictQueryAndEngineValidationBeforeControlReads(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 4, "grant")
	// A selected corrupted row would fail a real control read. Malformed input
	// must still be rejected locally rather than reaching that state read.
	admissionSQL(t, f, `UPDATE review_round_controls SET actor_id='invalid/actor' WHERE project_id=?`, attemptProject)
	before := roundTables(t, f) + policyState(t, f)
	numbers := make([]string, 101)
	for i := range numbers {
		numbers[i] = strconv.Itoa(i + 1)
	}
	queries := []string{"", "pull_numbers", "pull_numbers=", "pull_numbers=null", "pull_numbers=0", "pull_numbers=-1", "pull_numbers=01", "pull_numbers=%2B1", "pull_numbers=1.0", "pull_numbers=2147483648", "pull_numbers=1,", "pull_numbers=,1", "pull_numbers=1,,2", "pull_numbers=1,1", "pull_numbers=1,%202", "pull_numbers=1%0A", "pull_numbers=2&pull_numbers=4", "pull_numbers=2&unknown=x", "pull_numbers=%zz", "pull_numbers=1;other=x", "pull_numbers=" + strings.Join(numbers, ","), "pull_numbers=" + strings.Repeat("1", 4097)}
	for _, query := range queries {
		r := httptest.NewRequest(http.MethodGet, roundProgressPath(attemptProject), nil)
		r.URL.RawQuery = query
		r.Header.Set("Authorization", "Bearer "+f.reader)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"validation_failed"`) {
			t.Fatal("invalid query reached control read or was accepted", query, w.Code, w.Body.String())
		}
	}
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	tooMany := make([]int64, 101)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	for _, input := range [][]int64{nil, {}, {0}, {-1}, {2147483648}, {2, 2}, tooMany} {
		_, err := f.e.ReadRoundProgress(t.Context(), scope, input)
		attemptCode(t, err, "validation_failed")
	}
	if _, err := f.e.ReadRoundProgress(nil, scope, []int64{2}); err == nil {
		t.Fatal("nil context accepted")
	}
	w := f.request(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=4,2", f.reader, "", "")
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), `"items"`) {
		t.Fatal("corrupt control returned partial or fabricated progress", w.Code, w.Body.String())
	}
	if before != roundTables(t, f)+policyState(t, f) {
		t.Fatal("validation or corrupt-read refusal changed state")
	}
}

func TestRoundProgressReadAuthorizationRevocationAndBatchBound(t *testing.T) {
	f := roundFixture(t)
	path := roundProgressPath(attemptProject) + "?pull_numbers=2"
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	for _, test := range []struct{ name, path, token, instance string }{
		{"foreign-project", strings.Replace(path, attemptProject, otherAttemptProject, 1), f.reader, f.store.InstanceID()},
		{"foreign-credential", path, f.other, f.store.InstanceID()},
		{"missing-access", path, "", f.store.InstanceID()},
		{"wrong-instance", path, f.reader, "other-instance"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := roundTables(t, f) + policyState(t, f)
			r := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.token != "" {
				r.Header.Set("Authorization", "Bearer "+test.token)
			}
			r.Header.Set("X-FC-Instance", test.instance)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code < 400 || strings.Contains(w.Body.String(), `"items"`) || before != roundTables(t, f)+policyState(t, f) {
				t.Fatal("unauthorized read exposed data or mutated state", w.Code)
			}
		})
	}
	values := make([]string, 100)
	for i := range values {
		values[i] = strconv.Itoa(i + 1)
	}
	w := f.request(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers="+strings.Join(values, ","), f.reader, "", "")
	var result RoundProgress
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Items) != 100 {
		t.Fatal("maximum valid batch refused", w.Code)
	}
	admissionSQL(t, f, `UPDATE grants SET can_read=0 WHERE credential_id='exec-reader-cred'`)
	_, err := f.e.ReadRoundProgress(t.Context(), scope, []int64{2})
	attemptCode(t, err, "forbidden")
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
	_, err = f.e.ReadRoundProgress(t.Context(), scope, []int64{2})
	attemptCode(t, err, "unauthorized")
	if w := f.request(http.MethodGet, path, f.reader, "", ""); w.Code != http.StatusUnauthorized {
		t.Fatal("revoked HTTP reader retained access", w.Code)
	}
}

func TestRoundProgressReadDoesNotWaitForWriterAdmission(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 2, "grant")
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.store.BackgroundWrite(t.Context(), func(context.Context, *sql.Tx) error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	result, err := f.e.ReadRoundProgress(ctx, scope, []int64{2})
	cancel()
	close(release)
	writerErr := <-done
	if err != nil || writerErr != nil || len(result.Items) != 1 || !result.Items[0].Channels[1].Configured {
		t.Fatal("read waited for writer admission", result, err, writerErr)
	}
}

func TestRoundProgressQueuedHoldDropsPendingAtomicallyWithEvent(t *testing.T) {
	for _, failEvent := range []bool{false, true} {
		name := "normal"
		if failEvent {
			name = "event-failure-and-retry"
		}
		t.Run(name, func(t *testing.T) {
			f := roundFixture(t)
			roundGrant(t, f, 2, "cli", 0, 0, 3, "grant")
			receipt := f.enqueue(t, attemptProject, "queued-before-change", "cli", 2, 1)
			if c := readProgress(t, f, 2).Items[0].Channels[1]; c.Pending != 1 || c.Used != 0 {
				t.Fatal("queued operation was not included", c)
			}
			f.configure(t, attemptProject, "other-account", 1, false)
			if failEvent {
				admissionSQL(t, f, `CREATE TRIGGER progress_hold_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.operation_updated' BEGIN SELECT RAISE(ABORT,'fixture event failure'); END`)
				before := roundTables(t, f) + policyState(t, f)
				original := roundOperation(t, f, receipt.ID)
				if _, err := f.e.StartAttempt(t.Context(), attemptProject, receipt.ID); err == nil {
					t.Fatal("event failure did not reject the hold transaction")
				}
				if roundOperation(t, f, receipt.ID) != original || before != roundTables(t, f)+policyState(t, f) || readProgress(t, f, 2).Items[0].Channels[1].Pending != 1 {
					t.Fatal("failed event partially committed hold or pending count")
				}
				admissionSQL(t, f, `DROP TRIGGER progress_hold_event_failure`)
			}
			attempt, err := f.e.StartAttempt(t.Context(), attemptProject, receipt.ID)
			if err != nil || attempt.ID != "" {
				t.Fatal("configuration hold unexpectedly created an attempt", attempt, err)
			}
			operation := roundOperation(t, f, receipt.ID)
			if operation.State != "held" || operation.Reason != "configuration_changed" || operation.AttemptID != "" {
				t.Fatal("queued operation did not retain the hold", operation)
			}
			before := roundTables(t, f) + policyState(t, f)
			if c := readProgress(t, f, 2).Items[0].Channels[1]; c.Pending != 0 || c.Used != 0 || c.Dry != 0 {
				t.Fatal("held operation kept occupied capacity or gained completion", c)
			}
			if count := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.operation_updated' AND project_id=? AND resource_id=? AND revision=? AND payload_json='{}'`, attemptProject, receipt.ID, operation.Revision); count != 1 {
				t.Fatal("hold event identity/revision not committed exactly once", count)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || before != roundTables(t, f)+policyState(t, f) {
				t.Fatal("hold read created an attempt, receipt or event")
			}
		})
	}
}

func TestRoundProgressLedgerOnlyTerminalTransitionInvalidatesWithoutProjection(t *testing.T) {
	for _, failEvent := range []bool{false, true} {
		name := "normal"
		if failEvent {
			name = "event-failure-and-retry"
		}
		t.Run(name, func(t *testing.T) {
			f := attemptProjectionFixture(t, false)
			if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			roundGrant(t, f, 2, "cli", 0, 0, 3, "ledger-grant")
			receipt := f.enqueue(t, attemptProject, "ledger-only-pending", "cli", 2, 1)
			a := f.start(t, attemptProject, receipt)
			if c := readProgress(t, f, 2).Items[0].Channels[1]; c.Pending != 1 {
				t.Fatal("ledger-only action not pending", c)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != 0 {
				t.Fatal("missing PR fixture unexpectedly has a browser projection")
			}
			if failEvent {
				admissionSQL(t, f, `CREATE TRIGGER progress_terminal_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.operation_updated' BEGIN SELECT RAISE(ABORT,'fixture event failure'); END`)
				before := roundTables(t, f) + policyState(t, f) + attemptState(t, f, a)
				if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", "local_preparation_failed", ""); err == nil {
					t.Fatal("event failure allowed terminal state to commit")
				}
				if before != roundTables(t, f)+policyState(t, f)+attemptState(t, f, a) || readProgress(t, f, 2).Items[0].Channels[1].Pending != 1 {
					t.Fatal("failed invalidation partially changed ledger or pending count")
				}
				admissionSQL(t, f, `DROP TRIGGER progress_terminal_event_failure`)
			}
			terminal, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", "local_preparation_failed", "")
			if err != nil || terminal.State != "failed" {
				t.Fatal("ledger-only failure transition unavailable", terminal, err)
			}
			operation := roundOperation(t, f, receipt.ID)
			before := roundTables(t, f) + policyState(t, f) + attemptState(t, f, terminal)
			if c := readProgress(t, f, 2).Items[0].Channels[1]; c.Pending != 0 || c.Used != 0 || c.Dry != 0 {
				t.Fatal("terminal action stayed in flight or gained completion credit", c)
			}
			if count := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.operation_updated' AND project_id=? AND resource_id=? AND revision=? AND payload_json='{}'`, attemptProject, receipt.ID, operation.Revision); count != 1 {
				t.Fatal("ledger-only terminal state did not emit one compact matching event", count)
			}
			if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != 0 || before != roundTables(t, f)+policyState(t, f)+attemptState(t, f, terminal) {
				t.Fatal("terminal read fabricated display state or mutated retained facts")
			}
		})
	}
}

func TestRoundProgressLedgerOnlyHostedSettlementInvalidatesAtomically(t *testing.T) {
	f := attemptProjectionFixture(t, false)
	if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	// Bind to the current grant through normal acceptance, before any attempt
	// exists. Settlement must release pending capacity without final credit.
	roundGrant(t, f, 2, "hosted", 0, 0, 2, "settlement-grant")
	receipt := f.enqueue(t, attemptProject, "settlement-pending", "hosted", 2, 1)
	a := f.start(t, attemptProject, receipt)
	anchor := attemptAnchor(attemptProject, 2)
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "dispatch_interrupted", "")
	if err != nil {
		t.Fatal(err)
	}
	finished := time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	input := HostedObservationInput{CaptureBytes: 1024, StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: finished, ObservedAt: finished, ExpectedAnchor: anchor, Intent: intent}
	// Use the same complete retained-observation path as hostedSettlementFixture;
	// no direct state inserts bypass the public settlement admission guard.
	for i := range hostedObservationLimit {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint("controlled retained reference ", i))))
		input.CaptureSHA256, input.CaptureID = digest, "review_capture_"+digest[:32]
		if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil {
			t.Fatal(err)
		}
	}
	if c := readProgress(t, f, 2).Items[0].Channels[0]; c.Epoch != 1 || c.Pending != 1 || c.Used != 0 {
		t.Fatal("uncertain settlement candidate not pending in current epoch", c)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != 0 {
		t.Fatal("ledger-only settlement fixture unexpectedly has browser state")
	}
	admissionSQL(t, f, `CREATE TRIGGER progress_settlement_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.operation_updated' BEGIN SELECT RAISE(ABORT,'fixture event failure'); END`)
	before := roundTables(t, f) + policyState(t, f) + hostedObservationState(t, f, a)
	failed := hostedSettlementCall(t, f, a, "progress-settlement", hostedSettlementBody(a))
	if failed.Code != http.StatusInternalServerError {
		t.Fatal("failed invalidation allowed settlement", failed.Code, failed.Body.String())
	}
	if before != roundTables(t, f)+policyState(t, f)+hostedObservationState(t, f, a) || readProgress(t, f, 2).Items[0].Channels[0].Pending != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 {
		t.Fatal("failed settlement invalidation partially committed ledger, capacity or receipt")
	}
	admissionSQL(t, f, `DROP TRIGGER progress_settlement_event_failure`)
	settled := hostedSettlementCall(t, f, a, "progress-settlement", hostedSettlementBody(a))
	if settled.Code != http.StatusOK {
		t.Fatal("settlement retry failed", settled.Code, settled.Body.String())
	}
	operation := roundOperation(t, f, receipt.ID)
	if operation.State != "cancelled" || operation.Reason != "operator_verified_external_stop" || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 {
		t.Fatal("settlement did not release the original action", operation)
	}
	if c := readProgress(t, f, 2).Items[0].Channels[0]; c.Pending != 0 || c.Used != 0 || c.Dry != 0 || c.Epoch != 1 {
		t.Fatal("settlement kept pending capacity or fabricated credit", c)
	}
	if count := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.operation_updated' AND project_id=? AND resource_id=? AND revision=? AND payload_json='{}'`, attemptProject, receipt.ID, operation.Revision); count != 1 {
		t.Fatal("settlement did not emit one compact operation invalidation", count)
	}
	before = roundTables(t, f) + policyState(t, f) + hostedObservationState(t, f, a)
	replay := hostedSettlementCall(t, f, a, "progress-settlement", hostedSettlementBody(a))
	if replay.Code != http.StatusOK || replay.Body.String() != settled.Body.String() || before != roundTables(t, f)+policyState(t, f)+hostedObservationState(t, f, a) {
		t.Fatal("settlement replay duplicated events, receipts or credits", replay.Code, replay.Body.String())
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
		t.Fatal("settlement manufactured browser state or completed evidence")
	}
}

func TestRoundProgressLedgerOnlyStartupRecoveryInvalidatesAtomically(t *testing.T) {
	f := attemptProjectionFixture(t, false)
	if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	roundGrant(t, f, 2, "cli", 0, 0, 3, "recovery-cli-grant")
	roundGrant(t, f, 2, "hosted", 0, 0, 2, "recovery-hosted-grant")
	preparingReceipt := f.enqueue(t, attemptProject, "recovery-preparing", "cli", 2, 1)
	preparing := f.start(t, attemptProject, preparingReceipt)
	postingReceipt := f.enqueue(t, attemptProject, "recovery-posting", "hosted", 2, 1)
	posting := f.start(t, attemptProject, postingReceipt)
	var err error
	posting, err = f.e.Admit(t.Context(), attemptProject, posting.ID, posting.Revision, attemptAnchor(attemptProject, 2), "retained-recovery-intent")
	if err != nil {
		t.Fatal(err)
	}
	for _, channel := range readProgress(t, f, 2).Items[0].Channels {
		if channel.Epoch != 1 || channel.Pending != 1 || channel.Used != 0 {
			t.Fatal("startup fixture lacks current-epoch pending work", channel)
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != 0 {
		t.Fatal("ledger-only startup fixture unexpectedly has browser state")
	}
	admissionSQL(t, f, `CREATE TRIGGER progress_recovery_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.operation_updated' BEGIN SELECT RAISE(ABORT,'fixture event failure'); END`)
	before := roundTables(t, f) + policyState(t, f) + attemptState(t, f, preparing) + attemptState(t, f, posting)
	if err := f.e.RecoverInterrupted(t.Context()); err == nil {
		t.Fatal("failed invalidation allowed startup cancellation")
	}
	if before != roundTables(t, f)+policyState(t, f)+attemptState(t, f, preparing)+attemptState(t, f, posting) {
		t.Fatal("failed startup invalidation partially changed attempts or capacity")
	}
	admissionSQL(t, f, `DROP TRIGGER progress_recovery_event_failure`)
	if err := f.e.RecoverInterrupted(t.Context()); err != nil {
		t.Fatal("startup recovery retry failed", err)
	}
	cancelled := roundOperation(t, f, preparingReceipt.ID)
	uncertain := roundOperation(t, f, postingReceipt.ID)
	if cancelled.State != "cancelled" || cancelled.Reason != "supervisor_interrupted" || uncertain.State != "uncertain" || uncertain.Reason != "supervisor_interrupted" {
		t.Fatal("startup recovery changed the wrong lifecycle states", cancelled, uncertain)
	}
	progress := readProgress(t, f, 2)
	if c := progress.Items[0].Channels[1]; c.Pending != 0 || c.Used != 0 || c.Dry != 0 || c.Epoch != 1 {
		t.Fatal("preparing cancellation kept pending capacity or gained credit", c)
	}
	if c := progress.Items[0].Channels[0]; c.Pending != 1 || c.Used != 0 || c.Dry != 0 || c.Epoch != 1 {
		t.Fatal("ambiguous hosted dispatch incorrectly released capacity or gained credit", c)
	}
	if count := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.operation_updated' AND project_id=? AND resource_id=? AND revision=? AND payload_json='{}'`, attemptProject, preparingReceipt.ID, cancelled.Revision); count != 1 {
		t.Fatal("startup cancellation lacks one compact matching invalidation", count)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, preparing.ID) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, posting.ID) != 1 {
		t.Fatal("startup recovery released the wrong reservations")
	}
	before = roundTables(t, f) + policyState(t, f) + attemptState(t, f, preparing) + attemptState(t, f, posting)
	if err := f.e.RecoverInterrupted(t.Context()); err != nil || before != roundTables(t, f)+policyState(t, f)+attemptState(t, f, preparing)+attemptState(t, f, posting) {
		t.Fatal("repeated recovery duplicated events, transitions or credit", err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_snapshots`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.observed'`) != 0 {
		t.Fatal("startup recovery manufactured a browser projection")
	}
}
