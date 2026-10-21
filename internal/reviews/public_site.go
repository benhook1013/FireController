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
		rows, err := tx.QueryContext(ctx, `SELECT source,source_run_id,channel,complete,found,accepted,routed,started_at,finished_at FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=? AND outcome='completed' AND finalized=1 AND channel IN ('cli','hosted') ORDER BY run_order LIMIT ?`, project, snapshot, enrolled.Number, 2*publicsite.MaxChannelRounds+1)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var source, run, channel, started string
			round := publicsite.RoundFacts{Completed: true, Finalized: true}
			if err = rows.Scan(&source, &run, &channel, &round.Complete, &round.Found, &round.Accepted, &round.Routed, &started, &round.ObservedAt); err != nil {
				rows.Close()
				return nil, err
			}
			mac := hmac.New(sha256.New, key)
			// Length-prefixed fields avoid ambiguous joins; the per-publication
			// secret prevents these public references from exposing source IDs.
			for _, part := range []string{project, fmt.Sprint(enrolled.Number), source, run, channel} {
				fmt.Fprintf(mac, "%d:%s", len(part), part)
			}
			round.Key = "public_round_" + hex.EncodeToString(mac.Sum(nil)[:16])
			if start, e := time.Parse(time.RFC3339Nano, started); e == nil && started != "" && round.ObservedAt != "" {
				if end, e := time.Parse(time.RFC3339Nano, round.ObservedAt); e == nil && !end.Before(start) {
					duration := end.Sub(start).Milliseconds()
					round.DurationMilliseconds = &duration
				}
			}
			if channel == "cli" {
				fact.CLI = append(fact.CLI, round)
			} else {
				fact.Hosted = append(fact.Hosted, round)
			}
			if len(fact.CLI) > publicsite.MaxChannelRounds || len(fact.Hosted) > publicsite.MaxChannelRounds {
				rows.Close()
				return nil, errors.New("public review history exceeds selected capacity")
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return facts, nil
}
