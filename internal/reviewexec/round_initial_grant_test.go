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

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRoundInitialGrantRefusesUnboundExecution(t *testing.T) {
	for _, state := range []string{"queued", "preparing", "posting", "active", "uncertain"} {
		for _, closed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/closed=%t", state, closed), func(t *testing.T) {
				f := roundFixture(t)
				channel := "cli"
				if state == "posting" || state == "uncertain" {
					channel = "hosted"
				}
				receipt := f.enqueue(t, attemptProject, "unbound", channel, 2, 1)
				if state != "queued" {
					a := f.start(t, attemptProject, receipt)
					if state != "preparing" {
						intent := ""
						if channel == "hosted" {
							intent = "original-posting-intent"
						}
						var err error
						a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), intent)
						if err != nil {
							t.Fatal(err)
						}
						if state == "uncertain" {
							if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, state, "ambiguous_post", ""); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				revision := int64(0)
				if closed {
					if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "close-before-grant", 2, channel, RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
						t.Fatal(err)
					}
					revision++
				}
				before, operation := roundTables(t, f), roundOperation(t, f, receipt.ID)
				minimum, maximum := int64(0), int64(1000)
				_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "first-grant", 2, channel, RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum})
				attemptCode(t, err, "review_round_inflight")
				if roundTables(t, f) != before || !reflect.DeepEqual(operation, roundOperation(t, f, receipt.ID)) || operation.State != state {
					t.Fatal("refused grant changed history, binding, receipt or accepted work")
				}
				if !closed {
					if configured, err := f.e.RoundAdmission(t.Context(), attemptProject, receipt.ID); err != nil || configured {
						t.Fatal("refused grant changed original unconfigured admission", configured, err)
					}
					if state == "queued" {
						f.start(t, attemptProject, receipt)
					}
				}
				roundSnapshotValid(t, f)
			})
		}
	}
}

func TestRoundInitialGrantWaitsForUnboundFinalization(t *testing.T) {
	for _, closed := range []bool{false, true} {
		for _, accepted := range []bool{false, true} {
			t.Run(fmt.Sprintf("closed=%t/accepted=%t", closed, accepted), func(t *testing.T) {
				f := roundFixture(t)
				a, evidence := roundCapture(t, f, 0, accepted)
				revision := int64(0)
				if closed {
					if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "close", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
						t.Fatal(err)
					}
					revision++
				}
				before := roundTables(t, f)
				minimum, maximum := int64(1), int64(1)
				request := RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum}
				scope := f.scope(t, attemptProject, f.writer, false)
				_, err := f.e.ConfigureRoundControl(t.Context(), scope, "first-grant", 2, "cli", request)
				attemptCode(t, err, "review_round_inflight")
				if roundTables(t, f) != before {
					t.Fatal("unfinalized refusal changed round state or retained a receipt")
				}
				roundFinalize(t, f, a, evidence, 0, accepted)
				receipt, err := f.e.ConfigureRoundControl(t.Context(), scope, "first-grant", 2, "cli", request)
				if err != nil {
					t.Fatal("settled unbound work still prevented initial grant", err)
				}
				c := roundRead(t, f, 2, "cli")
				if c.Closed || c.Used != 0 || c.Dry != 0 || c.Minimum != 1 || c.Maximum != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_round_operation_bindings WHERE operation_id=?`, a.OperationID) != 0 {
					t.Fatal("initial grant manufactured historical binding or completion", c)
				}
				before = roundTables(t, f)
				roundCompleteHook(t, f, a.OperationID)
				replayed, err := f.e.ConfigureRoundControl(t.Context(), scope, "first-grant", 2, "cli", request)
				if err != nil || string(receipt) != string(replayed) || roundTables(t, f) != before {
					t.Fatal("late callback or exact replay changed the initial grant", err)
				}
				if !accepted {
					next, id := roundCapture(t, f, 1, false)
					roundFinalize(t, f, next, id, 1, false)
					if c = roundRead(t, f, 2, "cli"); c.Used != 1 || !roundDone(c) {
						t.Fatal("new bound completion did not earn its round", c)
					}
				}
				roundSnapshotValid(t, f)
			})
		}
	}
}

func TestRoundInitialGrantIgnoresSettledAndIncompleteUnboundWork(t *testing.T) {
	for _, state := range []string{"held", "failed", "cancelled", "rate_limited", "completed-no-capture", "completed-incomplete"} {
		t.Run(state, func(t *testing.T) {
			f := roundFixture(t)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "old-work", "cli", 2, 1))
			terminal := state
			if strings.HasPrefix(state, "completed-") {
				terminal = "completed"
				anchor := attemptAnchor(attemptProject, 2)
				anchor.ParentSHA = anchor.BaseSHA
				var err error
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
				if err != nil {
					t.Fatal(err)
				}
				if state == "completed-incomplete" {
					digest := strings.Repeat("2", 64)
					input := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, ExpectedAnchor: anchor, StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), Review: reviewprovider.ParsedReview{Outcome: "failed", Complete: false, Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{}}}
					if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, terminal, "settled_old_work", ""); err != nil {
				t.Fatal(err)
			}
			roundGrant(t, f, 2, "cli", 0, 0, 1, "first")
			if c := roundRead(t, f, 2, "cli"); c.Used != 0 || c.Dry != 0 {
				t.Fatal("settled or incomplete work received credit", c)
			}
			roundSnapshotValid(t, f)
		})
	}
}

func TestRoundInitialGrantIgnoresOtherUnboundScopes(t *testing.T) {
	f := roundFixture(t)
	f.configure(t, otherAttemptProject, "test-capability", 0, false)
	f.enqueue(t, otherAttemptProject, "other-project", "cli", 2, 1)
	f.enqueue(t, attemptProject, "other-pull", "cli", 4, 1)
	f.enqueue(t, attemptProject, "other-channel", "hosted", 2, 1)
	roundGrant(t, f, 2, "cli", 0, 0, 1, "first")
	roundSnapshotValid(t, f)
}

func TestRoundInitialGrantIgnoresOtherUnfinalizedScopes(t *testing.T) {
	f := roundFixture(t)
	roundCapture(t, f, 0, false)
	roundGrant(t, f, 4, "cli", 0, 0, 1, "other-pull")
	roundGrant(t, f, 2, "hosted", 0, 0, 1, "other-channel")
	revision, minimum, maximum := int64(0), int64(0), int64(1)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, otherAttemptProject, f.other, false), "other-project", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum}); err != nil {
		t.Fatal("another project's unfinalized capture prevented the grant", err)
	}
	roundSnapshotValid(t, f)
}

func TestRoundInitialGrantPreservesLegacyBoundWorkOnReopening(t *testing.T) {
	f := roundFixture(t)
	seedLegacyRoundGrant(t, f, 2, "cli", 0, 0, 2, "legacy")
	a, evidence := roundCapture(t, f, 0, false)
	revision := int64(1)
	if _, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "close", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	roundGrant(t, f, 2, "cli", 2, 0, 1, "reopen")
	var grant, epoch int64
	if err := f.store.DB().QueryRow(`SELECT grant_revision,epoch FROM review_round_operation_bindings WHERE operation_id=?`, a.OperationID).Scan(&grant, &epoch); err != nil || grant != 1 || epoch != 1 || roundPendingCount(t, f, 2, "cli") != 1 {
		t.Fatal("reopening changed the original legacy binding or occupancy", grant, epoch, err)
	}
	roundFinalize(t, f, a, evidence, 0, false)
	if c := roundRead(t, f, 2, "cli"); c.Used != 1 || c.Dry != 1 || !roundDone(c) {
		t.Fatal("original legacy completion failed to consume reopened allowance", c)
	}
	roundSnapshotValid(t, f)
}

func TestRoundInitialGrantRefusesDirtyUnboundCapture(t *testing.T) {
	f := roundFixture(t)
	_, evidence := roundCapture(t, f, 0, false)
	if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=body_json WHERE id=?`, evidence); err != nil {
		t.Fatal(err)
	}
	before := roundTables(t, f)
	revision, minimum, maximum := int64(0), int64(0), int64(1)
	_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "dirty", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum})
	attemptCode(t, err, "storage_unavailable")
	if before != roundTables(t, f) {
		t.Fatal("dirty comparison authority allowed a grant mutation")
	}
}

func TestRoundInitialGrantQueriesUseScopedIndexes(t *testing.T) {
	f := roundFixture(t)
	for _, query := range []string{roundUnboundExecutionSQL, roundUnboundCaptureSQL} {
		rows, err := f.store.DB().Query("EXPLAIN QUERY PLAN "+query, attemptProject, 2, "cli")
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
			t.Log(detail)
			if strings.Contains(detail, "SEARCH o USING COVERING INDEX") || strings.Contains(detail, "SEARCH p USING INDEX review_comparison_proofs_") {
				found = strings.Contains(detail, "project_id=? AND pull_number=? AND channel=? AND state=?")
			}
			if strings.Contains(detail, "SCAN o") || strings.Contains(detail, "SCAN p") || strings.Contains(detail, "SCAN b") || strings.Contains(detail, "USE TEMP B-TREE") {
				t.Fatal("unbound occupancy scanned or sorted retained history", detail)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !found {
			t.Fatal("unbound occupancy did not use a scoped state index", err)
		}
	}
}

func TestRoundInitialGrantSerializesWithUnboundEnqueue(t *testing.T) {
	for i := range 8 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := roundFixture(t)
			scope := f.scope(t, attemptProject, f.writer, false)
			start := make(chan struct{})
			grant := make(chan error, 1)
			go func() {
				<-start
				revision, minimum, maximum := int64(0), int64(0), int64(1)
				_, err := f.e.ConfigureRoundControl(t.Context(), scope, "first", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum})
				grant <- err
			}()
			close(start)
			body, err := f.e.Enqueue(t.Context(), scope, "concurrent", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
			grantErr := <-grant
			if err != nil {
				t.Fatal(err)
			}
			var receipt Receipt
			if err := json.Unmarshal(body, &receipt); err != nil {
				t.Fatal(err)
			}
			configured, err := f.e.RoundAdmission(t.Context(), attemptProject, receipt.ID)
			if err != nil {
				t.Fatal("initial grant stranded a concurrent accepted request", err)
			}
			bindings := attemptCount(t, f.store, `SELECT count(*) FROM review_round_operation_bindings WHERE operation_id=?`, receipt.ID)
			if grantErr == nil {
				if !configured || bindings != 1 {
					t.Fatal("winning grant failed to bind the later enqueue", configured, bindings)
				}
			} else {
				attemptCode(t, grantErr, "review_round_inflight")
				if configured || bindings != 0 || roundRead(t, f, 2, "cli").Configured {
					t.Fatal("losing grant changed unconfigured work", configured, bindings)
				}
			}
			roundSnapshotValid(t, f)
		})
	}
}

func roundSnapshotValid(t *testing.T, f *attemptFixture) {
	t.Helper()
	if err := ValidateRoundSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func roundPendingCount(t *testing.T, f *attemptFixture, pull int64, channel string) int64 {
	t.Helper()
	var pending int64
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		c, err := readRoundRecord(ctx, tx, attemptProject, pull, channel)
		if err != nil {
			return err
		}
		pending, err = roundOccupied(ctx, tx, c, "")
		return err
	})
	return pending
}
