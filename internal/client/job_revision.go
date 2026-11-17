package client

import (
	"context"
	"strconv"
)

// JobRevision reads exactly one retained full-state revision, without paging or
// adding current progress, notes or checklist context to historical evidence.
func (c *Client) JobRevision(ctx context.Context, id string, revision int64) (JobHistoryEntry, error) {
	if !validID(id) || revision < 1 {
		return JobHistoryEntry{}, validation()
	}
	var value JobHistoryEntry
	if err := c.get(ctx, c.projectPath("jobs", id, "history", strconv.FormatInt(revision, 10)), nil, true, &value); err != nil {
		return JobHistoryEntry{}, err
	}
	if value.Revision != revision || !c.validJobHistoryEntry(value, id) {
		return JobHistoryEntry{}, invalidResponse()
	}
	return value, nil
}

func (c *Client) validJobHistoryEntry(item JobHistoryEntry, id string) bool {
	return item.Revision >= 1 && item.State.ID == id && item.State.Revision == item.Revision && c.validJobState(item.State) && validID(item.ActorID) && cleanText(item.CreatedAt, 64)
}
