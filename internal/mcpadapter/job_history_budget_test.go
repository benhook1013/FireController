package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestJobHistoryEncodedPagesAcrossCLIAndSDK(t *testing.T) {
	testJobHistoryPagesAcrossCLIAndSDK(t, 32<<10, 8<<10, "")
}

// Keep ordinary paging, cursor reuse and session recovery under the race
// detector. The maximum encoded response is a separate ordinary-runtime gate.
func TestJobHistoryPagesAcrossCLIAndSDK(t *testing.T) {
	testJobHistoryPagesAcrossCLIAndSDK(t, 512, 256, "2")
}

func testJobHistoryPagesAcrossCLIAndSDK(t *testing.T, briefBytes, progressBytes int, initialLimit string) {
	t.Helper()
	f := mutationFixture(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := profiles(t, map[string]profile.Context{"chosen": {Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}}, f.credential)
	base := []string{"--profiles-file", path, "--profile", "chosen"}
	session := reviewControlsStdio(t, path, "chosen")
	var job client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"idempotency_key": "second-history-create", "title": "History", "brief": strings.Repeat("<", briefBytes)}), &job)
	for revision := 1; revision <= 16; revision++ {
		args := map[string]any{"job_id": job.ID, "idempotency_key": fmt.Sprintf("second-history-%d", revision), "expected_revision": revision, "title": fmt.Sprintf("History %d", revision)}
		if revision == 1 {
			args["progress"] = map[string]any{"summary": strings.Repeat("<", progressBytes), "next": strings.Repeat("&", progressBytes)}
		}
		var receipt client.JobReceipt
		decodeResult(t, call(t, session, "job_update", args), &receipt)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	readCLI := func(limit, cursor string) client.Page[client.JobHistoryEntry] {
		t.Helper()
		args := append(append([]string{}, base...), "jobs", "history", job.ID)
		if limit != "" {
			args = append(args, "--limit", limit)
		}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		var out, diagnostic bytes.Buffer
		if code := cli.Run(args, nil, &out, &diagnostic); code != cli.ExitSuccess || diagnostic.Len() != 0 {
			t.Fatalf("CLI history failed: code=%d diagnostic=%s", code, diagnostic.String())
		}
		if out.Len() > 4<<20 {
			t.Fatal("CLI page exceeds budget")
		}
		var page client.Page[client.JobHistoryEntry]
		if json.Unmarshal(out.Bytes(), &page) != nil {
			t.Fatal("CLI history JSON")
		}
		return page
	}
	readMCP := func(limit, cursor string) client.Page[client.JobHistoryEntry] {
		t.Helper()
		// Keep each owned SDK session within the existing helper's 20-second
		// lifetime, including under the race detector's JSON overhead.
		reader := reviewControlsStdio(t, path, "chosen")
		defer reader.Close()
		args := map[string]any{"job_id": job.ID}
		if limit != "" {
			switch limit {
			case "100":
				args["limit"] = 100
			case "2":
				args["limit"] = 2
			default:
				args["limit"] = 1
			}
		}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var page client.Page[client.JobHistoryEntry]
		decodeResult(t, call(t, reader, "job_history", args), &page)
		return page
	}
	original := readMCP(initialLimit, "")
	if !original.HasMore || len(original.Items) == 0 || len(original.Items) >= 17 {
		t.Fatal("expected encoded-budget continuation")
	}
	var appended client.JobReceipt
	writer := reviewControlsStdio(t, path, "chosen")
	decodeResult(t, call(t, writer, "job_update", map[string]any{"job_id": job.ID, "idempotency_key": "append-after-cursor", "expected_revision": 17, "title": "Appended"}), &appended)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for name, read := range map[string]func(string, string) client.Page[client.JobHistoryEntry]{"CLI": readCLI, "MCP": readMCP} {
		old := read("100", original.Cursor)
		if len(old.Items) == 0 || old.Items[0].Revision != original.Items[len(original.Items)-1].Revision+1 || old.Items[len(old.Items)-1].Revision != 18 {
			t.Fatal("issued cursor lost appended revisions")
		}
		for _, limit := range []string{"", "100"} {
			cursor := ""
			want := int64(1)
			for pages := 0; ; pages++ {
				if pages > 18 {
					t.Fatal("nonadvancing page")
				}
				page := read(limit, cursor)
				for _, item := range page.Items {
					if item.Revision != want {
						t.Fatalf("%s limit=%s want=%d got=%d", name, limit, want, item.Revision)
					}
					want++
				}
				if !page.HasMore {
					if want != 19 || page.Cursor != "" {
						t.Fatal("incomplete terminal page")
					}
					t.Logf("%s limit=%q revisions=18 pages=%d", name, limit, pages+1)
					break
				}
				if len(page.Items) == 0 || page.Cursor == "" || cursor == page.Cursor {
					t.Fatal("invalid continuation")
				}
				cursor = page.Cursor
			}
		}
	}
	reader := reviewControlsStdio(t, path, "chosen")
	decodeResult(t, call(t, reader, "job_read", map[string]any{"job_id": job.ID}), new(client.Job))
}
