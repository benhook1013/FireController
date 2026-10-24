package reviews

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"reflect"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/publicsite"
)

// SourceMetadataPin binds an already visible PR to its display facts before
// provider I/O. Attempts and findings may advance independently of this pin.
// Its private fields prevent an in-process caller from inventing a baseline.
type SourceMetadataPin struct {
	project     string
	number      int64
	fingerprint string
}

func sourceMetadataFingerprint(body []any) string {
	// Exclude completeness, run/attempt counts and summaries: this publication
	// must preserve concurrent evidence while rejecting changed display facts.
	sum := sha256.Sum256([]byte(mustJSON([]any{body[:7], body[12]})))
	return hex.EncodeToString(sum[:])
}

// PinSourceMetadata uses indexed current references only. Missing/retired PRs
// are deliberately omitted; a provider read cannot reactivate queue entries.
func PinSourceMetadata(ctx context.Context, tx *sql.Tx, project string, numbers []int64) (map[int64]SourceMetadataPin, error) {
	if len(numbers) > maxQueuePulls {
		return nil, validationError("Select a bounded source prefix.")
	}
	pins := make(map[int64]SourceMetadataPin, len(numbers))
	for _, n := range numbers {
		var retired bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_retired WHERE project_id=? AND number=?)`, project, n).Scan(&retired); err != nil {
			return nil, err
		}
		if retired {
			continue
		}
		body, _, err := retainedTransferPullBody(ctx, tx, project, n)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		pins[n] = SourceMetadataPin{project, n, sourceMetadataFingerprint(body)}
	}
	return pins, nil
}

// PublishSourceMetadata reuses fresh, already-read source facts. It performs no
// provider work and grants no review permission. Only existing visible entries
// whose display facts still match their pre-I/O pin can change. Every evidence
// record, queue position, retirement and historical snapshot remains retained.
func PublishSourceMetadata(ctx context.Context, tx *sql.Tx, scope core.Scope, repositoryID int64, pins map[int64]SourceMetadataPin, pulls []PullInput) (Receipt, error) {
	return publishSourceMetadata(ctx, tx, scope, repositoryID, pins, pulls, nil)
}

func publishSourceMetadata(ctx context.Context, tx *sql.Tx, scope core.Scope, repositoryID int64, pins map[int64]SourceMetadataPin, pulls []PullInput, statuses map[int64]SourceStatus) (Receipt, error) {
	if len(pulls) > maxQueuePulls {
		return Receipt{}, validationError("Select a bounded source prefix.")
	}
	var actualRepo int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&actualRepo); err != nil {
		return Receipt{}, err
	}
	if actualRepo != repositoryID {
		return Receipt{}, validationError("Source repository identity changed.")
	}
	type change struct {
		number   int64
		position int
		body     []any
	}
	changes := []change{}
	seen := map[int64]bool{}
	for _, pull := range pulls {
		if pull.Number < 1 || pull.Number > 2147483647 || !validSHA(pull.HeadSHA) || !validSHA(pull.BaseSHA) || !oneOf(pull.State, "open", "merged", "closed", "unknown") {
			return Receipt{}, validationError("Source metadata contains invalid pull identity or state.")
		}
		if seen[pull.Number] || len(pull.Runs) != 0 || len(pull.Attempts) != 0 || pull.Complete || pull.CIState != "" || pull.Mergeability != "" {
			return Receipt{}, validationError("Source metadata contains unsupported evidence or summary fields.")
		}
		seen[pull.Number] = true
		pin, ok := pins[pull.Number]
		if !ok {
			continue
		}
		if pin.project != scope.ProjectID || pin.number != pull.Number {
			return Receipt{}, validationError("Source baseline belongs to another project or PR.")
		}
		var retired bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_retired WHERE project_id=? AND number=?)`, scope.ProjectID, pull.Number).Scan(&retired); err != nil {
			return Receipt{}, err
		}
		if retired {
			continue
		}
		old, position, err := retainedTransferPullBody(ctx, tx, scope.ProjectID, pull.Number)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return Receipt{}, err
		}
		if _, err := ReadPullMetadata(old[12].(string), old[1].(string)); err != nil {
			return Receipt{}, err
		}
		if sourceMetadataFingerprint(old) != pin.fingerprint {
			continue
		}
		if pull.Title == "" {
			pull.Title, _ = old[0].(string)
		}
		// Invalid optional provider display facts must not prevent the source
		// worker from settling. Validate retained same-head metadata first so
		// omitting a candidate cannot conceal corrupt inherited facts.
		if _, err := inheritedPullMetadata(old, PullInput{HeadSHA: pull.HeadSHA, State: pull.State}); err != nil {
			return Receipt{}, err
		}
		if !validText(pull.Title, 500, true) || !validText(pull.BaseRef, 256, false) || pullMetadata(pull).Validate(pull.State) != nil {
			continue
		}
		pull.CIState, pull.Mergeability = "unknown", "unknown"
		if old[2] == pull.HeadSHA && old[3] == pull.BaseRef && old[4] == pull.BaseSHA {
			pull.CIState, _ = old[5].(string)
			pull.Mergeability, _ = old[6].(string)
		}
		if status, ok := statuses[pull.Number]; ok {
			pull.Mergeability = status.Mergeability
			if status.CIState != "" {
				pull.CIState = status.CIState
			}
		}
		if err = validateObservationContent([]PullInput{pull}, nil, true); err != nil {
			return Receipt{}, err
		}
		metadata, err := inheritedPullMetadata(old, pull)
		if err != nil {
			return Receipt{}, err
		}
		body := append([]any(nil), old...)
		body[0], body[1], body[2], body[3], body[4], body[5], body[6], body[12] = pull.Title, pull.State, pull.HeadSHA, pull.BaseRef, pull.BaseSHA, pull.CIState, pull.Mergeability, metadata
		if old[2] != pull.HeadSHA {
			body[7] = int64(0)
		}
		if !reflect.DeepEqual(old, body) {
			changes = append(changes, change{pull.Number, position, body})
		}
	}
	if len(changes) == 0 {
		return Receipt{}, nil
	}
	var previous, observed string
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT w.snapshot_id,w.revision,s.observed_at FROM review_workspace w JOIN review_snapshots s ON s.project_id=w.project_id AND s.snapshot_id=w.snapshot_id WHERE w.project_id=?`, scope.ProjectID).Scan(&previous, &revision, &observed); err != nil {
		return Receipt{}, err
	}
	if revision == int64(^uint64(0)>>1) {
		return Receipt{}, core.Fail("revision_exhausted", "The review workspace revision cannot advance further.", 409)
	}
	id := core.NewID("review_snapshot")
	next := revision + 1
	fingerprint := sha256.Sum256([]byte(previous + "\x00" + mustJSON(pulls)))
	// A partial source refresh does not refresh other rows' observation age.
	if _, err := tx.ExecContext(ctx, `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, id, next, repositoryID, nativeSource, observed, time.Now().UTC().Format(time.RFC3339Nano), hex.EncodeToString(fingerprint[:])); err != nil {
		return Receipt{}, err
	}
	write := newHistorySnapshotWrite(scope.ProjectID, next)
	for _, c := range changes {
		if err := write.pull(ctx, tx, c.number, c.position, c.body); err != nil {
			return Receipt{}, err
		}
	}
	if err := write.applyCurrent(ctx, tx, id, revision); err != nil {
		return Receipt{}, err
	}
	if err := core.AppendEvent(ctx, tx, scope, "review.observed", id, next, map[string]any{"snapshot_id": id, "source": nativeSource}); err != nil {
		return Receipt{}, err
	}
	if err := publicsite.Refresh(ctx, tx, scope.ProjectID, PublicSiteReviewFacts); err != nil {
		return Receipt{}, err
	}
	return Receipt{ID: id, Revision: next, Outcome: "source_observed"}, nil
}
