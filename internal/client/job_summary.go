package client

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// JobShareableSummary is explicitly authored and independent of the private job.
// JobSummary remains the existing compact private job-list item.
type JobShareableSummary struct {
	JobID       string `json:"job_id"`
	JobRevision int64  `json:"job_revision"`
	Revision    int64  `json:"revision"`
	Title       string `json:"title"`
	Body        string `json:"body"`
}
type JobSummaryRevision struct {
	JobID       string `json:"job_id"`
	JobRevision int64  `json:"job_revision"`
	Revision    int64  `json:"revision"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	ActorID     string `json:"actor_id"`
	WorkerID    string `json:"worker_id"`
	CreatedAt   string `json:"created_at"`
}
type SetJobSummaryRequest struct {
	ExpectedJobRevision int64  `json:"expected_job_revision"`
	ExpectedRevision    int64  `json:"expected_revision"`
	Title               string `json:"title"`
	Body                string `json:"body"`
}
type JobSummaryReceipt struct {
	ID          string `json:"id"`
	Revision    int64  `json:"revision"`
	JobRevision int64  `json:"job_revision"`
	Outcome     string `json:"outcome"`
}

func validSummaryText(title, body string) bool {
	if title == "" && body == "" {
		return true
	}
	if strings.TrimSpace(title) == "" || len(title) > 240 || !utf8.ValidString(title) || !checkpointText(body, 2000, true) {
		return false
	}
	for _, ch := range title {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}
func ValidateJobSummaryRequest(r SetJobSummaryRequest) error {
	if r.ExpectedJobRevision < 1 || r.ExpectedRevision < 0 || r.ExpectedRevision == math.MaxInt64 {
		return fail("invalid_revision", "Supply a positive job revision and writable summary revision.", "Read the current summary and supply both revisions explicitly.", 400)
	}
	if !validSummaryText(r.Title, r.Body) {
		return mutationValidation()
	}
	return nil
}
func validShareableSummary(r JobShareableSummary, id string) bool {
	return r.JobID == id && r.JobRevision >= 1 && r.Revision >= 0 && validSummaryText(r.Title, r.Body) && (r.Revision != 0 || r.Title == "" && r.Body == "")
}
func validSummaryRevision(r JobSummaryRevision, id string) bool {
	return validShareableSummary(JobShareableSummary{JobID: r.JobID, JobRevision: r.JobRevision, Revision: r.Revision, Title: r.Title, Body: r.Body}, id) && r.Revision >= 1 && validID(r.ActorID) && (r.WorkerID == "" || validID(r.WorkerID)) && validJobProgressTime(r.CreatedAt)
}
func (c *Client) JobSummary(ctx context.Context, id string) (JobShareableSummary, error) {
	if !validID(id) {
		return JobShareableSummary{}, validation()
	}
	var out JobShareableSummary
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", id, "summary"), nil, true, "", nil, 200, false, &out, 16<<10, decodeResponse); err != nil {
		return JobShareableSummary{}, err
	}
	if !validShareableSummary(out, id) {
		return JobShareableSummary{}, invalidResponse()
	}
	return out, nil
}
func (c *Client) SetJobSummary(ctx context.Context, key, id string, r SetJobSummaryRequest) (JobSummaryReceipt, error) {
	if !validID(id) || !validMutationKey(key) {
		return JobSummaryReceipt{}, mutationValidation()
	}
	if err := ValidateJobSummaryRequest(r); err != nil {
		return JobSummaryReceipt{}, err
	}
	var out JobSummaryReceipt
	if err := c.mutate(ctx, http.MethodPut, c.projectPath("jobs", id, "summary"), key, r, 200, &out); err != nil {
		return JobSummaryReceipt{}, err
	}
	if out.ID != id || out.JobRevision != r.ExpectedJobRevision || (out.Outcome == "updated" && out.Revision != r.ExpectedRevision+1) || (out.Outcome == "unchanged" && out.Revision != r.ExpectedRevision) || out.Outcome != "updated" && out.Outcome != "unchanged" {
		return JobSummaryReceipt{}, unknownMutation("invalid_response", "The job summary receipt was invalid; its outcome is unknown.", http.StatusOK)
	}
	return out, nil
}
func (c *Client) JobSummaryHistory(ctx context.Context, id string, options PageOptions) (Page[JobSummaryRevision], error) {
	if options.Limit == 0 {
		options.Limit = 20
	}
	q, err := pageQuery(options)
	if err != nil || !validID(id) {
		return Page[JobSummaryRevision]{}, validation()
	}
	var out Page[JobSummaryRevision]
	if err = c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", id, "summary", "history"), q, true, "", nil, 200, false, &out, 2<<20, decodeResponse); err != nil {
		return Page[JobSummaryRevision]{}, err
	}
	if !validPage(out, options) {
		return Page[JobSummaryRevision]{}, invalidResponse()
	}
	var last int64
	for _, v := range out.Items {
		if !validSummaryRevision(v, id) || last != 0 && v.Revision >= last {
			return Page[JobSummaryRevision]{}, invalidResponse()
		}
		last = v.Revision
	}
	return out, nil
}
func (c *Client) JobSummaryRevision(ctx context.Context, id string, revision int64) (JobSummaryRevision, error) {
	if !validID(id) || revision < 1 {
		return JobSummaryRevision{}, validation()
	}
	var out JobSummaryRevision
	if err := c.requestBounded(ctx, http.MethodGet, c.projectPath("jobs", id, "summary", "history", strconv.FormatInt(revision, 10)), nil, true, "", nil, 200, false, &out, 16<<10, decodeResponse); err != nil {
		return JobSummaryRevision{}, err
	}
	if !validSummaryRevision(out, id) || out.Revision != revision {
		return JobSummaryRevision{}, invalidResponse()
	}
	return out, nil
}
