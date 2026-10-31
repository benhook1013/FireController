package client_test

import (
	"github.com/benhook1013/FireController/internal/client"
	"testing"
)

func TestCheckpointSourceArchiveReadRemainsStrictAndExplicit(t *testing.T) {
	record := client.Checkpoint{ID: "checkpoint-one", ProjectID: "project-one", JobID: "job-one", JobRevision: 1, Sequence: 2, ActorID: "actor-one", Done: "Original done", Next: "Original next", Evidence: []string{}, CreatedAt: "2026-10-15T00:00:00Z"}
	c, calls := progressResponseClient(t, progressJSON(t, client.Page[client.Checkpoint]{Items: []client.Checkpoint{record}, HasMore: true, Cursor: "next"}))
	page, err := c.CheckpointHistory(t.Context(), "job-one", client.PageOptions{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Done != record.Done || calls.Load() != 1 {
		t.Fatal("archive read changed source or fanned out", err)
	}
	record.ProjectID = "foreign"
	c, _ = progressResponseClient(t, progressJSON(t, client.Page[client.Checkpoint]{Items: []client.Checkpoint{record}}))
	_, err = c.CheckpointHistory(t.Context(), "job-one", client.PageOptions{})
	checkError(t, err, "invalid_response")
}
