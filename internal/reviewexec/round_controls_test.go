package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func roundFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := newAttemptFixture(t)
	if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	f.configure(t, attemptProject, "test-capability", 0, false)
	return f
}
func roundGrant(t *testing.T, f *attemptFixture, pull int64, channel string, revision, minimum, maximum int64, key string) RoundControlReceipt {
	t.Helper()
	body, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), key, pull, channel, RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum})
	if err != nil {
		t.Fatal(err)
	}
	var receipt RoundControlReceipt
	if json.Unmarshal(body, &receipt) != nil || receipt.PullNumber != pull || receipt.Channel != channel || receipt.Revision != revision+1 || receipt.Outcome != "granted" {
		t.Fatal("invalid grant receipt")
	}
	return receipt
}
func roundRead(t *testing.T, f *attemptFixture, pull int64, channel string) RoundControl {
	t.Helper()
	c, err := f.e.ReadRoundControl(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), pull, channel)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func roundTables(t *testing.T, f *attemptFixture) string {
	t.Helper()
	tables := []string{"review_round_controls", "review_round_changes", "review_round_operation_bindings", "review_round_completions", "events", "receipts"}
	out := []any{}
	for _, table := range tables {
		rows, err := f.store.DB().Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		data := [][]any{}
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			data = append(data, values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, data)
	}
	return encoded(out)
}
func roundHook(t *testing.T, f *attemptFixture, fn func(context.Context, *sql.Tx) error) {
	t.Helper()
	if err := f.store.BackgroundWrite(t.Context(), fn); err != nil {
		t.Fatal(err)
	}
}
func roundOperation(t *testing.T, f *attemptFixture, id string) Operation {
	t.Helper()
	op, err := readOperation(t.Context(), f.store.DB(), attemptProject, id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}
func roundBind(t *testing.T, f *attemptFixture, id string) {
	t.Helper()
	op := roundOperation(t, f, id)
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error { return bindRoundControl(ctx, tx, op) })
}
func roundCompleteHook(t *testing.T, f *attemptFixture, id string) {
	t.Helper()
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error { return completeRoundControl(ctx, tx, attemptProject, id) })
}

// Real native journal/capture/adjudication APIs establish eligible credit. No
// SQL fixture inserts completion, frozen counts or derived ready-cache rows.
func roundCapture(t *testing.T, f *attemptFixture, n int, accepted bool) (Attempt, string) {
	t.Helper()
	receipt := f.enqueue(t, attemptProject, fmt.Sprintf("round-operation-%d", n), "cli", 2, 1)
	roundBind(t, f, receipt.ID)
	a := f.start(t, attemptProject, receipt)
	anchor := attemptAnchor(attemptProject, 2)
	anchor.ParentSHA = anchor.BaseSHA
	var err error
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%064x", n+1)
	input := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{"internal/changed.go"}}}
	if accepted {
		input.Review.Findings = []reviewprovider.ParsedFinding{{Title: "Retained correction", Detail: "Source fact", Severity: "high", Path: "source.go", StartLine: 1, EndLine: 1}}
		input.Review.ReportedFindings = 1
	}
	captured, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "captured_native_result", ""); err != nil {
		t.Fatal(err)
	}
	return a, captured.ID
}
func roundFinalize(t *testing.T, f *attemptFixture, a Attempt, evidence string, n int, accepted bool) {
	t.Helper()
	previous := int64(0)
	if accepted {
		_, err := f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), fmt.Sprintf("round-decision-%d", n), evidence, FindingDecisionRequest{ExpectedRevision: &previous, Finding: 0, Disposition: "accepted", Reason: "Verified source correction"})
		if err != nil {
			t.Fatal(err)
		}
		previous = 1
	}
	if _, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), fmt.Sprintf("round-finalize-%d", n), evidence, FinalizeReviewRequest{ExpectedRevision: &previous}); err != nil {
		t.Fatal(err)
	}
	roundCompleteHook(t, f, a.OperationID)
}

func TestRoundControlsGrantCloseScopeReplayAndAuthority(t *testing.T) {
	f := roundFixture(t)
	before := roundTables(t, f)
	c := roundRead(t, f, 2, "cli")
	if c.Configured || c.Revision != 0 || c.Epoch != 0 || c.ProjectID != attemptProject || c.PullNumber != 2 || c.Channel != "cli" {
		t.Fatal("unconfigured state not explicit")
	}
	if roundTables(t, f) != before {
		t.Fatal("read allocated round state")
	}
	zero, min, max := int64(0), int64(0), int64(4)
	request := RoundControlRequest{ExpectedRevision: &zero, Action: "grant", Minimum: &min, Maximum: &max}
	writer := f.scope(t, attemptProject, f.writer, false)
	first, err := f.e.ConfigureRoundControl(t.Context(), writer, "original", 2, "cli", request)
	if err != nil {
		t.Fatal(err)
	}
	c = roundRead(t, f, 2, "cli")
	if c.Revision != 1 || c.Epoch != 1 || c.Closed || c.Minimum != 0 || c.Maximum != 4 || c.Taper != 3 || c.Used != 0 || c.Dry != 0 || c.Tapered || c.ActorID != writer.PrincipalID || c.CreatedAt == "" || c.UpdatedAt == "" {
		t.Fatal("new grant baseline incorrect")
	}
	roundGrant(t, f, 2, "hosted", 0, 0, 3, "hosted")
	if roundRead(t, f, 2, "hosted").Taper != 1 {
		t.Fatal("hosted default taper")
	}
	revision := int64(1)
	_, err = f.e.ConfigureRoundControl(t.Context(), writer, "close", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"})
	if err != nil {
		t.Fatal(err)
	}
	closed := roundRead(t, f, 2, "cli")
	if !closed.Closed || closed.Revision != 2 || closed.Epoch != 1 || roundRead(t, f, 2, "hosted").Closed || roundRead(t, f, 4, "cli").Configured {
		t.Fatal("closure changed unrelated scope or allowance")
	}
	before = roundTables(t, f)
	replay, err := f.e.ConfigureRoundControl(t.Context(), writer, "original", 2, "cli", request)
	if err != nil || string(replay) != string(first) || roundTables(t, f) != before {
		t.Fatal("exact old grant replay changed closed current state")
	}
	_, err = f.e.ConfigureRoundControl(t.Context(), writer, "stale", 2, "cli", request)
	attemptCode(t, err, "review_round_revision_conflict")
	changed := int64(5)
	request.Maximum = &changed
	_, err = f.e.ConfigureRoundControl(t.Context(), writer, "original", 2, "cli", request)
	attemptCode(t, err, "idempotency_conflict")
	reader := evidenceReadScope(t, f, attemptProject, f.reader)
	_, err = f.e.ConfigureRoundControl(t.Context(), reader, "reader-denied", 4, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"})
	attemptCode(t, err, "forbidden")
	foreign := evidenceReadScope(t, f, otherAttemptProject, f.other)
	foreign.ProjectID = attemptProject
	_, err = f.e.ReadRoundControl(t.Context(), foreign, 2, "cli")
	attemptCode(t, err, "forbidden")
	_, err = f.e.ConfigureRoundControl(t.Context(), foreign, "foreign-denied", 2, "cli", request)
	attemptCode(t, err, "forbidden")
	if roundTables(t, f) != before {
		t.Fatal("refused mutations changed state")
	}
	roundGrant(t, f, 2, "cli", 2, 1, 2, "reopen")
	c = roundRead(t, f, 2, "cli")
	if c.Epoch != 2 || c.Closed || c.Used != 0 || c.Dry != 0 || c.Tapered || c.CreatedAt != closed.CreatedAt {
		t.Fatal("new grant failed to reset only its epoch")
	}
	// A scope obtained before write permission was withdrawn cannot recover an
	// old successful receipt or make another change with its cached authority.
	if _, err = f.store.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id=(SELECT id FROM credentials WHERE principal_id=?) AND project_id=?`, writer.PrincipalID, attemptProject); err != nil {
		t.Fatal(err)
	}
	before = roundTables(t, f)
	request.Maximum = &max
	_, err = f.e.ConfigureRoundControl(t.Context(), writer, "original", 2, "cli", request)
	attemptCode(t, err, "forbidden")
	_, err = f.e.ConfigureRoundControl(t.Context(), writer, "after-write-revocation", 4, "cli", request)
	attemptCode(t, err, "forbidden")
	if roundTables(t, f) != before {
		t.Fatal("write-revoked scope changed state or replayed a receipt")
	}
	if _, err = f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE principal_id=?`, writer.PrincipalID); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.ConfigureRoundControl(t.Context(), writer, "original", 2, "cli", request)
	attemptCode(t, err, "unauthorized")
	if roundTables(t, f) != before {
		t.Fatal("revoked credential changed state")
	}
}

func TestRoundControlsInitialCloseAndValidation(t *testing.T) {
	f := roundFixture(t)
	admin := f.scope(t, attemptProject, f.admin, true)
	zero := int64(0)
	_, err := f.e.ConfigureRoundControl(t.Context(), admin, "close-before-grant", 2, "cli", RoundControlRequest{ExpectedRevision: &zero, Action: "close"})
	if err != nil {
		t.Fatal(err)
	}
	c := roundRead(t, f, 2, "cli")
	if !c.Configured || !c.Closed || c.Epoch != 1 || c.Maximum != 1 || c.Minimum != 0 || c.Used != 0 || c.Revision != 1 {
		t.Fatal("initial closed state")
	}
	before := roundTables(t, f)
	for _, request := range []RoundControlRequest{{ExpectedRevision: &zero, Action: "grant"}, {ExpectedRevision: &zero, Action: "grant", Minimum: adjudicationRevision(-1), Maximum: adjudicationRevision(2)}, {ExpectedRevision: &zero, Action: "grant", Minimum: adjudicationRevision(3), Maximum: adjudicationRevision(2)}, {ExpectedRevision: &zero, Action: "grant", Minimum: &zero, Maximum: adjudicationRevision(1001)}, {ExpectedRevision: &zero, Action: "close", Maximum: adjudicationRevision(1)}} {
		_, err = f.e.ConfigureRoundControl(t.Context(), admin, core.NewID("key"), 4, "cli", request)
		attemptCode(t, err, "validation_failed")
	}
	if before != roundTables(t, f) {
		t.Fatal("invalid round request changed state")
	}
}

func TestRoundControlsInflightRefusesNewGrant(t *testing.T) {
	for _, state := range []string{"queued", "preparing", "posting", "active", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			f := roundFixture(t)
			channel := "cli"
			if state == "posting" || state == "uncertain" {
				channel = "hosted"
			}
			roundGrant(t, f, 2, channel, 0, 0, 3, "grant")
			receipt := f.enqueue(t, attemptProject, "pending", channel, 2, 1)
			roundBind(t, f, receipt.ID)
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
			before := roundTables(t, f)
			revision, min, max := int64(1), int64(0), int64(2)
			_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "replacement", 2, channel, RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &min, Maximum: &max})
			attemptCode(t, err, "review_round_inflight")
			if before != roundTables(t, f) {
				t.Fatal("in-flight refusal lost grant/binding")
			}
		})
	}
}

func TestRoundControlsBindingCapacityAndClosedAdmission(t *testing.T) {
	f := roundFixture(t)
	unbound := f.enqueue(t, attemptProject, "unconfigured-operation", "hosted", 4, 1)
	roundBind(t, f, unbound.ID)
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_operation_bindings`) != 0 {
		t.Fatal("unconfigured accepted operation gained binding")
	}
	roundGrant(t, f, 2, "cli", 0, 0, 1, "single")
	receipt := f.enqueue(t, attemptProject, "single-operation", "cli", 2, 1)
	roundBind(t, f, receipt.ID)
	op := roundOperation(t, f, receipt.ID)
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		configured, err := admitRoundControl(ctx, tx, op)
		if err == nil && !configured {
			t.Fatal("configured admission returned legacy policy")
		}
		return err
	})
	// The grant has one occupied slot, yet admission must exclude this operation.
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_operation_bindings`) != 1 {
		t.Fatal("binding retry duplicated operation")
	}
	revision := int64(1)
	_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-queued", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"})
	if err != nil {
		t.Fatal(err)
	}
	before := roundTables(t, f)
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error { _, err := admitRoundControl(ctx, tx, op); return err })
	attemptCode(t, err, "review_round_closed")
	if before != roundTables(t, f) {
		t.Fatal("closed dispatch guard changed history")
	}
}

func TestRoundControlsFrozenAcceptedDryStreakAndTaperLatch(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 8, "discovery")
	var acceptedID string
	for i, accepted := range []bool{false, false, true, false, false, false} {
		a, id := roundCapture(t, f, i, accepted)
		roundFinalize(t, f, a, id, i, accepted)
		c := roundRead(t, f, 2, "cli")
		wantDry := []int64{1, 2, 0, 1, 2, 3}[i]
		if c.Used != int64(i+1) || c.Dry != wantDry || c.Tapered != (i == 5) {
			t.Fatalf("round%d wrong progress %+v", i, c)
		}
		before := roundTables(t, f)
		roundCompleteHook(t, f, a.OperationID)
		if before != roundTables(t, f) {
			t.Fatal("completion replay changed consumed allowance")
		}
		if accepted {
			acceptedID = id
			current := adjudicationRead(t, f, id)
			if current.FrozenCounts.Accepted != 1 {
				t.Fatal("accepted round lacks frozen count")
			}
			zero := ""
			if _, err := f.e.RecordFindingFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "fix-for-next-round", id, FindingFixRequest{ExpectedRevision: &current.Revision, Finding: 0, ExpectedSHA: &zero, FixSHA: strings.Repeat("f", 40), ProofNote: "Verified next-round prerequisite"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Correcting finalized fix proof cannot rewrite accepted discovery work
	// or reopen a grant that already tapered.
	state := adjudicationRead(t, f, acceptedID)
	before := roundRead(t, f, 2, "cli")
	if _, err := f.e.RecordFindingFix(t.Context(), f.scope(t, attemptProject, f.writer, false), "late-fix-correction", acceptedID, FindingFixRequest{ExpectedRevision: &state.Revision, Finding: 0, ExpectedSHA: adjudicationSHA(strings.Repeat("f", 40)), FixSHA: strings.Repeat("e", 40), ProofNote: "Later verified fix correction"}); err != nil {
		t.Fatal(err)
	}
	if got := roundRead(t, f, 2, "cli"); !reflect.DeepEqual(got, before) {
		t.Fatal("later correction rewrote completed discovery")
	}
}

func TestRoundControlsExactMinimumAndLateOldEpochCompletion(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 3, 3, "exact")
	for i := 0; i < 3; i++ {
		a, id := roundCapture(t, f, i, false)
		roundFinalize(t, f, a, id, i, false)
		c := roundRead(t, f, 2, "cli")
		if roundDone(c) != (i == 2) || c.Used != int64(i+1) {
			t.Fatal("exact grant ended before its minimum")
		}
	}
	roundGrant(t, f, 2, "cli", 1, 0, 2, "next-epoch")
	a, id := roundCapture(t, f, 4, true)
	// The completed operation is terminal but source decisions remain pending.
	c := roundRead(t, f, 2, "cli")
	if c.Used != 0 {
		t.Fatal("unfinalized native result supplied a round")
	}
	roundGrant(t, f, 2, "cli", 2, 0, 2, "fresh-epoch")
	current := roundRead(t, f, 2, "cli")
	roundFinalize(t, f, a, id, 4, true)
	if got := roundRead(t, f, 2, "cli"); !reflect.DeepEqual(got, current) {
		t.Fatal("late previous-epoch finalization spent the new allowance")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions WHERE project_id=? AND epoch=2`, attemptProject) != 1 {
		t.Fatal("late old grant lost its historical completion")
	}
}

func TestRoundControlsConfigurationRollbackAndCompletionNoCredit(t *testing.T) {
	f := roundFixture(t)
	before := roundTables(t, f)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER round_event_failure BEFORE INSERT ON events WHEN NEW.event_type='reviews.rounds_configured' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	zero, min, max := int64(0), int64(0), int64(2)
	admin := f.scope(t, attemptProject, f.admin, true)
	request := RoundControlRequest{ExpectedRevision: &zero, Action: "grant", Minimum: &min, Maximum: &max}
	if _, err := f.e.ConfigureRoundControl(t.Context(), admin, "retry-after-rollback", 2, "cli", request); err == nil {
		t.Fatal("event failure was not atomic")
	}
	if before != roundTables(t, f) {
		t.Fatal("event failure retained grant/audit/receipt")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER round_event_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.ConfigureRoundControl(t.Context(), admin, "retry-after-rollback", 2, "cli", request); err != nil {
		t.Fatal(err)
	}
	receipt := f.enqueue(t, attemptProject, "failed-native-operation", "cli", 2, 1)
	roundBind(t, f, receipt.ID)
	a := f.start(t, attemptProject, receipt)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", "local_failure", ""); err != nil {
		t.Fatal(err)
	}
	before = roundTables(t, f)
	roundCompleteHook(t, f, a.OperationID)
	if before != roundTables(t, f) || roundRead(t, f, 2, "cli").Used != 0 {
		t.Fatal("failed native attempt consumed completed round")
	}
}

func TestRoundControlsOriginalFrozenCountsSurviveDecisionCorrection(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 4, "grant")
	a, id := roundCapture(t, f, 0, true)
	zero := int64(0)
	if _, err := f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "initial-rejection", id, FindingDecisionRequest{ExpectedRevision: &zero, Finding: 0, Disposition: "rejected", Reason: "Initial source disposition"}); err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	if _, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "freeze-rejected", id, FinalizeReviewRequest{ExpectedRevision: &one}); err != nil {
		t.Fatal(err)
	}
	roundCompleteHook(t, f, a.OperationID)
	before := roundRead(t, f, 2, "cli")
	if before.Used != 1 || before.Dry != 1 {
		t.Fatal("original zero-accepted source did not count dry")
	}
	two := int64(2)
	if _, err := f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "later-acceptance", id, FindingDecisionRequest{ExpectedRevision: &two, Finding: 0, Disposition: "accepted", Reason: "Later source correction"}); err != nil {
		t.Fatal(err)
	}
	roundCompleteHook(t, f, a.OperationID)
	if after := roundRead(t, f, 2, "cli"); !reflect.DeepEqual(after, before) {
		t.Fatal("current decision rewrote frozen round progress")
	}
	if adjudicationRead(t, f, id).FrozenCounts.Accepted != 0 {
		t.Fatal("source correction changed original frozen counts")
	}
}

func TestRoundControlsClosePreservesPostedCompletion(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 2, "grant")
	a, id := roundCapture(t, f, 0, false)
	one := int64(1)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "close-completed-source", 2, "cli", RoundControlRequest{ExpectedRevision: &one, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	roundFinalize(t, f, a, id, 0, false)
	c := roundRead(t, f, 2, "cli")
	if !c.Closed || c.Revision != 2 || c.Epoch != 1 || c.Used != 1 || c.Dry != 1 {
		t.Fatal("close discarded original grant's finalized completion")
	}
}

func TestRoundControlsCompletionFailureRollsBackNativeFinalization(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 2, "grant")
	a, id := roundCapture(t, f, 0, false)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER round_completion_failure BEFORE INSERT ON review_round_completions BEGIN SELECT RAISE(ABORT,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	before, authority := roundTables(t, f), adjudicationState(t, f, a)
	zero := int64(0)
	scope := f.scope(t, attemptProject, f.writer, false)
	if _, err := f.e.FinalizeReviewEvidence(t.Context(), scope, "retry-finalization", id, FinalizeReviewRequest{ExpectedRevision: &zero}); err == nil {
		t.Fatal("completion storage failure did not fail native finalization")
	}
	if roundTables(t, f) != before || adjudicationState(t, f, a) != authority {
		t.Fatal("failed completion retained source/audit/receipt/progress changes")
	}
	if adjudicationRead(t, f, id).Finalized {
		t.Fatal("failed completion left source finalized")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER round_completion_failure`); err != nil {
		t.Fatal(err)
	}
	first, err := f.e.FinalizeReviewEvidence(t.Context(), scope, "retry-finalization", id, FinalizeReviewRequest{ExpectedRevision: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if c := roundRead(t, f, 2, "cli"); c.Used != 1 || c.Dry != 1 {
		t.Fatal("retried finalization did not count exactly one round")
	}
	after := roundTables(t, f)
	replay, err := f.e.FinalizeReviewEvidence(t.Context(), scope, "retry-finalization", id, FinalizeReviewRequest{ExpectedRevision: &zero})
	if err != nil || string(first) != string(replay) || after != roundTables(t, f) {
		t.Fatal("recovered native finalization duplicated completion")
	}
}
