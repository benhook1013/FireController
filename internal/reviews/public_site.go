package reviews

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/publicsite"
)

// PublicSiteReviewFacts is a deliberately narrow projection adapter. It reads
// only enrolled PRs and finalized completed CLI/Hosted summaries. Titles, links,
// snapshots, attempt activity, identities and provider text never cross it.
func PublicSiteReviewFacts(ctx context.Context, tx *sql.Tx, project string, manifest publicsite.Manifest, salt string) ([]publicsite.ReviewFacts, error) {
	key, err := hex.DecodeString(strings.TrimPrefix(salt, "public_salt_"))
	if !strings.HasPrefix(salt, "public_salt_") || err != nil || len(key) != 16 {
		return nil, errors.New("public review references unavailable")
	}
	var snapshot string
	if err = tx.QueryRowContext(ctx, `SELECT snapshot_id FROM review_workspace WHERE project_id=?`, project).Scan(&snapshot); errors.Is(err, sql.ErrNoRows) {
		return []publicsite.ReviewFacts{}, nil
	} else if err != nil {
		return nil, err
	}
	facts := make([]publicsite.ReviewFacts, 0, len(manifest.Reviews))
	for _, enrolled := range manifest.Reviews {
		fact := publicsite.ReviewFacts{Number: enrolled.Number, CLI: []publicsite.RoundFacts{}, Hosted: []publicsite.RoundFacts{}}
		err = tx.QueryRowContext(ctx, `SELECT state FROM review_pulls WHERE project_id=? AND snapshot_id=? AND number=?`, project, snapshot, enrolled.Number).Scan(&fact.Lifecycle)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		fact.CLI, fact.CLIHasMore, err = publicSiteChannelRounds(ctx, tx, project, enrolled.Number, "cli", key)
		if err != nil {
			return nil, err
		}
		fact.Hosted, fact.HostedHasMore, err = publicSiteChannelRounds(ctx, tx, project, enrolled.Number, "hosted", key)
		if err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return facts, nil
}

// Current references provide position ordering independently for each channel.
// Filtering may inspect other current rows; only 101 eligible scalar summaries
// cross this query boundary, and no capture or finding bodies are decoded.
const publicSiteChannelRoundsSQL = `SELECT v.source,v.source_run_id,v.complete,v.found,v.accepted,v.routed,v.started_at,v.finished_at
 FROM review_current_runs c
 CROSS JOIN review_run_memberships m ON m.project_id=c.project_id AND m.id=c.id AND m.start_revision=c.start_revision
 CROSS JOIN review_run_versions v ON v.project_id=m.project_id AND v.id=m.id AND v.version_revision=m.version_revision
 WHERE c.project_id=? AND c.pull_number=? AND v.outcome='completed' AND v.finalized=1 AND v.channel=?
 ORDER BY c.run_order DESC LIMIT ?`

func publicSiteChannelRounds(ctx context.Context, tx *sql.Tx, project string, number int64, channel string, key []byte) ([]publicsite.RoundFacts, bool, error) {
	rows, err := tx.QueryContext(ctx, publicSiteChannelRoundsSQL, project, number, channel, publicsite.MaxChannelRounds+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	rounds := make([]publicsite.RoundFacts, 0, publicsite.MaxChannelRounds)
	more := false
	for rows.Next() {
		var source, run, started string
		round := publicsite.RoundFacts{Completed: true, Finalized: true}
		if err := rows.Scan(&source, &run, &round.Complete, &round.Found, &round.Accepted, &round.Routed, &started, &round.ObservedAt); err != nil {
			return nil, false, err
		}
		if len(rounds) == publicsite.MaxChannelRounds {
			more = true
			continue
		}
		mac := hmac.New(sha256.New, key)
		// Preserve the original length-prefixed, publication-keyed identity.
		for _, part := range []string{project, fmt.Sprint(number), source, run, channel} {
			fmt.Fprintf(mac, "%d:%s", len(part), part)
		}
		round.Key = "public_round_" + hex.EncodeToString(mac.Sum(nil)[:16])
		if start, e := time.Parse(time.RFC3339Nano, started); e == nil && started != "" && round.ObservedAt != "" {
			if end, e := time.Parse(time.RFC3339Nano, round.ObservedAt); e == nil && !end.Before(start) {
				duration := end.Sub(start).Milliseconds()
				round.DurationMilliseconds = &duration
			}
		}
		rounds = append(rounds, round)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return rounds, more, nil
}
