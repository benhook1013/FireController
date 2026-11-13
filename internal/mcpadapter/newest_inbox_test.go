package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestNewestInboxCLIAndMCPReadSamePendingSelection(t *testing.T) {
	f := mutationFixture(t)
	base := "/api/v1/projects/" + f.project
	send := func(key string) string {
		return apiRequest(t, f.handler, "POST", base+"/messages", f.admin, key, f.sender, `{"recipient_worker_id":"`+f.recipient+`","body":"`+key+`"}`, 201)["id"].(string)
	}
	old := send("old-pending")
	newest := send("seen-newest-pending")
	apiRequest(t, f.handler, "POST", base+"/messages/"+newest+"/seen", f.admin, "seen-only", f.recipient, `{}`, 200)
	var writes atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	binding := profile.Context{Profile: "chosen", ServerURL: s.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.recipient}
	path := profiles(t, map[string]profile.Context{"chosen": binding}, f.credential)
	session := reviewControlsStdio(t, path, "chosen")
	for _, tool := range []string{"inbox_list", "inbox_snapshot"} {
		var page client.Page[client.InboxItem]
		read := func(args map[string]any) {
			result := call(t, session, tool, args)
			if tool == "inbox_snapshot" {
				var snapshot client.InboxSnapshot
				decodeResult(t, result, &snapshot)
				page = snapshot.Inbox
			} else {
				decodeResult(t, result, &page)
			}
		}
		read(map[string]any{"newest": true, "limit": 1})
		if len(page.Items) != 1 || page.Items[0].ID != newest || !page.Items[0].Seen || page.Items[0].Acknowledged || !page.HasMore {
			t.Fatal("latest pending was hidden")
		}
		read(map[string]any{"newest": true, "limit": 1, "cursor": page.Cursor})
		if len(page.Items) != 1 || page.Items[0].ID != old || page.HasMore {
			t.Fatal("pending continuation lost")
		}
		for _, bad := range []any{"true", nil, 1} {
			errorResult(t, call(t, session, tool, map[string]any{"newest": bad}), "invalid_arguments")
		}
	}
	var out, errout bytes.Buffer
	if cli.Run([]string{"--profiles-file", path, "--profile", "chosen", "inbox", "list", "--newest", "--limit", "1"}, nil, &out, &errout) != cli.ExitSuccess {
		t.Fatal(errout.String())
	}
	var page client.Page[client.InboxItem]
	if json.Unmarshal(out.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != newest || writes.Load() != 0 {
		t.Fatal("CLI did not preserve newest pending read")
	}
}
