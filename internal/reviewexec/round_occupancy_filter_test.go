package reviewexec

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRoundOccupancyFilterAuthorityAndScope(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			f := roundFixture(t)
			if legacy {
				seedLegacyRoundGrant(t, f, 2, "cli", 0, 0, 3, "first")
			} else {
				roundGrant(t, f, 2, "cli", 0, 0, 3, "first")
			}
			a, evidence := roundCapture(t, f, 0, false)
			check := func(mutate func(*roundRecord), excluding string, want int64) {
				t.Helper()
				roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
					c, err := readRoundRecord(ctx, tx, attemptProject, 2, "cli")
					if err != nil {
						return err
					}
					if mutate != nil {
						mutate(&c)
					}
					got, err := roundUnfinalizedOccupancy(ctx, tx, c, excluding)
					if err == nil && got != want {
						t.Errorf("occupancy=%d want %d", got, want)
					}
					return err
				})
			}
			check(nil, "", 1)
			check(nil, a.OperationID, 0)
			check(func(c *roundRecord) { c.Epoch++ }, "", 0)
			check(func(c *roundRecord) { c.PullNumber = 4 }, "", 0)
			check(func(c *roundRecord) { c.Channel = "hosted" }, "", 0)
			check(func(c *roundRecord) { c.ProjectID = otherAttemptProject }, "", 0)
			// Authority mutations invalidate the ledger even when JSON now looks non-occupying.
			if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=json_set(body_json,'$.capture.review.Complete',0) WHERE id=?`, evidence); err != nil {
				t.Fatal(err)
			}
			before := roundTables(t, f)
			revision, minimum, maximum := int64(1), int64(0), int64(3)
			_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.writer, false), "dirty", 2, "cli", RoundControlRequest{ExpectedRevision: &revision, Action: "grant", Minimum: &minimum, Maximum: &maximum})
			attemptCode(t, err, "storage_unavailable")
			if before != roundTables(t, f) {
				t.Fatal("dirty authority changed round state")
			}
		})
	}
}

func TestRoundOccupancyFilterRetainedHistory(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 1000, "first")
	// More than two former pages of valid bound terminal history. Missing,
	// incomplete, and failed captures must never reach full canonical decoding.
	for i := 0; i < 130; i++ {
		a := f.start(t, attemptProject, f.enqueue(t, attemptProject, fmt.Sprintf("history-%d", i), "cli", 2, 1))
		anchor := attemptAnchor(attemptProject, 2)
		anchor.ParentSHA = anchor.BaseSHA
		var err error
		a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, anchor, "")
		if err != nil {
			t.Fatal(err)
		}
		if i%3 != 0 {
			digest := fmt.Sprintf("%064x", i+100)
			outcome := "failed"
			if i%3 == 1 {
				outcome = "rate_limited"
			}
			input := ReviewEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024, ExpectedAnchor: anchor, StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), Review: reviewprovider.ParsedReview{Outcome: outcome, Complete: false, Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{}}}
			if _, err = f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "history", ""); err != nil {
			t.Fatal(err)
		}
	}
	a, evidence := roundCapture(t, f, 1000, false)
	roundHook(t, f, func(ctx context.Context, tx *sql.Tx) error {
		c, err := readRoundRecord(ctx, tx, attemptProject, 2, "cli")
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, roundUnfinalizedSQL, c.ProjectID, c.PullNumber, c.Channel, "pending", c.Epoch, "", "")
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) != 1 || ids[0] != a.OperationID {
			return fmt.Errorf("full-decode candidates=%v", ids)
		}
		got, err := roundUnfinalizedOccupancy(ctx, tx, c, "")
		if got != 1 {
			t.Errorf("occupancy=%d", got)
		}
		return err
	})
	roundFinalize(t, f, a, evidence, 1000, false)
	if got := roundPendingCount(t, f, 2, "cli"); got != 0 {
		t.Fatal("finalized occupancy", got)
	}
	roundSnapshotValid(t, f)
}

func TestRoundOccupancyFilterIndexedPlan(t *testing.T) {
	f := roundFixture(t)
	for _, state := range []string{"dirty", "pending"} {
		rows, err := f.store.DB().Query("EXPLAIN QUERY PLAN "+roundUnfinalizedSQL, attemptProject, 2, "cli", state, 1, "", "")
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
			if strings.Contains(detail, "SEARCH p USING INDEX review_comparison_proofs_state") && strings.Contains(detail, "project_id=? AND pull_number=? AND channel=? AND state=?") {
				found = true
			}
			if strings.Contains(detail, "SCAN ") || strings.Contains(detail, "USE TEMP B-TREE") {
				t.Fatal("history scan or sort", detail)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !found {
			t.Fatal("missing scoped index", err)
		}
	}
}

func TestRoundOccupancyFilterStillValidatesCapture(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 3, "first")
	_, evidence := roundCapture(t, f, 0, false)
	// Fault injection: preserve the cheap complete-result fields while corrupting
	// original attribution, then bypass invalidation to challenge canonical proof.
	if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=json_set(body_json,'$.capture.capture_sha256','invalid') WHERE id=?`, evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE review_comparison_proofs SET state='pending' WHERE evidence_id=?`, evidence); err != nil {
		t.Fatal(err)
	}
	err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		c, err := readRoundRecord(ctx, tx, attemptProject, 2, "cli")
		if err != nil {
			return err
		}
		_, err = roundUnfinalizedOccupancy(ctx, tx, c, "")
		return err
	})
	attemptCode(t, err, "storage_unavailable")
}

func TestRoundOccupancyFilterPagesCompleteResults(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 100, "first")
	for i := 0; i < 65; i++ {
		roundCapture(t, f, i, false)
	}
	if got := roundPendingCount(t, f, 2, "cli"); got != 65 {
		t.Fatalf("paged occupancy=%d want 65", got)
	}
	roundSnapshotValid(t, f)
}
