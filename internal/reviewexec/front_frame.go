package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxFrontPrefixBytes = 128 * 1024

// frontPull deliberately omits provider titles, statistics, captures and bodies.
type frontPull struct {
	Number               int64  `json:"number"`
	VisibleAtAcquisition bool   `json:"visible_at_acquisition"`
	State                string `json:"state"`
	Merged               bool   `json:"merged"`
	HeadSHA              string `json:"head_sha"`
	BaseSHA              string `json:"base_sha"`
	BaseRef              string `json:"base_ref"`
	ParentPullNumber     int64  `json:"parent_pull_number"`
	ParentSHA            string `json:"parent_sha"`
	ObservedAt           string `json:"observed_at"`
	SourceReason         string `json:"source_reason,omitempty"`
	// Selection order is retained on the first entry only when it differs
	// from configured order. Old frames keep their original exact-prefix proof.
	SelectionOrder []int64 `json:"selection_order,omitempty"`
}
type frontFrame struct {
	repository, configuration           int64
	checkedAt, observedAt, sourceReason string
	complete                            bool
	prefix                              []frontPull
}

func (f frontFrame) plans() []SourcePlan {
	out := make([]SourcePlan, 0, len(f.prefix))
	for _, p := range f.prefix {
		out = append(out, SourcePlan{Repository: reviewprovider.Repository{ID: f.repository}, Pull: reviewprovider.Pull{RepositoryID: f.repository, Number: p.Number, State: p.State, Merged: p.Merged, HeadSHA: p.HeadSHA, BaseSHA: p.BaseSHA, BaseRef: p.BaseRef}, ParentPullNumber: p.ParentPullNumber, ParentSHA: p.ParentSHA, ObservedAt: p.ObservedAt, SourceReason: p.SourceReason, schedulingPaused: p.SourceReason == "pull_scheduling_paused"})
	}
	return out
}
func validFrontFrame(f frontFrame, c Configuration, channel string) bool {
	if f.repository < 1 || f.configuration < 1 || !roundTime(f.checkedAt) || validateConfigurationShape(c) != nil || c.Paused || !slices.Contains(c.Channels, channel) || *c.ExpectedRevision != f.configuration-1 || len(f.prefix) > len(c.Queue) || len(f.prefix) > 100 || f.prefix == nil || (f.sourceReason != "" && (!identifier.MatchString(f.sourceReason) || len(f.sourceReason) > 128)) {
		return false
	}
	hasPaused := slices.ContainsFunc(f.prefix, func(p frontPull) bool { return p.SourceReason == "pull_scheduling_paused" })
	if f.complete != (len(f.prefix) == len(c.Queue) && f.sourceReason == "" && !hasPaused) || len(f.prefix) == 0 && f.sourceReason == "" && len(c.Queue) > 0 {
		return false
	}
	observed := ""
	var latest time.Time
	queue := c.Queue
	if len(f.prefix) > 0 && f.prefix[0].SelectionOrder != nil {
		queue = f.prefix[0].SelectionOrder
		if len(queue) != len(c.Queue) {
			return false
		}
		seen := make(map[int64]bool, len(queue))
		for _, number := range queue {
			if seen[number] || !slices.Contains(c.Queue, number) {
				return false
			}
			seen[number] = true
		}
	}
	for i, p := range f.prefix {
		inOrder := p.Number == queue[i]
		if i > 0 && p.SelectionOrder != nil {
			return false
		}
		if p.SourceReason == "pull_scheduling_paused" {
			if !inOrder || p.State != "" || p.Merged || p.HeadSHA != "" || p.BaseSHA != "" || p.BaseRef != "" || p.ParentPullNumber != 0 || p.ParentSHA != "" || p.ObservedAt != "" {
				return false
			}
			continue
		}
		if !inOrder || (p.State != "open" && p.State != "closed") || p.Merged && p.State != "closed" || !sha.MatchString(p.HeadSHA) || !sha.MatchString(p.BaseSHA) || !sha.MatchString(p.ParentSHA) || p.ParentSHA != p.BaseSHA || p.BaseRef == "" || !safeText(p.BaseRef, 256) || !roundTime(p.ObservedAt) || p.SourceReason != "" && !perPullSourceHold(p.SourceReason) {
			return false
		}
		if p.ParentPullNumber < 0 || p.ParentPullNumber > 2147483647 || p.ParentPullNumber == p.Number || c.Mode == "independent" && p.ParentPullNumber != 0 {
			return false
		}
		if p.ParentPullNumber != 0 {
			for _, parent := range f.prefix {
				if parent.Number == p.ParentPullNumber && (parent.State != "open" || parent.Merged || parent.HeadSHA != p.ParentSHA) {
					return false
				}
			}
		}

		at, _ := time.Parse(time.RFC3339Nano, p.ObservedAt)
		if observed == "" || at.After(latest) {
			latest = at
			observed = p.ObservedAt
		}
	}
	return f.observedAt == observed
}
func writeFrontFrame(ctx context.Context, tx *sql.Tx, r Run, repo int64, c Configuration, choice runChoice) error {
	f := frontFrame{repository: repo, configuration: r.ConfigurationRevision, checkedAt: stamp(), sourceReason: choice.sourceReason, prefix: []frontPull{}}
	var hasProjection bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='review_current_pulls')`).Scan(&hasProjection); err != nil {
		return err
	}
	for _, p := range choice.prefix {
		visible := false
		if hasProjection {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM review_current_pulls WHERE project_id=? AND number=?)`, r.ProjectID, p.Pull.Number).Scan(&visible); err != nil {
				return err
			}
		}
		f.prefix = append(f.prefix, frontPull{Number: p.Pull.Number, VisibleAtAcquisition: visible, State: p.Pull.State, Merged: p.Pull.Merged, HeadSHA: p.Pull.HeadSHA, BaseSHA: p.Pull.BaseSHA, BaseRef: p.Pull.BaseRef, ParentPullNumber: p.ParentPullNumber, ParentSHA: p.ParentSHA, ObservedAt: p.ObservedAt, SourceReason: p.SourceReason})
		if p.ObservedAt == "" {
			continue
		}
		if f.observedAt == "" {
			f.observedAt = p.ObservedAt
		} else {
			a, _ := time.Parse(time.RFC3339Nano, f.observedAt)
			b, _ := time.Parse(time.RFC3339Nano, p.ObservedAt)
			if b.After(a) {
				f.observedAt = p.ObservedAt
			}
		}
	}
	f.complete = len(f.prefix) == len(c.Queue) && f.sourceReason == "" && !slices.ContainsFunc(choice.prefix, func(p SourcePlan) bool { return p.schedulingPaused })
	if len(f.prefix) > 0 && choice.orderedQueue != nil && !slices.Equal(choice.orderedQueue, c.Queue) {
		f.prefix[0].SelectionOrder = choice.orderedQueue
	}
	body, err := json.Marshal(f.prefix)
	if err != nil {
		return err
	}
	// Malformed/overbudget source can replace only with explicit unavailable;
	// never make an otherwise valid admission depend on retaining rich facts.
	if len(body) > maxFrontPrefixBytes || !validFrontFrame(f, c, r.Channel) {
		f.prefix = []frontPull{}
		f.complete = false
		f.observedAt = ""
		f.sourceReason = "source_unavailable"
		body = []byte("[]")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO review_front_frames(project_id,channel,repository_id,configuration_revision,checked_at,source_observed_at,complete,source_reason,prefix_json) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(project_id,channel) DO UPDATE SET repository_id=excluded.repository_id,configuration_revision=excluded.configuration_revision,checked_at=excluded.checked_at,source_observed_at=excluded.source_observed_at,complete=excluded.complete,source_reason=excluded.source_reason,prefix_json=excluded.prefix_json`, r.ProjectID, r.Channel, f.repository, f.configuration, f.checkedAt, f.observedAt, f.complete, f.sourceReason, string(body))
	return err
}
func readFrontFrame(ctx context.Context, q EvidenceQuery, project, channel string) (frontFrame, bool, error) {
	var f frontFrame
	var repository, configuration, complete sql.NullInt64
	var checked, observed, reason, body sql.NullString
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(repository_id)='integer' THEN repository_id END,CASE WHEN typeof(configuration_revision)='integer' THEN configuration_revision END,CASE WHEN typeof(checked_at)='text' AND length(CAST(checked_at AS BLOB))<=64 THEN checked_at END,CASE WHEN typeof(source_observed_at)='text' AND length(CAST(source_observed_at AS BLOB))<=64 THEN source_observed_at END,CASE WHEN typeof(complete)='integer' THEN complete END,CASE WHEN typeof(source_reason)='text' AND length(CAST(source_reason AS BLOB))<=128 THEN source_reason END,CASE WHEN typeof(prefix_json)='text' AND length(CAST(prefix_json AS BLOB))<=131072 THEN prefix_json END FROM review_front_frames WHERE project_id=? AND channel=?`, project, channel).Scan(&repository, &configuration, &checked, &observed, &complete, &reason, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	// Guarded NULLs are malformed project data, not query/transaction failures.
	if !repository.Valid || !configuration.Valid || !checked.Valid || !observed.Valid || !complete.Valid || !reason.Valid || !body.Valid || complete.Int64 < 0 || complete.Int64 > 1 || strictjson.Decode([]byte(body.String), &f.prefix) != nil {
		return f, false, runUnavailable()
	}
	f.repository, f.configuration = repository.Int64, configuration.Int64
	f.checkedAt, f.observedAt, f.sourceReason = checked.String, observed.String, reason.String
	f.complete = complete.Int64 == 1
	canonical, err := json.Marshal(f.prefix)
	if err != nil || !bytes.Equal(canonical, []byte(body.String)) {
		return f, false, runUnavailable()
	}
	var configBody sql.NullString
	err = q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(body_json)='text' AND length(CAST(body_json AS BLOB))<=16384 THEN body_json END FROM review_execution_config WHERE project_id=? AND revision=?`, project, f.configuration).Scan(&configBody)
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, runUnavailable()
	}
	if err != nil {
		return f, false, err
	}
	var c Configuration
	if !configBody.Valid || strictjson.Decode([]byte(configBody.String), &c) != nil || !validFrontFrame(f, c, channel) {
		return f, false, runUnavailable()
	}
	return f, true, nil
}
