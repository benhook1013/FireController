package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"math"
	"sync"
	"time"
)

// This is bounded operational telemetry, never admission authority or review evidence.
const decisionHistoryLimit = 256
const decisionHistoryDays = 7
const decisionCandidatesLimit = 100

type SelectionCandidateDecision struct {
	PullNumber         int64  `json:"pull_number"`
	HeadSHA            string `json:"head_sha,omitempty"`
	Outcome            string `json:"outcome"`
	Reason             string `json:"reason,omitempty"`
	BlockingPullNumber int64  `json:"blocking_pull_number,omitempty"`
}
type SelectionDecision struct {
	Sequence           int64                        `json:"sequence"`
	Phase              string                       `json:"phase"`
	SelectionID        string                       `json:"selection_id,omitempty"`
	Outcome            string                       `json:"outcome"`
	Reason             string                       `json:"reason,omitempty"`
	SelectedPullNumber int64                        `json:"selected_pull_number,omitempty"`
	Candidates         []SelectionCandidateDecision `json:"candidates"`
	FirstSeenAt        string                       `json:"first_seen_at"`
	LastSeenAt         string                       `json:"last_seen_at"`
	Occurrences        int64                        `json:"occurrences"`
}
type SelectionDecisionHistory struct {
	OmittedRecentPasses int                 `json:"omitted_recent_passes"`
	ProjectID           string              `json:"project_id"`
	Channel             string              `json:"channel"`
	RetentionDays       int                 `json:"retention_days"`
	MaximumEntries      int                 `json:"maximum_entries"`
	Items               []SelectionDecision `json:"items"`
	HasMore             bool                `json:"has_more"`
}
type decisionTrace struct {
	candidates []SelectionCandidateDecision
	reason     string
}
type decisionTraceKey struct{}

func traceDecision(ctx context.Context, number int64, head, outcome, reason string, blocking int64) {
	t, _ := ctx.Value(decisionTraceKey{}).(*decisionTrace)
	if t == nil || number <= 0 {
		return
	}
	d := SelectionCandidateDecision{number, head, outcome, reason, blocking}
	for i := range t.candidates {
		if t.candidates[i].PullNumber == number {
			t.candidates[i] = d
			return
		}
	}
	if len(t.candidates) < decisionCandidatesLimit {
		t.candidates = append(t.candidates, d)
	}
}
func traceReason(ctx context.Context, reason string) {
	if t, ok := ctx.Value(decisionTraceKey{}).(*decisionTrace); ok {
		t.reason = reason
	}
}
func ensureDecisionHistorySchema(ctx context.Context, s *core.Store) error {
	return s.EnsureSchema(ctx, "review_selection_decision_history", DecisionHistorySchemaStatements())
}

// DecisionHistorySchemaStatements supplies exact schema definitions to backup validation.
func DecisionHistorySchemaStatements() []string {
	return []string{
		`CREATE TABLE review_selection_decision_history(sequence INTEGER PRIMARY KEY AUTOINCREMENT,project_id TEXT NOT NULL REFERENCES projects(id),channel TEXT NOT NULL CHECK(channel IN ('cli','hosted')),phase TEXT NOT NULL CHECK(phase IN ('selection','automatic_plan')),fingerprint TEXT NOT NULL,body TEXT NOT NULL CHECK(length(CAST(body AS BLOB))<=65536),first_seen_at TEXT NOT NULL,last_seen_at TEXT NOT NULL,occurrences INTEGER NOT NULL CHECK(occurrences>=1))`,
		`CREATE INDEX review_selection_decision_lane ON review_selection_decision_history(project_id,channel,sequence DESC)`,
	}
}
func writeSelectionDecision(ctx context.Context, tx *sql.Tx, project, channel string, d SelectionDecision, now time.Time) error {
	if !validChannel(channel) || !validSelectionDecision(d) {
		return invalid()
	}
	if d.Candidates == nil {
		d.Candidates = []SelectionCandidateDecision{}
	}
	d.Sequence = 0
	d.FirstSeenAt = ""
	d.LastSeenAt = ""
	d.Occurrences = 0
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(body) > 65536 {
		return invalid()
	}
	// A new selection identity is preserved rather than merged with an older selection.
	sum := sha256.Sum256(body)
	fingerprint := hex.EncodeToString(sum[:])
	at := now.UTC().Format(time.RFC3339Nano)
	cutoff := now.Add(-decisionHistoryDays * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `DELETE FROM review_selection_decision_history WHERE project_id=? AND channel=? AND last_seen_at<?`, project, channel, cutoff); err != nil {
		return err
	}
	var seq, count int64
	var previous, previousLast string
	err = tx.QueryRowContext(ctx, `SELECT sequence,fingerprint,occurrences,last_seen_at FROM review_selection_decision_history WHERE project_id=? AND channel=? ORDER BY sequence DESC LIMIT 1`, project, channel).Scan(&seq, &previous, &count, &previousLast)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	lastTime, lastErr := time.Parse(time.RFC3339Nano, previousLast)
	if err == nil && previous == fingerprint && count < math.MaxInt64 && lastErr == nil && !now.Before(lastTime) {
		_, err = tx.ExecContext(ctx, `UPDATE review_selection_decision_history SET last_seen_at=?,occurrences=occurrences+1 WHERE sequence=?`, at, seq)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO review_selection_decision_history(project_id,channel,phase,fingerprint,body,first_seen_at,last_seen_at,occurrences) VALUES(?,?,?,?,?,?,?,1)`, project, channel, d.Phase, fingerprint, string(body), at, at)
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM review_selection_decision_history WHERE project_id=? AND channel=? AND sequence NOT IN (SELECT sequence FROM review_selection_decision_history WHERE project_id=? AND channel=? ORDER BY sequence DESC LIMIT ?)`, project, channel, project, channel, decisionHistoryLimit)
	return err
}
func (e *Engine) ReadSelectionDecisionHistory(ctx context.Context, scope core.Scope, channel string, before int64, limit int) (SelectionDecisionHistory, error) {
	out := SelectionDecisionHistory{ProjectID: scope.ProjectID, Channel: channel, RetentionDays: decisionHistoryDays, MaximumEntries: decisionHistoryLimit, Items: []SelectionDecision{}}
	if !validChannel(channel) || before < 0 || limit < 1 || limit > 100 {
		return out, invalid()
	}
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT sequence,CASE WHEN length(CAST(body AS BLOB))<=65536 THEN body END,first_seen_at,last_seen_at,occurrences FROM review_selection_decision_history WHERE project_id=? AND channel=? AND (?=0 OR sequence<?) AND last_seen_at>=? ORDER BY sequence DESC LIMIT ?`, scope.ProjectID, channel, before, before, time.Now().Add(-decisionHistoryDays*24*time.Hour).UTC().Format(time.RFC3339Nano), limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d SelectionDecision
			var body string
			var seq, count int64
			var first, last string
			if err = rows.Scan(&seq, &body, &first, &last, &count); err != nil {
				return err
			}
			if len(body) > 65536 || json.Unmarshal([]byte(body), &d) != nil || !validSelectionDecision(d) {
				return runUnavailable()
			}
			d.Sequence = seq
			d.FirstSeenAt = first
			d.LastSeenAt = last
			d.Occurrences = count
			firstTime, firstErr := time.Parse(time.RFC3339Nano, first)
			lastTime, lastErr := time.Parse(time.RFC3339Nano, last)
			if firstErr != nil || lastErr != nil || lastTime.Before(firstTime) || count < 1 {
				return runUnavailable()
			}
			if len(out.Items) == limit {
				out.HasMore = true
				break
			}
			out.Items = append(out.Items, d)
		}
		return rows.Err()
	})
	if err == nil {
		out.OmittedRecentPasses = e.decisionOmissions.count(scope.ProjectID, channel)
	}
	return out, err
}

func validSelectionDecision(d SelectionDecision) bool {
	if d.Phase != "selection" && d.Phase != "automatic_plan" || !identifier.MatchString(d.Outcome) || len(d.Outcome) > 128 || d.Reason != "" && (!identifier.MatchString(d.Reason) || len(d.Reason) > 128) || d.SelectionID != "" && !validRunID(d.SelectionID) || d.SelectedPullNumber < 0 || d.SelectedPullNumber > 2147483647 || len(d.Candidates) > decisionCandidatesLimit {
		return false
	}
	if d.Phase == "selection" && d.SelectionID == "" || d.Phase == "automatic_plan" && (d.SelectionID != "" || d.SelectedPullNumber != 0) || (d.Outcome == "selected") != (d.SelectedPullNumber != 0) {
		return false
	}
	seen := map[int64]bool{}
	for _, p := range d.Candidates {
		if p.PullNumber < 1 || p.PullNumber > 2147483647 || seen[p.PullNumber] || p.HeadSHA != "" && !sha.MatchString(p.HeadSHA) || !identifier.MatchString(p.Outcome) || len(p.Outcome) > 128 || p.Reason != "" && (!identifier.MatchString(p.Reason) || len(p.Reason) > 128) || p.BlockingPullNumber < 0 || p.BlockingPullNumber > 2147483647 {
			return false
		}
		seen[p.PullNumber] = true
	}
	return true
}

// A fixed process-local ring makes recent telemetry gaps visible without waiting
// for the database writer. Counts are lower bounds, reset on restart and eviction.
type decisionOmission struct{ project, channel string }
type decisionOmissionBuffer struct {
	mu      sync.Mutex
	entries [256]decisionOmission
	next    uint64
}

func (b *decisionOmissionBuffer) add(project, channel string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries[b.next%uint64(len(b.entries))] = decisionOmission{project, channel}
	b.next++
}
func (b *decisionOmissionBuffer) count(project, channel string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, item := range b.entries {
		if item.project == project && item.channel == channel {
			count++
		}
	}
	return count
}

// ValidateDecisionHistorySnapshot checks optional, non-authoritative retained
// telemetry without treating it as review evidence or restoring scheduler work.
func ValidateDecisionHistorySnapshot(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT phase,fingerprint,CASE WHEN length(CAST(body AS BLOB))<=65536 THEN body END,first_seen_at,last_seen_at,occurrences FROM review_selection_decision_history`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var phase, fingerprint, body, first, last string
		var count int64
		if err = rows.Scan(&phase, &fingerprint, &body, &first, &last, &count); err != nil {
			return err
		}
		var d SelectionDecision
		sum := sha256.Sum256([]byte(body))
		ft, fe := time.Parse(time.RFC3339Nano, first)
		lt, le := time.Parse(time.RFC3339Nano, last)
		if json.Unmarshal([]byte(body), &d) != nil || !validSelectionDecision(d) || phase != d.Phase || fingerprint != hex.EncodeToString(sum[:]) || fe != nil || le != nil || lt.Before(ft) || count < 1 {
			return invalid()
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	var oversized int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT project_id,channel FROM review_selection_decision_history GROUP BY project_id,channel HAVING count(*)>256)`).Scan(&oversized); err != nil {
		return err
	}
	if oversized != 0 {
		return invalid()
	}
	return nil
}
