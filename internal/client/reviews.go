package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const (
	maxReviewListResponse = 1 << 20
	maxReviewRunResponse  = 4 << 20
	maxReviewPulls        = 100
	maxReviewRunsPerPull  = 100
	maxReviewAttempts     = 100
	maxReviewFindings     = 100
)

// ReviewRunOptions selects one bounded run-summary page from a retained snapshot.
type ReviewRunOptions struct {
	PageOptions
	Snapshot string
}

type ReviewQueue struct {
	ProjectID  string              `json:"project_id"`
	Configured bool                `json:"configured"`
	Revision   int64               `json:"revision"`
	SnapshotID string              `json:"snapshot_id"`
	Source     string              `json:"source"`
	ObservedAt string              `json:"observed_at"`
	RecordedAt string              `json:"recorded_at"`
	Items      []ReviewPullSummary `json:"items"`
	Cursor     string              `json:"cursor,omitempty"`
	HasMore    bool                `json:"has_more"`
}

type ReviewPull struct {
	ProjectID  string            `json:"project_id"`
	Revision   int64             `json:"revision"`
	SnapshotID string            `json:"snapshot_id"`
	Source     string            `json:"source"`
	ObservedAt string            `json:"observed_at"`
	RecordedAt string            `json:"recorded_at"`
	Pull       ReviewPullSummary `json:"pull"`
	Attempts   []ReviewAttempt   `json:"attempts"`
}

type ReviewRuns struct {
	ProjectID  string             `json:"project_id"`
	Revision   int64              `json:"revision"`
	SnapshotID string             `json:"snapshot_id"`
	Source     string             `json:"source"`
	ObservedAt string             `json:"observed_at"`
	RecordedAt string             `json:"recorded_at"`
	Items      []ReviewRunSummary `json:"items"`
	Cursor     string             `json:"cursor,omitempty"`
	HasMore    bool               `json:"has_more"`
}

type ReviewRunDetail struct {
	ProjectID  string           `json:"project_id"`
	Revision   int64            `json:"revision"`
	SnapshotID string           `json:"snapshot_id"`
	Source     string           `json:"source"`
	ObservedAt string           `json:"observed_at"`
	RecordedAt string           `json:"recorded_at"`
	PullNumber int64            `json:"pull_number"`
	Run        ReviewRunSummary `json:"run"`
	Findings   []ReviewFinding  `json:"findings"`
}

type ReviewPullSummary struct {
	Number         int64              `json:"number"`
	Title          string             `json:"title"`
	State          string             `json:"state"`
	HeadSHA        string             `json:"head_sha"`
	BaseRef        string             `json:"base_ref"`
	BaseSHA        string             `json:"base_sha"`
	CIState        string             `json:"ci_state"`
	Mergeability   string             `json:"mergeability"`
	Complete       bool               `json:"complete"`
	RunCount       int64              `json:"run_count"`
	AttemptCount   int64              `json:"attempt_count"`
	RecentRuns     []ReviewRunSummary `json:"recent_runs"`
	LatestAttempts []ReviewAttempt    `json:"latest_attempts"`
}

type ReviewRunSummary struct {
	Source         string   `json:"source"`
	ID             string   `json:"id"`
	PullNumber     int64    `json:"pull_number"`
	SourceRunID    string   `json:"source_run_id"`
	Channel        string   `json:"channel"`
	HeadSHA        string   `json:"head_sha"`
	Reviewer       string   `json:"reviewer"`
	Scope          string   `json:"scope"`
	Outcome        string   `json:"outcome"`
	Attributable   bool     `json:"attributable"`
	Complete       bool     `json:"complete"`
	StartedAt      string   `json:"started_at"`
	FinishedAt     string   `json:"finished_at"`
	Found          int64    `json:"found"`
	Accepted       int64    `json:"accepted"`
	Routed         int64    `json:"routed"`
	Finalized      bool     `json:"finalized"`
	Fixed          int64    `json:"fixed"`
	CoverageLimits []string `json:"coverage_limits"`
}

type ReviewAttempt struct {
	SourceAttemptID  string `json:"source_attempt_id"`
	Channel          string `json:"channel"`
	HeadSHA          string `json:"head_sha"`
	State            string `json:"state"`
	StartedAt        string `json:"started_at"`
	FinishedAt       string `json:"finished_at"`
	DurationSeconds  *int64 `json:"duration_seconds"`
	SourceRunID      string `json:"source_run_id"`
	TriggerID        string `json:"trigger_id"`
	ProviderReviewID string `json:"provider_review_id"`
}

type ReviewFinding struct {
	SourceFindingID string `json:"source_finding_id"`
	Title           string `json:"title"`
	Detail          string `json:"detail"`
	Severity        string `json:"severity"`
	Disposition     string `json:"disposition"`
	DecisionReason  string `json:"decision_reason"`
	Actor           string `json:"actor"`
	DecidedAt       string `json:"decided_at"`
	FixSHA          string `json:"fix_sha"`
	ProofNote       string `json:"proof_note"`
	ResolvedAt      string `json:"resolved_at"`
}

// ReviewQueue returns the compact, ordered review queue from one stored snapshot.
func (c *Client) ReviewQueue(ctx context.Context, options PageOptions) (ReviewQueue, error) {
	query, err := reviewPageQuery(options)
	if err != nil {
		return ReviewQueue{}, err
	}
	var value ReviewQueue
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "queue"), query, true, "", nil, http.StatusOK, false, &value, maxReviewListResponse, decodeReviewResponse); err != nil {
		return ReviewQueue{}, err
	}
	if !c.validReviewMeta(value.ProjectID, value.Revision, value.SnapshotID, value.Source, value.ObservedAt, value.RecordedAt, value.Configured) {
		if value.ProjectID != c.identity.ProjectID || value.Configured || value.Revision != 0 || value.SnapshotID != "" || value.Source != "" || value.ObservedAt != "" || value.RecordedAt != "" {
			return ReviewQueue{}, invalidResponse()
		}
		if value.Items == nil || len(value.Items) != 0 || value.Cursor != "" || value.HasMore {
			return ReviewQueue{}, invalidResponse()
		}
		return value, nil
	}
	if !validReviewPage(value.Items, value.Cursor, value.HasMore, options) || len(value.Items) > maxReviewPulls {
		return ReviewQueue{}, invalidResponse()
	}
	seenPulls := make(map[int64]struct{}, len(value.Items))
	for _, item := range value.Items {
		if !c.validReviewPull(item) {
			return ReviewQueue{}, invalidResponse()
		}
		if _, exists := seenPulls[item.Number]; exists {
			return ReviewQueue{}, invalidResponse()
		}
		seenPulls[item.Number] = struct{}{}
	}
	return value, nil
}

// ReviewPull returns one pull summary and its bounded retained attempt records.
func (c *Client) ReviewPull(ctx context.Context, number int64, snapshot string) (ReviewPull, error) {
	if !validPullNumber(number) || snapshot != "" && !validReviewSnapshot(snapshot) {
		return ReviewPull{}, validation()
	}
	query := make(url.Values)
	if snapshot != "" {
		query.Set("snapshot", snapshot)
	}
	var value ReviewPull
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "pulls", strconv.FormatInt(number, 10)), query, true, "", nil, http.StatusOK, false, &value, maxReviewListResponse, decodeReviewResponse); err != nil {
		return ReviewPull{}, err
	}
	if !c.validReviewMeta(value.ProjectID, value.Revision, value.SnapshotID, value.Source, value.ObservedAt, value.RecordedAt, true) || snapshot != "" && value.SnapshotID != snapshot || value.Pull.Number != number || !c.validReviewPull(value.Pull) || value.Attempts == nil || len(value.Attempts) > maxReviewAttempts || int64(len(value.Attempts)) != value.Pull.AttemptCount {
		return ReviewPull{}, invalidResponse()
	}
	seen := make(map[string]struct{}, len(value.Attempts))
	for _, attempt := range value.Attempts {
		if !validReviewAttempt(attempt) {
			return ReviewPull{}, invalidResponse()
		}
		if _, exists := seen[attempt.SourceAttemptID]; exists {
			return ReviewPull{}, invalidResponse()
		}
		seen[attempt.SourceAttemptID] = struct{}{}
	}
	return value, nil
}

// ReviewRuns returns one newest-first bounded page of run summaries.
func (c *Client) ReviewRuns(ctx context.Context, number int64, options ReviewRunOptions) (ReviewRuns, error) {
	query, err := reviewPageQuery(options.PageOptions)
	if err != nil || !validPullNumber(number) || options.Snapshot != "" && !validReviewSnapshot(options.Snapshot) {
		return ReviewRuns{}, validation()
	}
	if options.Snapshot != "" {
		query.Set("snapshot", options.Snapshot)
	}
	var value ReviewRuns
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "pulls", strconv.FormatInt(number, 10), "runs"), query, true, "", nil, http.StatusOK, false, &value, maxReviewListResponse, decodeReviewResponse); err != nil {
		return ReviewRuns{}, err
	}
	if !c.validReviewMeta(value.ProjectID, value.Revision, value.SnapshotID, value.Source, value.ObservedAt, value.RecordedAt, true) || options.Snapshot != "" && value.SnapshotID != options.Snapshot || !validReviewPage(value.Items, value.Cursor, value.HasMore, options.PageOptions) || len(value.Items) > maxReviewRunsPerPull {
		return ReviewRuns{}, invalidResponse()
	}
	seen := make(map[string]struct{}, len(value.Items))
	seenSource := make(map[[2]string]struct{}, len(value.Items))
	for _, item := range value.Items {
		if !validReviewRunSummary(item) || item.PullNumber != number {
			return ReviewRuns{}, invalidResponse()
		}
		if _, exists := seen[item.ID]; exists {
			return ReviewRuns{}, invalidResponse()
		}
		seen[item.ID] = struct{}{}
		if _, exists := seenSource[[2]string{item.Source, item.SourceRunID}]; exists {
			return ReviewRuns{}, invalidResponse()
		}
		seenSource[[2]string{item.Source, item.SourceRunID}] = struct{}{}
	}
	return value, nil
}

// ReviewRun returns one explicit full run detail, including its bounded findings.
func (c *Client) ReviewRun(ctx context.Context, id, snapshot string) (ReviewRunDetail, error) {
	if !validReviewRunID(id) || snapshot != "" && !validReviewSnapshot(snapshot) {
		return ReviewRunDetail{}, validation()
	}
	query := make(url.Values)
	if snapshot != "" {
		query.Set("snapshot", snapshot)
	}
	var value ReviewRunDetail
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "runs", id), query, true, "", nil, http.StatusOK, false, &value, maxReviewRunResponse, decodeReviewResponse); err != nil {
		return ReviewRunDetail{}, err
	}
	if !c.validReviewMeta(value.ProjectID, value.Revision, value.SnapshotID, value.Source, value.ObservedAt, value.RecordedAt, true) || snapshot != "" && value.SnapshotID != snapshot || !validPullNumber(value.PullNumber) || !validReviewRunSummary(value.Run) || value.Run.PullNumber != value.PullNumber || value.Run.ID != id || value.Findings == nil || len(value.Findings) > maxReviewFindings || int64(len(value.Findings)) > value.Run.Found {
		return ReviewRunDetail{}, invalidResponse()
	}
	seen := make(map[string]struct{}, len(value.Findings))
	fixed := int64(0)
	for _, finding := range value.Findings {
		if !validReviewFinding(finding) {
			return ReviewRunDetail{}, invalidResponse()
		}
		if _, exists := seen[finding.SourceFindingID]; exists {
			return ReviewRunDetail{}, invalidResponse()
		}
		seen[finding.SourceFindingID] = struct{}{}
		if finding.FixSHA != "" {
			fixed++
		}
	}
	if value.Run.Complete && int64(len(value.Findings)) != value.Run.Found || fixed != value.Run.Fixed {
		return ReviewRunDetail{}, invalidResponse()
	}
	return value, nil
}

func (c *Client) validReviewMeta(project string, revision int64, snapshot, source, observed, recorded string, configured bool) bool {
	return configured && project == c.identity.ProjectID && revision > 0 && validReviewSnapshot(snapshot) && cleanReviewText(source, 128, true) && validTimestamp(observed) && validTimestamp(recorded)
}

func (c *Client) validReviewPull(value ReviewPullSummary) bool {
	if !validPullNumber(value.Number) || !cleanReviewText(value.Title, 500, true) || !oneReviewOf(value.State, "open", "merged", "closed", "unknown") ||
		!validReviewSHA(value.HeadSHA) || !cleanReviewText(value.BaseRef, 256, false) || !validReviewSHA(value.BaseSHA) ||
		!oneReviewOf(value.CIState, "unknown", "pending", "success", "failure") || !oneReviewOf(value.Mergeability, "unknown", "mergeable", "conflicting") ||
		value.RunCount < 0 || value.RunCount > maxReviewRunsPerPull || value.AttemptCount < 0 || value.AttemptCount > maxReviewAttempts ||
		value.RecentRuns == nil || len(value.RecentRuns) > 3 || int64(len(value.RecentRuns)) > value.RunCount || value.RunCount > 0 && len(value.RecentRuns) == 0 ||
		value.LatestAttempts == nil || len(value.LatestAttempts) > 4 || int64(len(value.LatestAttempts)) > value.AttemptCount || value.AttemptCount > 0 && len(value.LatestAttempts) == 0 {
		return false
	}
	seenRuns := make(map[string]struct{}, len(value.RecentRuns))
	seenSourceRuns := make(map[[2]string]struct{}, len(value.RecentRuns))
	for _, run := range value.RecentRuns {
		if !validReviewRunSummary(run) || run.PullNumber != value.Number {
			return false
		}
		if _, exists := seenRuns[run.ID]; exists {
			return false
		}
		seenRuns[run.ID] = struct{}{}
		if _, exists := seenSourceRuns[[2]string{run.Source, run.SourceRunID}]; exists {
			return false
		}
		seenSourceRuns[[2]string{run.Source, run.SourceRunID}] = struct{}{}
	}
	seenChannels := make(map[string]struct{}, len(value.LatestAttempts))
	for _, attempt := range value.LatestAttempts {
		if !validReviewAttempt(attempt) {
			return false
		}
		if _, exists := seenChannels[attempt.Channel]; exists {
			return false
		}
		seenChannels[attempt.Channel] = struct{}{}
	}
	return true
}

func validReviewRunSummary(value ReviewRunSummary) bool {
	if !validReviewRunID(value.ID) || !validPullNumber(value.PullNumber) || !cleanReviewText(value.Source, 128, true) || !cleanReviewText(value.SourceRunID, 256, true) ||
		!oneReviewOf(value.Channel, "cli", "hosted", "manual", "subagent") || !validReviewSHA(value.HeadSHA) ||
		!cleanReviewText(value.Reviewer, 128, false) || !cleanReviewText(value.Scope, 128, true) ||
		!oneReviewOf(value.Outcome, "completed", "active", "prepared", "posting", "failed", "cancelled", "rate_limited", "uncertain", "unknown") ||
		value.Found < 0 || value.Accepted < 0 || value.Routed < 0 || value.Accepted > value.Found-value.Routed || value.Fixed < 0 || value.Fixed > value.Found ||
		value.CoverageLimits == nil || len(value.CoverageLimits) > 16 {
		return false
	}
	for _, limit := range value.CoverageLimits {
		if !cleanReviewText(limit, 1024, false) {
			return false
		}
	}
	if value.StartedAt != "" && !validTimestamp(value.StartedAt) || value.FinishedAt != "" && !validTimestamp(value.FinishedAt) {
		return false
	}
	if value.Outcome == "completed" || value.Complete {
		if value.StartedAt == "" || value.FinishedAt == "" {
			return false
		}
	}
	if value.StartedAt != "" && value.FinishedAt != "" {
		started, _ := time.Parse(time.RFC3339Nano, value.StartedAt)
		finished, _ := time.Parse(time.RFC3339Nano, value.FinishedAt)
		if finished.Before(started) {
			return false
		}
	}
	return true
}

func validReviewAttempt(value ReviewAttempt) bool {
	if !cleanReviewText(value.SourceAttemptID, 256, true) || !oneReviewOf(value.Channel, "cli", "hosted", "manual", "subagent") ||
		!validReviewSHA(value.HeadSHA) || !oneReviewOf(value.State, "prepared", "preparing", "held", "posting", "active", "completed", "failed", "cancelled", "rate_limited", "uncertain", "unknown") ||
		!cleanReviewText(value.SourceRunID, 256, false) || !cleanReviewText(value.TriggerID, 128, false) || !cleanReviewText(value.ProviderReviewID, 128, false) {
		return false
	}
	if value.StartedAt != "" && !validTimestamp(value.StartedAt) || value.FinishedAt != "" && !validTimestamp(value.FinishedAt) {
		return false
	}
	if value.State == "completed" && (value.StartedAt == "" || value.FinishedAt == "") {
		return false
	}
	if value.StartedAt != "" && value.FinishedAt != "" {
		started, _ := time.Parse(time.RFC3339Nano, value.StartedAt)
		finished, _ := time.Parse(time.RFC3339Nano, value.FinishedAt)
		if finished.Before(started) {
			return false
		}
	}
	return value.DurationSeconds == nil || *value.DurationSeconds >= 0
}

func validReviewFinding(value ReviewFinding) bool {
	if !cleanReviewText(value.SourceFindingID, 256, true) || !cleanReviewText(value.Title, 500, true) || !cleanReviewText(value.Detail, 16<<10, false) ||
		!oneReviewOf(value.Severity, "info", "low", "medium", "high", "critical", "unknown") || !oneReviewOf(value.Disposition, "unadjudicated", "accepted", "routed", "rejected") ||
		!cleanReviewText(value.DecisionReason, 2<<10, false) || !cleanReviewText(value.Actor, 128, false) || !validReviewSHA(value.FixSHA) || !cleanReviewText(value.ProofNote, 2<<10, false) {
		return false
	}
	if value.DecidedAt != "" && !validTimestamp(value.DecidedAt) || value.ResolvedAt != "" && !validTimestamp(value.ResolvedAt) {
		return false
	}
	return (value.FixSHA == "" || value.Disposition == "accepted") && (value.ResolvedAt == "" || value.FixSHA != "")
}

func validReviewPage[T any](items []T, cursor string, hasMore bool, options PageOptions) bool {
	limit := options.Limit
	if limit == 0 {
		limit = 20
	}
	return items != nil && len(items) <= limit && (cursor == "" || validReviewCursor(cursor)) && (!hasMore || len(items) > 0 && cursor != "") && (hasMore || cursor == "")
}

func reviewPageQuery(options PageOptions) (url.Values, error) {
	query, err := pageQuery(options)
	if err != nil || options.Cursor != "" && !validReviewCursor(options.Cursor) {
		return nil, validation()
	}
	return query, nil
}

func validPullNumber(value int64) bool { return value > 0 && value <= 2147483647 }

func validReviewSnapshot(value string) bool { return validReviewOpaqueID(value, "review_snapshot_") }
func validReviewRunID(value string) bool    { return validReviewOpaqueID(value, "review_run_") }
func validReviewOpaqueID(value, prefix string) bool {
	if len(value) != len(prefix)+32 || !strings.HasPrefix(value, prefix) {
		return false
	}
	encoded := strings.TrimPrefix(value, prefix)
	if encoded != strings.ToLower(encoded) {
		return false
	}
	_, err := hex.DecodeString(encoded)
	return err == nil
}

func validReviewCursor(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validReviewSHA(value string) bool {
	if value == "" {
		return true
	}
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func cleanReviewText(value string, max int, required bool) bool {
	if len(value) > max || !utf8.ValidString(value) || required && strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if r == 0x7f || r < 0x20 && r != '\t' && r != '\r' && r != '\n' {
			return false
		}
	}
	return true
}

func oneReviewOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func decodeReviewResponse(data []byte, dst any) error {
	if err := strictjson.Decode(data, dst); err != nil {
		return err
	}
	if !reviewShape(data, reflect.TypeOf(dst)) {
		return strictjson.ErrSyntax
	}
	return nil
}

func reviewShape(data []byte, typ reflect.Type) bool {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(trimmed, &fields) != nil || fields == nil {
			return false
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			parts := strings.Split(field.Tag.Get("json"), ",")
			name := parts[0]
			if name == "-" {
				continue
			}
			raw, present := fields[name]
			optional := name == "cursor" || len(parts) > 1 && parts[1] == "omitempty"
			if !present {
				if optional {
					continue
				}
				return false
			}
			if name == "duration_seconds" && field.Type.Kind() == reflect.Pointer && field.Type.Elem().Kind() == reflect.Int64 && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				continue
			}
			if !reviewShape(raw, field.Type) {
				return false
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(trimmed, &items) != nil || items == nil {
			return false
		}
		for _, item := range items {
			if !reviewShape(item, typ.Elem()) {
				return false
			}
		}
	}
	return true
}
