package reviewexec

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type cumulativeRoundHistory struct {
	Format  int          `json:"format"`
	Control RoundControl `json:"control"`
}

func cumulativeTapered(c RoundControl) bool {
	return c.Used >= c.Minimum && (c.Dry >= c.Taper || c.InitiallyTapered && c.Dry-c.Used == c.InitialDry)
}

// A deliberate threshold change requires the retained streak itself to meet
// that threshold. Historical imported stopping proof remains immutable but
// does not stand in for the newly selected clean-round target.
func effectiveRoundTapered(c roundRecord) bool {
	if c.taperSet {
		return c.Used >= c.Minimum && c.Dry >= c.Taper
	}
	return cumulativeTapered(c.RoundControl)
}

// cumulativeLegacyDry corrects the legacy frozen streak projection exactly once
// on an explicit format conversion. Only immutable, already-counted completions
// participate; completion credit, imported baseline and history are unchanged.
func cumulativeLegacyDry(ctx context.Context, q EvidenceQuery, c roundRecord) (int64, error) {
	projection := c
	projection.cumulative = true
	projection.Used = 0
	projection.Dry = c.InitialDry
	projection.Tapered = cumulativeTapered(projection.RoundControl)
	if err := advanceRoundReplay(ctx, q, &projection, c.Used); err != nil {
		return 0, err
	}
	return projection.Dry, nil
}

func validLegacyRoundControl(c RoundControl) bool {
	return validRoundControl(c) && (c.MaximumUnbounded || c.Maximum <= 1000) && (!c.InitiallyTapered || c.Used < c.Minimum || c.Tapered)
}
func encodeRoundHistory(c RoundControl, cumulative bool) string {
	if cumulative {
		return encoded(cumulativeRoundHistory{Format: 2, Control: c})
	}
	return encoded(c)
}
func decodeRoundHistory(body string) (RoundControl, bool, error) {
	if len(body) > 4096 {
		return RoundControl{}, false, roundUnavailable()
	}
	var c RoundControl
	if strictjson.Decode([]byte(body), &c) == nil && encoded(c) == body && validLegacyRoundControl(c) {
		return c, false, nil
	}
	var record cumulativeRoundHistory
	// A false taper latch can record an explicit reopening while keeping the
	// earned streak. Historical grants that kept taper remain unchanged.
	if strictjson.Decode([]byte(body), &record) != nil || record.Format != 2 || encoded(record) != body || !validRoundControl(record.Control) || record.Control.Tapered && !cumulativeTapered(record.Control) {
		return RoundControl{}, false, roundUnavailable()
	}
	return record.Control, true, nil
}

// initializeRoundControl installs only an absent first rule inside the caller's
// transaction. Existing explicit/adopted controls and accepted work are untouched.
func initializeRoundControl(ctx context.Context, tx *sql.Tx, scope core.Scope, pull int64, channel string, minimum, maximum int64) error {
	if !roundScope(scope.ProjectID, pull, channel) || minimum < 0 || maximum < 1 || maximum > 1000 || minimum > maximum {
		return invalid()
	}
	previous, err := readRoundRecord(ctx, tx, scope.ProjectID, pull, channel)
	if err != nil {
		return err
	}
	if previous.Configured {
		return nil
	}
	at := stamp()
	c := roundRecord{RoundControl: RoundControl{ProjectID: scope.ProjectID, PullNumber: pull, Channel: channel, Configured: true, Revision: 1, Epoch: 1, Minimum: minimum, Maximum: maximum, Taper: roundTaper(channel), ActorID: scope.PrincipalID, CreatedAt: at, UpdatedAt: at}, grantRevision: 1, cumulative: true}
	if !validRoundControl(c.RoundControl) {
		return invalid()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_round_changes(project_id,pull_number,channel,revision,epoch,action,body_json,worker_id) VALUES(?,?,?,1,1,'grant',?,?)`, scope.ProjectID, pull, channel, encodeRoundHistory(c.RoundControl, true), scope.WorkerID); err != nil {
		return err
	}
	if err = storeRoundRecord(ctx, tx, c); err != nil {
		return err
	}
	return core.AppendEvent(ctx, tx, scope, "reviews.rounds_configured", fmt.Sprintf("%d/%s", pull, channel), 1, map[string]any{"pull_number": pull, "channel": channel, "revision": 1})
}

// The indexed ledger narrows candidates before canonical capture decoding. Dirty
// authority is never filtered by capture fields, which may themselves be stale.
// SQL still examines retained pending rows; only full canonical decoding is
// restricted to complete, unfinalized candidates.
const roundUnfinalizedSQL = `SELECT p.operation_id FROM review_comparison_proofs p
JOIN review_round_operation_bindings b ON b.operation_id=p.operation_id
JOIN review_operations o ON o.id=p.operation_id
LEFT JOIN review_execution_evidence e ON e.id=p.evidence_id AND e.project_id=p.project_id AND e.operation_id=p.operation_id
LEFT JOIN review_adjudication_state s ON s.evidence_id=e.id AND s.project_id=e.project_id
WHERE p.project_id=? AND p.pull_number=? AND p.channel=? AND p.state=?
AND b.project_id=p.project_id AND b.pull_number=p.pull_number AND b.channel=p.channel AND b.epoch=?
AND p.operation_id!=? AND p.operation_id>? AND o.state='completed'
AND NOT EXISTS(SELECT 1 FROM review_round_completions done WHERE done.operation_id=p.operation_id)
AND (p.state='dirty' OR (json_extract(e.body_json,'$.capture.review.Complete')=1 AND json_extract(e.body_json,'$.capture.review.Outcome')='completed' AND COALESCE(json_extract(s.body_json,'$.finalized'),0)=0))
ORDER BY p.operation_id LIMIT 64`

// Preserve full capture validation for terminal-but-unfinalized reservations.
// No raw provider or review text is returned by this local read.
func roundUnfinalizedOccupancy(ctx context.Context, tx *sql.Tx, c roundRecord, excluding string) (int64, error) {
	var count int64
	// Separate state ranges retain index ordering without sorting retained history.
	for _, state := range []string{"dirty", "pending"} {
		after := ""
		for {
			rows, err := tx.QueryContext(ctx, roundUnfinalizedSQL, c.ProjectID, c.PullNumber, c.Channel, state, c.Epoch, excluding, after)
			if err != nil {
				return 0, err
			}
			ids := []string{}
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
				return 0, err
			}
			if state == "dirty" && len(ids) != 0 {
				return 0, roundUnavailable()
			}
			for _, id := range ids {
				proof, err := deriveComparisonProof(ctx, tx, c.ProjectID, id)
				if err != nil {
					return 0, err
				}
				if proof.evidence == "" {
					return 0, roundUnavailable()
				}
				evidence, adjudication, err := loadReviewAdjudication(ctx, tx, c.ProjectID, proof.evidence)
				if err != nil {
					return 0, err
				}
				if adjudication.Finalized || !evidence.Capture.Review.Complete || evidence.Capture.Review.Outcome != "completed" {
					return 0, roundUnavailable()
				}
				count++
				after = id
			}
			if len(ids) < 64 {
				break
			}
		}
	}
	return count, nil
}

// Original adoption carries earned progress; only a later explicit grant asks
// for another completion before the preserved streak may stop discovery again.
func roundTaperBoundary(bound RoundControl, used int64) bool {
	return bound.Tapered || used > bound.Used || bound.Revision == 1 && hasRoundOrigin(bound)
}
