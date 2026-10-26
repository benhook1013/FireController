package reviewexec

import (
	"context"
	"database/sql"
	"strings"
)

// Local positions explain remaining queue work, never source readiness. Read
// the complete configured prefix once so a browser history window cannot turn
// a known merged predecessor into an unknown gap.
type localFrontPull struct {
	state, head, base, ref string
}

func localFrontFallback(value FrontChannel) bool {
	if value.State == "held" && value.Reason == "reviews_paused" {
		return true
	}
	if value.State != "unknown" {
		return false
	}
	switch value.Reason {
	case "source_frame_unavailable", "configuration_changed", "visible_source_changed", "source_prefix_incomplete":
		return true
	}
	return false
}

func readLocalFrontPulls(ctx context.Context, tx *sql.Tx, project, snapshot string, queue []int64) (map[int64]localFrontPull, error) {
	args := []any{project, snapshot}
	for _, number := range queue {
		args = append(args, number)
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.number,p.state,p.head_sha,p.base_sha,p.base_ref FROM review_pulls p
 WHERE p.project_id=? AND p.snapshot_id=? AND p.delivery_selected=1 AND p.number IN (`+strings.TrimSuffix(strings.Repeat("?,", len(queue)), ",")+`)
 AND NOT EXISTS(SELECT 1 FROM review_current_retired r WHERE r.project_id=p.project_id AND r.number=p.number)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pulls := make(map[int64]localFrontPull, len(queue))
	for rows.Next() {
		var number int64
		var p localFrontPull
		if err := rows.Scan(&number, &p.state, &p.head, &p.base, &p.ref); err != nil {
			return nil, err
		}
		pulls[number] = p
	}
	return pulls, rows.Err()
}

func localChannelFront(ctx context.Context, tx *sql.Tx, project string, queue []int64, channel string, pulls map[int64]localFrontPull) (FrontChannel, bool, error) {
	for _, number := range queue {
		p, found := pulls[number]
		if !found || p.state == "unknown" {
			break
		}
		if p.state == "merged" || p.state == "closed" {
			continue
		}
		if p.state != "open" || !sha.MatchString(p.head) || !sha.MatchString(p.base) || !safeText(p.ref, 256) || p.ref == "" {
			break
		}
		rounds, err := readRoundControl(ctx, tx, project, number, channel)
		if err != nil {
			return FrontChannel{}, false, err
		}
		if rounds.Configured && (rounds.Closed || roundDone(rounds)) {
			continue
		}
		return FrontChannel{Channel: channel, State: "held", Reason: "local_queue_position", PullNumber: number, HeadSHA: p.head, BaseSHA: p.base, BaseRef: p.ref, CheckedAt: stamp()}, true, nil
	}
	return FrontChannel{}, false, nil
}
