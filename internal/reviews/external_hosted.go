package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/publicsite"
)

var externalHostedIdentity = regexp.MustCompile(`^(?:run|publication):[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// PublishExternalHosted retains independently verified provider results only.
// The caller owns collection completeness, original provider identity/head and
// rechecked enrollment. This creates no attempt, native credit or round state.
// changed means one additive archive snapshot was published; repeats are inert.
func PublishExternalHosted(ctx context.Context, tx *sql.Tx, scope core.Scope, repositoryID, number int64, runs []RunInput) (Receipt, bool, error) {
	if len(runs) > maxPullRuns {
		return Receipt{}, false, validationError("Select one bounded external hosted batch.")
	}
	visible, err := externalHostedVisible(ctx, tx, scope.ProjectID, repositoryID, number)
	if err != nil || !visible {
		return Receipt{}, false, err
	}
	cutoff, err := readExternalHostedFrontier(ctx, tx, scope.ProjectID, repositoryID, number)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, false, transferConflict("Initialize hosted discovery before collecting provider results.")
	}
	if err != nil {
		return Receipt{}, false, err
	}
	var previous, observed string
	var revision int64
	if err = tx.QueryRowContext(ctx, `SELECT w.snapshot_id,w.revision,s.observed_at FROM review_workspace w JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id WHERE w.project_id=?`, scope.ProjectID).Scan(&previous, &revision, &observed); err != nil {
		return Receipt{}, false, err
	}
	current := Receipt{ID: previous, Revision: revision, Outcome: "observed"}
	old, position, err := retainedTransferPullBody(ctx, tx, scope.ProjectID, number)
	if err != nil {
		return Receipt{}, false, err
	}
	// Only retained metadata participates in validation; incoming provider results
	// cannot alter a PR's current title, state, comparison, CI or completeness.
	text := make([]string, 7)
	for i := range text {
		var ok bool
		text[i], ok = old[i].(string)
		if !ok {
			return Receipt{}, false, historyInconsistent()
		}
	}
	p := PullInput{Number: number, Title: text[0], State: text[1], HeadSHA: text[2], BaseRef: text[3], BaseSHA: text[4], CIState: text[5], Mergeability: text[6], Runs: append([]RunInput(nil), runs...)}
	for i := range p.Runs {
		p.Runs[i].Findings = append([]Finding(nil), p.Runs[i].Findings...)
		for j := range p.Runs[i].Findings {
			if p.Runs[i].Findings[j].Disposition == "" {
				p.Runs[i].Findings[j].Disposition = "unadjudicated"
			}
		}
	}
	// Normalize only our copied input, never caller-owned source findings.
	copied := []PullInput{p}
	if err = validateObservationContent(copied, nil, true); err != nil {
		return Receipt{}, false, err
	}
	p = copied[0]
	for _, run := range p.Runs {
		if !externalHostedIdentity.MatchString(run.SourceRunID) || run.Channel != "hosted" || run.HeadSHA == "" || run.FinishedAt == "" || !run.Complete || run.Outcome != "completed" || run.Finalized || run.Accepted != 0 || run.Routed != 0 {
			return Receipt{}, false, validationError("External hosted results require original provider identity, head and finish without adjudication credit.")
		}
		for _, f := range run.Findings {
			if f.Disposition != "unadjudicated" || f.DecisionReason != "" || f.Actor != "" || f.DecidedAt != "" || f.FixSHA != "" || f.ProofNote != "" || f.ResolvedAt != "" || f.Route != nil {
				return Receipt{}, false, validationError("External discovery cannot supply local finding decisions, routes or fixes.")
			}
		}
	}
	body, err := json.Marshal(p.Runs)
	if err != nil || len(body) > maxObservationBytes {
		return Receipt{}, false, validationError("External hosted results exceed the bounded encoded batch.")
	}
	var frontier time.Time
	if cutoff != "" {
		frontier, _ = time.Parse(time.RFC3339Nano, cutoff)
	}
	added := make([]RunInput, 0, len(p.Runs))
	for _, run := range p.Runs {
		finished, _ := parseTimestamp(run.FinishedAt, true)
		if finished.IsZero() {
			return Receipt{}, false, validationError("External hosted results require a genuine finish time.")
		}
		if !frontier.IsZero() && !finished.After(frontier) {
			continue
		}
		// This exact identity lookup includes omitted historical/imported versions.
		// No text similarity, current-head assumption or native-evidence aliasing.
		var owner int64
		var channel, head string
		err = tx.QueryRowContext(ctx, `SELECT pull_number,channel,CASE WHEN length(CAST(head_sha AS BLOB))<=40 THEN head_sha END FROM review_run_versions INDEXED BY review_run_versions_external_source WHERE project_id=? AND source_run_id=? LIMIT 1`, scope.ProjectID, run.SourceRunID).Scan(&owner, &channel, &head)
		if err == nil {
			if owner != number || channel != "hosted" || head != run.HeadSHA {
				return Receipt{}, false, transferConflict("A retained external provider identity belongs to different source facts.")
			}
			// Imported history may have reused this ID in another version. A
			// matching first row cannot make contradictory retained identity safe.
			var conflicting bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_run_versions INDEXED BY review_run_versions_external_source WHERE project_id=? AND source_run_id=? AND (pull_number!=? OR channel!='hosted' OR head_sha!=?))`, scope.ProjectID, run.SourceRunID, number, run.HeadSHA).Scan(&conflicting); err != nil {
				return Receipt{}, false, err
			}
			if conflicting {
				return Receipt{}, false, transferConflict("Retained external provider identity has contradictory source facts.")
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Receipt{}, false, err
		}
		added = append(added, run)
	}
	if len(added) == 0 {
		return current, false, nil
	}
	if revision == math.MaxInt64 {
		return Receipt{}, false, core.Fail("revision_exhausted", "The review workspace revision cannot advance further.", 409)
	}
	id := core.NewID("review_snapshot")
	next := revision + 1
	recorded := time.Now().UTC().Format(time.RFC3339Nano)
	digest := sha256.Sum256([]byte(previous + "\x00" + mustJSON(added)))
	if _, err = tx.ExecContext(ctx, `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, id, next, repositoryID, externalHostedSource, observed, recorded, hex.EncodeToString(digest[:])); err != nil {
		return Receipt{}, false, err
	}
	write := newHistorySnapshotWrite(scope.ProjectID, next)
	reads := newHistoryReads(tx)
	defer reads.close()
	write.reads = reads
	var order int
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(run_order),-1)+1 FROM review_current_runs WHERE project_id=? AND pull_number=?`, scope.ProjectID, number).Scan(&order); err != nil {
		return Receipt{}, false, err
	}
	for _, run := range added {
		if err = write.run(ctx, tx, externalHostedSource, number, order, run); err != nil {
			return Receipt{}, false, err
		}
		order++
	}
	if err = rebuildNativeSummary(ctx, tx, write, id, number, position, old); err != nil {
		return Receipt{}, false, err
	}
	if err = write.applyCurrent(ctx, tx, id, revision); err != nil {
		return Receipt{}, false, err
	}
	for _, run := range added {
		if err = checkNativeRunDetail(ctx, tx, scope.ProjectID, id, runOpaqueID(scope.ProjectID, externalHostedSource, run.SourceRunID)); err != nil {
			return Receipt{}, false, err
		}
	}
	if err = core.AppendEvent(ctx, tx, scope, "review.observed", id, next, map[string]any{"snapshot_id": id, "source": externalHostedSource, "pull_number": number, "imported_history": true, "native_coverage": false}); err != nil {
		return Receipt{}, false, err
	}
	if err = publicsite.Refresh(ctx, tx, scope.ProjectID, PublicSiteReviewFacts); err != nil {
		return Receipt{}, false, err
	}
	return Receipt{ID: id, Revision: next, Outcome: "observed"}, true, nil
}
