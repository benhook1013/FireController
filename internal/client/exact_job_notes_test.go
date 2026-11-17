package client_test

import (
	"github.com/benhook1013/FireController/internal/client"
	"net/http/httptest"
	"testing"
)

func TestNotesExactJobIgnoresWorkerResponseConstraint(t *testing.T) {
	f := newMutationFixture(t)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	c := f.newClient(t, f.admin, f.sender, server.URL)
	job, err := c.CreateJob(t.Context(), "exact-job", client.CreateJobRequest{Title: "Shared work", WorkerID: f.sender})
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range []string{f.sender, f.recipient} {
		if _, err := c.CreateNote(t.Context(), "note-"+worker, client.CreateNoteRequest{WorkerID: worker, JobID: job.ID, Kind: "reminder", Body: "Shared note"}); err != nil {
			t.Fatal(err)
		}
	}
	filter := client.NoteFilter{JobID: job.ID, WorkerID: f.sender, IncludeAssignedJobs: false}
	first, err := c.Notes(t.Context(), client.PageOptions{Limit: 1}, filter)
	if err != nil || len(first.Items) != 1 || !first.HasMore {
		t.Fatal(first, err)
	}
	next, err := c.Notes(t.Context(), client.PageOptions{Limit: 1, Cursor: first.Cursor}, filter)
	if err != nil || len(next.Items) != 1 || next.HasMore || first.Items[0].ID == next.Items[0].ID {
		t.Fatal(next, err)
	}
}
