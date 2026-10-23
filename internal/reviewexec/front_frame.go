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
		out = append(out, SourcePlan{Repository: reviewprovider.Repository{ID: f.repository}, Pull: reviewprovider.Pull{RepositoryID: f.repository, Number: p.Number, State: p.State, Merged: p.Merged, HeadSHA: p.HeadSHA, BaseSHA: p.BaseSHA, BaseRef: p.BaseRef}, ParentPullNumber: p.ParentPullNumber, ParentSHA: p.ParentSHA, ObservedAt: p.ObservedAt, SourceReason: p.SourceReason})
	}
	return out
}
func validFrontFrame(f frontFrame, c Configuration, channel string) bool {
	if f.repository < 1 || f.configuration < 1 || !roundTime(f.checkedAt) || validateConfigurationShape(c) != nil || c.Paused || !slices.Contains(c.Channels, channel) || *c.ExpectedRevision != f.configuration-1 || len(f.prefix) > len(c.Queue) || len(f.prefix) > 100 || f.prefix == nil || (f.sourceReason != "" && (!identifier.MatchString(f.sourceReason) || len(f.sourceReason) > 128)) {
		return false
	}
	if f.complete != (len(f.prefix) == len(c.Queue) && f.sourceReason == "") || len(f.prefix) == 0 && f.sourceReason == "" {
		return false
	}
	observed := ""
	var latest time.Time
	for i, p := range f.prefix {
		if p.Number != c.Queue[i] || (p.State != "open" && p.State != "closed") || p.Merged && p.State != "closed" || !sha.MatchString(p.HeadSHA) || !sha.MatchString(p.BaseSHA) || !sha.MatchString(p.ParentSHA) || p.ParentSHA != p.BaseSHA || p.BaseRef == "" || !safeText(p.BaseRef, 256) || !roundTime(p.ObservedAt) || p.SourceReason != "" && !perPullSourceHold(p.SourceReason) {
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
	f.complete = len(f.prefix) == len(c.Queue) && f.sourceReason == ""
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
	var body string
	var complete int
	err := q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(repository_id)='integer' THEN repository_id END,CASE WHEN typeof(configuration_revision)='integer' THEN configuration_revision END,CASE WHEN typeof(checked_at)='text' AND length(CAST(checked_at AS BLOB))<=64 THEN checked_at END,CASE WHEN typeof(source_observed_at)='text' AND length(CAST(source_observed_at AS BLOB))<=64 THEN source_observed_at END,CASE WHEN typeof(complete)='integer' THEN complete END,CASE WHEN typeof(source_reason)='text' AND length(CAST(source_reason AS BLOB))<=128 THEN source_reason END,CASE WHEN typeof(prefix_json)='text' AND length(CAST(prefix_json AS BLOB))<=131072 THEN prefix_json END FROM review_front_frames WHERE project_id=? AND channel=?`, project, channel).Scan(&f.repository, &f.configuration, &f.checkedAt, &f.observedAt, &complete, &f.sourceReason, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	if err != nil {
		return f, false, runUnavailable()
	}
	if complete < 0 || complete > 1 || strictjson.Decode([]byte(body), &f.prefix) != nil {
		return f, false, runUnavailable()
	}
	f.complete = complete == 1
	canonical, err := json.Marshal(f.prefix)
	if err != nil || !bytes.Equal(canonical, []byte(body)) {
		return f, false, runUnavailable()
	}
	var configBody string
	if err = q.QueryRowContext(ctx, `SELECT CASE WHEN typeof(body_json)='text' AND length(CAST(body_json AS BLOB))<=16384 THEN body_json END FROM review_execution_config WHERE project_id=? AND revision=?`, project, f.configuration).Scan(&configBody); err != nil {
		return f, false, runUnavailable()
	}
	var c Configuration
	if strictjson.Decode([]byte(configBody), &c) != nil || !validFrontFrame(f, c, channel) {
		return f, false, runUnavailable()
	}
	return f, true, nil
}
