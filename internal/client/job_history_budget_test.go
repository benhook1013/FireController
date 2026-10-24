package client_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

// A default history page must remain readable after ordinary bounded updates.
// Exercise actual escaped state, rather than a mocked oversized response.
func TestJobHistoryEncodedPagesAcrossTypedClient(t *testing.T) {
	f := newMutationFixture(t)
	s := httptest.NewServer(f.handler)
	defer s.Close()
	c := f.newClient(t, f.admin, f.sender, s.URL)
	job, err := c.CreateJob(t.Context(), "second-history-create", client.CreateJobRequest{Title: "History", Brief: strings.Repeat("<", 32<<10)})
	if err != nil {
		t.Fatal(err)
	}
	summary, next := strings.Repeat("<", 8<<10), strings.Repeat("&", 8<<10)
	for revision := int64(1); revision <= 16; revision++ {
		title := fmt.Sprintf("History %d", revision)
		patch := client.UpdateJobRequest{ExpectedRevision: revision, Title: &title}
		if revision == 1 {
			patch.Progress = &client.ProgressPatch{Summary: &summary, Next: &next}
		}
		if _, err := c.UpdateJob(t.Context(), fmt.Sprintf("second-history-%d", revision), job.ID, patch); err != nil {
			t.Fatal(err)
		}
	}
	// Demonstrate that the same native endpoint accepts this selected page.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+f.project+"/jobs/"+job.ID+"/history", nil)
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("X-FC-Worker", f.sender)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	var raw client.Page[client.JobHistoryEntry]
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &raw) != nil || len(raw.Items) == 0 || len(raw.Items) >= 17 || !raw.HasMore || w.Body.Len() > 4<<20 {
		t.Fatalf("native history status=%d", w.Code)
	}
	t.Logf("native default page: status=%d bytes=%d items=%d", w.Code, w.Body.Len(), len(raw.Items))
	if page, err := c.JobHistory(t.Context(), job.ID, client.PageOptions{Limit: 1}); err != nil || len(page.Items) != 1 || !page.HasMore {
		t.Fatalf("small-page recovery failed: %v", err)
	}
	// A cursor already issued before another append must remain usable.
	title := "Appended after first page"
	if _, err := c.UpdateJob(t.Context(), "history-concurrent-append", job.ID, client.UpdateJobRequest{ExpectedRevision: 17, Title: &title}); err != nil {
		t.Fatal(err)
	}
	old, err := c.JobHistory(t.Context(), job.ID, client.PageOptions{Limit: 100, Cursor: raw.Cursor})
	if err != nil || len(old.Items) == 0 || old.Items[0].Revision != raw.Items[len(raw.Items)-1].Revision+1 || old.Items[len(old.Items)-1].Revision != 18 {
		t.Fatalf("original cursor lost appended history: %v", err)
	}
	for _, limit := range []int{0, 100} {
		cursor := ""
		want := int64(1)
		for pages := 0; ; pages++ {
			if pages > 18 {
				t.Fatal("nonadvancing continuation")
			}
			page, err := c.JobHistory(t.Context(), job.ID, client.PageOptions{Limit: limit, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range page.Items {
				if item.Revision != want {
					t.Fatalf("limit=%d want revision=%d got=%d", limit, want, item.Revision)
				}
				want++
			}
			if !page.HasMore {
				if page.Cursor != "" || want != 19 {
					t.Fatal("incomplete final page")
				}
				break
			}
			if page.Cursor == "" || page.Cursor == cursor || len(page.Items) == 0 {
				t.Fatal("invalid continuation")
			}
			cursor = page.Cursor
		}
	}
}
