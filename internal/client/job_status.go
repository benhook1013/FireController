package client

import (
	"context"
	"net/http"
	"strings"
	"unicode/utf8"
)

// JobStatus is a compact current-state aid. Use Job to recover working context.
type JobStatus struct {
	ID                string `json:"id"`
	Title             string `json:"title"`
	WorkerID          string `json:"worker_id"`
	Status            string `json:"status"`
	Revision          int64  `json:"revision"`
	UpdatedAt         string `json:"updated_at"`
	ProgressSummary   string `json:"progress_summary"`
	Next              string `json:"next"`
	Blocker           string `json:"blocker"`
	ProgressTruncated bool   `json:"progress_truncated"`
	NextTruncated     bool   `json:"next_truncated"`
	BlockerTruncated  bool   `json:"blocker_truncated"`
	EvidenceCount     int    `json:"evidence_count"`
}

func validStatusPreview(value string, limit int, truncated bool) bool {
	// A UTF-8 prefix can leave at most three unused bytes at a rune boundary.
	return checkpointText(value, limit, false) && (!truncated || len(value) >= limit-3)
}

func (c *Client) JobStatus(ctx context.Context, id string) (JobStatus, error) {
	if !validID(id) {
		return JobStatus{}, validation()
	}
	var out JobStatus
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", id, "status"), nil, true, "", nil, 200, false, &out, 16<<10, decodeResponse); err != nil {
		return JobStatus{}, err
	}
	if out.ID != id || !validID(out.ID) || out.WorkerID != "" && !validID(out.WorkerID) || out.Revision < 1 || !validStatus(out.Status) || out.Title == "" || out.Title != strings.TrimSpace(out.Title) || len(out.Title) > 240 || !utf8.ValidString(out.Title) || !validJobProgressTime(out.UpdatedAt) || !validStatusPreview(out.ProgressSummary, 1024, out.ProgressTruncated) || !validStatusPreview(out.Next, 512, out.NextTruncated) || !validStatusPreview(out.Blocker, 512, out.BlockerTruncated) || out.EvidenceCount < 0 || out.EvidenceCount > 10 {
		return JobStatus{}, invalidResponse()
	}
	return out, nil
}
