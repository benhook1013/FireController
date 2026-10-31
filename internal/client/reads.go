package client

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

func (c *Client) projectPath(parts ...string) []string {
	return append([]string{"api", "v1", "projects", c.identity.ProjectID}, parts...)
}
func pageQuery(options PageOptions) (url.Values, error) {
	if options.Limit < 0 || options.Limit > 100 || len(options.Cursor) > 4096 || !utf8.ValidString(options.Cursor) {
		return nil, validation()
	}
	q := make(url.Values)
	if options.Limit > 0 {
		q.Set("limit", strconv.Itoa(options.Limit))
	}
	if options.Cursor != "" {
		q.Set("cursor", options.Cursor)
	}
	return q, nil
}

func validPage[T any](page Page[T], options PageOptions) bool {
	limit := options.Limit
	if limit == 0 {
		limit = 100
	}
	return page.Items != nil && len(page.Items) <= limit && len(page.Cursor) <= 4096 && (!page.HasMore || len(page.Items) > 0 && page.Cursor != "") && (page.HasMore || page.Cursor == "")
}
func validStatus(value string) bool {
	return value == "active" || value == "parked" || value == "blocked" || value == "completed"
}

// ValidJobStatusFilter includes the list-only open filter, never a stored job state.
func ValidJobStatusFilter(value string) bool { return value == "open" || validStatus(value) }

func (c *Client) Worker(ctx context.Context) (Worker, error) {
	var value Worker
	err := c.get(ctx, c.projectPath("workers", c.identity.WorkerID), nil, true, &value)
	if err == nil && (value.ID != c.identity.WorkerID || value.ProjectID != c.identity.ProjectID || !cleanText(value.Name, 200)) {
		err = invalidResponse()
	}
	if err != nil {
		return Worker{}, err
	}
	return value, nil
}
func (c *Client) Workers(ctx context.Context, options PageOptions) (Page[Worker], error) {
	q, err := pageQuery(options)
	if err != nil {
		return Page[Worker]{}, err
	}
	var value Page[Worker]
	if err = c.get(ctx, c.projectPath("workers"), q, true, &value); err != nil {
		return Page[Worker]{}, err
	}
	if !validPage(value, options) {
		return Page[Worker]{}, invalidResponse()
	}
	for _, item := range value.Items {
		if !validID(item.ID) || item.ProjectID != c.identity.ProjectID || !cleanText(item.Name, 200) {
			return Page[Worker]{}, invalidResponse()
		}
	}
	return value, nil
}
func (c *Client) Jobs(ctx context.Context, options PageOptions, filter JobFilter) (Page[JobSummary], error) {
	q, err := pageQuery(options)
	if err != nil || filter.WorkerID != "" && !validID(filter.WorkerID) || filter.Status != "" && !ValidJobStatusFilter(filter.Status) {
		return Page[JobSummary]{}, validation()
	}
	if filter.WorkerID != "" {
		q.Set("worker_id", filter.WorkerID)
	}
	if filter.Status != "" {
		q.Set("status", filter.Status)
	}
	var value Page[JobSummary]
	if err = c.get(ctx, c.projectPath("jobs"), q, true, &value); err != nil {
		return Page[JobSummary]{}, err
	}
	if !validPage(value, options) {
		return Page[JobSummary]{}, invalidResponse()
	}
	for _, item := range value.Items {
		if !validProgressPreview(item.ProgressSummary, item.ProgressTruncated) || !validProgressPreview(item.Blocker, item.BlockerTruncated) || !validID(item.ID) || !cleanText(item.Title, 240) || item.WorkerID != "" && !validID(item.WorkerID) || !validStatus(item.Status) || item.Revision < 1 || filter.WorkerID != "" && item.WorkerID != filter.WorkerID || filter.Status == "open" && item.Status == "completed" || filter.Status != "" && filter.Status != "open" && item.Status != filter.Status {
			return Page[JobSummary]{}, invalidResponse()
		}
	}
	return value, nil
}
func (c *Client) Job(ctx context.Context, id string) (Job, error) {
	if !validID(id) {
		return Job{}, validation()
	}
	var value Job
	err := c.requestBounded(ctx, "GET", c.projectPath("jobs", id), nil, true, "", nil, 200, false, &value, 1<<20, decodeResponse)
	if err == nil && (value.ID != id || !c.validJob(value)) {
		err = invalidResponse()
	}
	if err != nil {
		return Job{}, err
	}
	return value, nil
}
func (c *Client) validJob(value Job) bool {
	return c.validJobCurrent(value) && validID(value.ID) && value.ProjectID == c.identity.ProjectID && cleanText(value.Title, 240) && len(value.Brief) <= 32<<10 && (value.WorkerID == "" || validID(value.WorkerID)) && validStatus(value.Status) && value.Revision >= 1 && cleanText(value.CreatedAt, 64) && cleanText(value.UpdatedAt, 64)
}
func (c *Client) JobHistory(ctx context.Context, id string, options PageOptions) (Page[JobHistoryEntry], error) {
	q, err := pageQuery(options)
	if err != nil || !validID(id) {
		return Page[JobHistoryEntry]{}, validation()
	}
	var value Page[JobHistoryEntry]
	if err = c.get(ctx, c.projectPath("jobs", id, "history"), q, true, &value); err != nil {
		return Page[JobHistoryEntry]{}, err
	}
	if !validPage(value, options) {
		return Page[JobHistoryEntry]{}, invalidResponse()
	}
	for _, item := range value.Items {
		if !c.validJobHistoryEntry(item, id) {
			return Page[JobHistoryEntry]{}, invalidResponse()
		}
	}
	return value, nil
}
func (c *Client) Inbox(ctx context.Context, options PageOptions, filter InboxFilter, selectedWorker ...string) (Page[InboxItem], error) {
	worker, err := c.selectedWorker(selectedWorker)
	if err != nil {
		return Page[InboxItem]{}, err
	}
	q, err := pageQuery(options)
	if err != nil {
		return Page[InboxItem]{}, err
	}
	if filter.Newest {
		q.Set("newest", "true")
	}
	if filter.Unread {
		q.Set("unread", "true")
	}
	if filter.Unacknowledged {
		q.Set("unacknowledged", "true")
	}
	var value Page[InboxItem]
	if err = c.get(ctx, c.projectPath("inbox", worker), q, true, &value); err != nil {
		return Page[InboxItem]{}, err
	}
	if !validPage(value, options) {
		return Page[InboxItem]{}, invalidResponse()
	}
	for _, item := range value.Items {
		if !c.validInboxItem(item, filter, worker) {
			return Page[InboxItem]{}, invalidResponse()
		}
	}
	return value, nil
}

func (c *Client) validInboxItem(item InboxItem, filter InboxFilter, selectedWorker ...string) bool {
	worker := c.identity.WorkerID
	if len(selectedWorker) > 0 {
		worker = selectedWorker[0]
	}
	return validID(item.ID) && validID(item.ConversationID) && validID(item.SenderWorkerID) && item.RecipientWorkerID == worker && utf8.RuneCountInString(item.Preview) <= 160 && !(item.Acknowledged && !item.Seen) && cleanText(item.CreatedAt, 64) && (!filter.Unread || !item.Seen) && (!filter.Unacknowledged || !item.Acknowledged)
}
func (c *Client) Conversation(ctx context.Context, id string, options PageOptions) (ConversationPage, error) {
	q, err := pageQuery(options)
	if err != nil || !validID(id) {
		return ConversationPage{}, validation()
	}
	var value ConversationPage
	if err = c.get(ctx, c.projectPath("conversations", id), q, true, &value); err != nil {
		return ConversationPage{}, err
	}
	if value.ConversationID != id || !validID(value.RootMessageID) || !validPage(Page[Message]{value.Items, value.Cursor, value.HasMore}, options) {
		return ConversationPage{}, invalidResponse()
	}
	for _, item := range value.Items {
		if !validID(item.ID) || item.ConversationID != id || !validID(item.SenderWorkerID) || !validID(item.RecipientWorkerID) || item.ReplyToMessageID != "" && !validID(item.ReplyToMessageID) || strings.TrimSpace(item.Body) == "" || len(item.Body) > 16<<10 || !cleanText(item.CreatedAt, 64) {
			return ConversationPage{}, invalidResponse()
		}
	}
	return value, nil
}
