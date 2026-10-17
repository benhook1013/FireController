package client

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/benhook1013/FireController/internal/strictjson"
)

type ProjectMapPhase struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Archived bool   `json:"archived"`
}

type ProjectMapLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type ProjectMapEditable struct {
	Kind        string            `json:"kind"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	State       string            `json:"state"`
	Where       string            `json:"where"`
	Next        string            `json:"next"`
	Trigger     string            `json:"trigger"`
	Phases      []ProjectMapPhase `json:"phases"`
	Links       []ProjectMapLink  `json:"links"`
	Archived    bool              `json:"archived"`
}

type ProjectMapEntry struct {
	ID          string            `json:"id"`
	ProjectID   string            `json:"project_id"`
	Revision    int64             `json:"revision"`
	Kind        string            `json:"kind"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	State       string            `json:"state"`
	Where       string            `json:"where"`
	Next        string            `json:"next"`
	Trigger     string            `json:"trigger"`
	Phases      []ProjectMapPhase `json:"phases"`
	Links       []ProjectMapLink  `json:"links"`
	Archived    bool              `json:"archived"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
}

type ProjectMapSummary struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	State    string `json:"state"`
	Archived bool   `json:"archived"`
	Revision int64  `json:"revision"`
}

type ProjectMapHistorySummary struct {
	Revision  int64  `json:"revision"`
	ActorID   string `json:"actor_id"`
	CreatedAt string `json:"created_at"`
}

type ProjectMapRevision struct {
	Revision  int64           `json:"revision"`
	ActorID   string          `json:"actor_id"`
	CreatedAt string          `json:"created_at"`
	State     ProjectMapEntry `json:"state"`
}

type ReplaceProjectMapEntryRequest struct {
	ExpectedRevision int64              `json:"expected_revision"`
	Entry            ProjectMapEditable `json:"entry"`
}

type ProjectMapReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type ProjectMapFilter struct {
	Kind     string
	Archived bool
}

type JobWorkstream struct {
	JobID           string `json:"job_id"`
	WorkstreamID    string `json:"workstream_id"`
	WorkstreamTitle string `json:"workstream_title"`
	Revision        int64  `json:"revision"`
}

type ReplaceJobWorkstreamRequest struct {
	ExpectedRevision int64  `json:"expected_revision"`
	WorkstreamID     string `json:"workstream_id"`
}

type JobWorkstreamReceipt struct {
	JobID    string `json:"job_id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}

type JobWorkstreamHistoryEntry struct {
	Revision        int64  `json:"revision"`
	WorkstreamID    string `json:"workstream_id"`
	WorkstreamTitle string `json:"workstream_title"`
	JobRevision     int64  `json:"job_revision"`
	ActorID         string `json:"actor_id"`
	CreatedAt       string `json:"created_at"`
}

func projectMapKind(kind string) bool { return kind == "workstream" || kind == "return_point" }

func projectMapTitle(title string, maximum int) bool {
	return noteText(title, maximum, true, false) && title == strings.TrimSpace(title)
}

func projectMapPhaseID(id string) bool {
	if len(id) < 1 || len(id) > 64 || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return true
}

func validProjectMapEditable(entry ProjectMapEditable) bool {
	if !projectMapKind(entry.Kind) || !projectMapTitle(entry.Title, 240) || !noteText(entry.State, 240, false, false) || !noteText(entry.Description, 8<<10, false, true) || !noteText(entry.Where, 8<<10, false, true) || !noteText(entry.Next, 8<<10, false, true) || !noteText(entry.Trigger, 8<<10, false, true) || entry.Phases == nil || entry.Links == nil || len(entry.Phases) > 64 || len(entry.Links) > 12 {
		return false
	}
	if entry.Kind == "workstream" && entry.Trigger != "" || entry.Kind == "return_point" && (entry.Where != "" || entry.Next != "" || len(entry.Phases) != 0) {
		return false
	}
	seen := make(map[string]bool, len(entry.Phases))
	for _, phase := range entry.Phases {
		if !projectMapPhaseID(phase.ID) || seen[phase.ID] || !projectMapTitle(phase.Name, 160) || !noteText(phase.State, 240, false, false) {
			return false
		}
		seen[phase.ID] = true
	}
	for _, link := range entry.Links {
		u, err := url.Parse(link.URL)
		if !projectMapTitle(link.Label, 120) || !noteText(link.URL, 2048, true, false) || strings.IndexFunc(link.URL, unicode.IsSpace) >= 0 || err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
			return false
		}
	}
	body, err := json.Marshal(entry)
	return err == nil && len(body) <= 64<<10
}

func normalizeProjectMap(entry ProjectMapEditable) ProjectMapEditable {
	entry.Title = strings.TrimSpace(entry.Title)
	entry.Phases = append([]ProjectMapPhase{}, entry.Phases...)
	for i := range entry.Phases {
		entry.Phases[i].Name = strings.TrimSpace(entry.Phases[i].Name)
	}
	entry.Links = append([]ProjectMapLink{}, entry.Links...)
	for i := range entry.Links {
		entry.Links[i].Label = strings.TrimSpace(entry.Links[i].Label)
	}
	return entry
}

func ValidateProjectMapEditable(entry ProjectMapEditable) error {
	if !validProjectMapEditable(normalizeProjectMap(entry)) {
		return mutationValidation()
	}
	return nil
}

// DecodeProjectMapEditable validates an explicit input document before client
// setup. Optional omission means replacement defaults; scalar null is invalid.
func DecodeProjectMapEditable(data []byte) (ProjectMapEditable, error) {
	var entry ProjectMapEditable
	if len(data) > 256<<10 || strictjson.Decode(data, &entry) != nil {
		return ProjectMapEditable{}, mutationValidation()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return ProjectMapEditable{}, mutationValidation()
	}
	for name, raw := range fields {
		if name != "phases" && name != "links" && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return ProjectMapEditable{}, mutationValidation()
		}
	}
	var phases []map[string]json.RawMessage
	if raw, exists := fields["phases"]; exists {
		if json.Unmarshal(raw, &phases) != nil {
			return ProjectMapEditable{}, mutationValidation()
		}
		for _, phase := range phases {
			for _, raw := range phase {
				if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					return ProjectMapEditable{}, mutationValidation()
				}
			}
		}
	}
	entry = normalizeProjectMap(entry)
	if !validProjectMapEditable(entry) {
		return ProjectMapEditable{}, mutationValidation()
	}
	return entry, nil
}

func ValidateReplaceProjectMapEntryRequest(request ReplaceProjectMapEntryRequest) error {
	if request.ExpectedRevision < 1 || request.ExpectedRevision == math.MaxInt64 {
		return fail("invalid_revision", "Supply a writable expected map-entry revision.", "Read the entry and supply its revision explicitly.", 400)
	}
	return ValidateProjectMapEditable(request.Entry)
}

func ValidateReplaceJobWorkstreamRequest(request ReplaceJobWorkstreamRequest) error {
	if request.ExpectedRevision < 0 || request.ExpectedRevision == math.MaxInt64 {
		return fail("invalid_revision", "Supply a writable expected job-workstream revision.", "Read the job's independent workstream association revision.", 400)
	}
	if request.WorkstreamID != "" && !validID(request.WorkstreamID) {
		return mutationValidation()
	}
	return nil
}

func (c *Client) projectMapPath(parts ...string) []string {
	return c.projectPath(append([]string{"map", "entries"}, parts...)...)
}

func (c *Client) CreateProjectMapEntry(ctx context.Context, key string, entry ProjectMapEditable) (ProjectMapReceipt, error) {
	if !validMutationKey(key) {
		return ProjectMapReceipt{}, mutationValidation()
	}
	if err := ValidateProjectMapEditable(entry); err != nil {
		return ProjectMapReceipt{}, err
	}
	var result ProjectMapReceipt
	if err := c.mutate(ctx, http.MethodPost, c.projectMapPath(), key, normalizeProjectMap(entry), http.StatusCreated, &result); err != nil {
		return ProjectMapReceipt{}, err
	}
	if !validID(result.ID) || result.Revision != 1 || result.Outcome != "created" {
		return ProjectMapReceipt{}, unknownMutation("invalid_response", "The service returned an invalid map-entry receipt; its outcome is unknown.", http.StatusCreated)
	}
	return result, nil
}

func (c *Client) ReplaceProjectMapEntry(ctx context.Context, key, id string, request ReplaceProjectMapEntryRequest) (ProjectMapReceipt, error) {
	if !validMutationKey(key) || !validID(id) {
		return ProjectMapReceipt{}, mutationValidation()
	}
	if err := ValidateReplaceProjectMapEntryRequest(request); err != nil {
		return ProjectMapReceipt{}, err
	}
	request.Entry = normalizeProjectMap(request.Entry)
	var result ProjectMapReceipt
	if err := c.mutate(ctx, http.MethodPut, c.projectMapPath(id), key, request, http.StatusOK, &result); err != nil {
		return ProjectMapReceipt{}, err
	}
	if result.ID != id || !(result.Outcome == "updated" && result.Revision == request.ExpectedRevision+1 || result.Outcome == "unchanged" && result.Revision == request.ExpectedRevision) {
		return ProjectMapReceipt{}, unknownMutation("invalid_response", "The service returned an invalid map-entry receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

func (c *Client) validProjectMapEntry(entry ProjectMapEntry, id string) bool {
	return entry.ID == id && validID(id) && entry.ProjectID == c.identity.ProjectID && entry.Revision > 0 && len(entry.CreatedAt) <= 64 && len(entry.UpdatedAt) <= 64 && noteTimes(entry.CreatedAt, entry.UpdatedAt) && validProjectMapEditable(ProjectMapEditable{entry.Kind, entry.Title, entry.Description, entry.State, entry.Where, entry.Next, entry.Trigger, entry.Phases, entry.Links, entry.Archived})
}

func (c *Client) ProjectMapEntry(ctx context.Context, id string) (ProjectMapEntry, error) {
	if !validID(id) {
		return ProjectMapEntry{}, validation()
	}
	var result ProjectMapEntry
	if err := c.get(ctx, c.projectMapPath(id), nil, true, &result); err != nil {
		return ProjectMapEntry{}, err
	}
	if !c.validProjectMapEntry(result, id) {
		return ProjectMapEntry{}, invalidResponse()
	}
	return result, nil
}

func projectMapPage(options PageOptions) (PageOptions, url.Values, error) {
	if options.Limit == 0 {
		options.Limit = 25
	}
	q, err := pageQuery(options)
	if err != nil || options.Cursor != "" && !validOpaqueCursor(options.Cursor) {
		return options, nil, validation()
	}
	return options, q, nil
}

func (c *Client) ProjectMapEntries(ctx context.Context, options PageOptions, filter ProjectMapFilter) (Page[ProjectMapSummary], error) {
	options, q, err := projectMapPage(options)
	if err != nil || filter.Kind != "" && !projectMapKind(filter.Kind) {
		return Page[ProjectMapSummary]{}, validation()
	}
	if filter.Kind != "" {
		q.Set("kind", filter.Kind)
	}
	q.Set("archived", strconv.FormatBool(filter.Archived))
	var result Page[ProjectMapSummary]
	if err = c.get(ctx, c.projectMapPath(), q, true, &result); err != nil {
		return Page[ProjectMapSummary]{}, err
	}
	if !validPage(result, options) || result.Cursor != "" && !validOpaqueCursor(result.Cursor) {
		return Page[ProjectMapSummary]{}, invalidResponse()
	}
	seen := make(map[string]bool, len(result.Items))
	previous := ""
	for _, entry := range result.Items {
		if !validID(entry.ID) || seen[entry.ID] || previous != "" && entry.ID <= previous || !projectMapKind(entry.Kind) || !projectMapTitle(entry.Title, 240) || !noteText(entry.State, 240, false, false) || entry.Revision < 1 || entry.Archived != filter.Archived || filter.Kind != "" && entry.Kind != filter.Kind {
			return Page[ProjectMapSummary]{}, invalidResponse()
		}
		seen[entry.ID] = true
		previous = entry.ID
	}
	return result, nil
}

func (c *Client) ProjectMapHistory(ctx context.Context, id string, options PageOptions) (Page[ProjectMapHistorySummary], error) {
	options, q, err := projectMapPage(options)
	if err != nil || !validID(id) {
		return Page[ProjectMapHistorySummary]{}, validation()
	}
	var result Page[ProjectMapHistorySummary]
	if err = c.get(ctx, c.projectMapPath(id, "history"), q, true, &result); err != nil {
		return Page[ProjectMapHistorySummary]{}, err
	}
	if !validPage(result, options) || result.Cursor != "" && !validOpaqueCursor(result.Cursor) {
		return Page[ProjectMapHistorySummary]{}, invalidResponse()
	}
	var previous int64
	for _, entry := range result.Items {
		if entry.Revision < 1 || !validID(entry.ActorID) || !validJobProgressTime(entry.CreatedAt) || previous != 0 && entry.Revision >= previous {
			return Page[ProjectMapHistorySummary]{}, invalidResponse()
		}
		previous = entry.Revision
	}
	return result, nil
}

func (c *Client) ProjectMapRevision(ctx context.Context, id string, revision int64) (ProjectMapRevision, error) {
	if !validID(id) || revision < 1 {
		return ProjectMapRevision{}, validation()
	}
	var result ProjectMapRevision
	if err := c.get(ctx, c.projectMapPath(id, "history", strconv.FormatInt(revision, 10)), nil, true, &result); err != nil {
		return ProjectMapRevision{}, err
	}
	if result.Revision != revision || result.State.Revision != revision || !c.validProjectMapEntry(result.State, id) || !validID(result.ActorID) || !validJobProgressTime(result.CreatedAt) {
		return ProjectMapRevision{}, invalidResponse()
	}
	return result, nil
}

func validWorkstreamReference(id, title string) bool {
	return id == "" && title == "" || validID(id) && projectMapTitle(title, 240)
}

func (c *Client) JobWorkstream(ctx context.Context, jobID string) (JobWorkstream, error) {
	if !validID(jobID) {
		return JobWorkstream{}, validation()
	}
	var result JobWorkstream
	if err := c.get(ctx, c.projectPath("jobs", jobID, "workstream"), nil, true, &result); err != nil {
		return JobWorkstream{}, err
	}
	if result.JobID != jobID || result.Revision < 0 || !validWorkstreamReference(result.WorkstreamID, result.WorkstreamTitle) || result.Revision == 0 && result.WorkstreamID != "" {
		return JobWorkstream{}, invalidResponse()
	}
	return result, nil
}

func (c *Client) ReplaceJobWorkstream(ctx context.Context, key, jobID string, request ReplaceJobWorkstreamRequest) (JobWorkstreamReceipt, error) {
	if !validMutationKey(key) || !validID(jobID) {
		return JobWorkstreamReceipt{}, mutationValidation()
	}
	if err := ValidateReplaceJobWorkstreamRequest(request); err != nil {
		return JobWorkstreamReceipt{}, err
	}
	var result JobWorkstreamReceipt
	if err := c.mutate(ctx, http.MethodPut, c.projectPath("jobs", jobID, "workstream"), key, request, http.StatusOK, &result); err != nil {
		return JobWorkstreamReceipt{}, err
	}
	if result.JobID != jobID || !(result.Outcome == "updated" && result.Revision == request.ExpectedRevision+1 || result.Outcome == "unchanged" && result.Revision == request.ExpectedRevision) {
		return JobWorkstreamReceipt{}, unknownMutation("invalid_response", "The service returned an invalid job-workstream receipt; its outcome is unknown.", http.StatusOK)
	}
	return result, nil
}

func (c *Client) JobWorkstreamHistory(ctx context.Context, jobID string, options PageOptions) (Page[JobWorkstreamHistoryEntry], error) {
	options, q, err := projectMapPage(options)
	if err != nil || !validID(jobID) {
		return Page[JobWorkstreamHistoryEntry]{}, validation()
	}
	var result Page[JobWorkstreamHistoryEntry]
	if err = c.get(ctx, c.projectPath("jobs", jobID, "workstream", "history"), q, true, &result); err != nil {
		return Page[JobWorkstreamHistoryEntry]{}, err
	}
	if !validPage(result, options) || result.Cursor != "" && !validOpaqueCursor(result.Cursor) {
		return Page[JobWorkstreamHistoryEntry]{}, invalidResponse()
	}
	var previous int64
	for _, entry := range result.Items {
		if entry.Revision < 1 || entry.JobRevision < 1 || !validWorkstreamReference(entry.WorkstreamID, entry.WorkstreamTitle) || !validID(entry.ActorID) || !validJobProgressTime(entry.CreatedAt) || previous != 0 && entry.Revision >= previous {
			return Page[JobWorkstreamHistoryEntry]{}, invalidResponse()
		}
		previous = entry.Revision
	}
	return result, nil
}
