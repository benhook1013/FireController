package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

// This exercises the agent-facing defaults against old handled traffic, rather
// than asserting a parser boolean in isolation. Seen work is still pending work.
func TestPendingInboxDefaultsFindUnhandledBehindHandledHistory(t *testing.T) {
	f := mutationFixture(t)
	base := "/api/v1/projects/" + f.project
	send := func(key, body string) string {
		t.Helper()
		encoded, err := json.Marshal(map[string]string{"recipient_worker_id": f.recipient, "body": body})
		if err != nil {
			t.Fatal(err)
		}
		return apiRequest(t, f.handler, "POST", base+"/messages", f.admin, key, f.sender, string(encoded), 201)["id"].(string)
	}
	var archived []string
	for i := 0; i < 25; i++ {
		id := send(fmt.Sprintf("handled-%d", i), fmt.Sprintf("Already handled %d", i))
		apiRequest(t, f.handler, "POST", base+"/messages/"+id+"/ack", f.admin, fmt.Sprintf("ack-%d", i), f.recipient, `{}`, 200)
		archived = append(archived, id)
	}
	seenBody := "Seen but unfinished: " + strings.Repeat("important full instructions ", 40)
	seen := send("seen-unhandled", seenBody)
	apiRequest(t, f.handler, "POST", base+"/messages/"+seen+"/seen", f.admin, "mark-seen-only", f.recipient, `{}`, 200)
	unreadBody := "Unread coordination: " + strings.Repeat("another complete body ", 40)
	unread := send("unread-unhandled", unreadBody)

	var requests, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	binding := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.recipient}
	path := profiles(t, map[string]profile.Context{"chosen": binding}, f.credential)
	session := reviewControlsStdio(t, path, "chosen")
	runCLI := func(args ...string) (int, []byte, string) {
		t.Helper()
		var out, diagnostic bytes.Buffer
		code := cli.Run(append([]string{"--profiles-file", path, "--profile", "chosen"}, args...), nil, &out, &diagnostic)
		return code, out.Bytes(), diagnostic.String()
	}
	readCLI := func(out any, args ...string) {
		t.Helper()
		code, data, diagnostic := runCLI(args...)
		if code != cli.ExitSuccess || diagnostic != "" {
			t.Fatalf("%v: code=%d diagnostic=%s", args, code, diagnostic)
		}
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
	}
	assertPending := func(page client.Page[client.InboxItem]) {
		t.Helper()
		if len(page.Items) != 2 || page.HasMore || page.Cursor != "" || page.Items[0].ID != seen || !page.Items[0].Seen || page.Items[0].Acknowledged || page.Items[1].ID != unread || page.Items[1].Seen || page.Items[1].Acknowledged {
			t.Fatalf("pending work hidden or receipt meaning changed: %+v", page)
		}
	}
	type state struct{ events, receipts, seen, ack, revisions int }
	readState := func() state {
		t.Helper()
		var value state
		if err := f.store.DB().QueryRow("SELECT count(*) FROM events").Scan(&value.events); err != nil {
			t.Fatal(err)
		}
		if err := f.store.DB().QueryRow("SELECT count(*) FROM receipts").Scan(&value.receipts); err != nil {
			t.Fatal(err)
		}
		if err := f.store.DB().QueryRow("SELECT sum(seen),sum(acknowledged),sum(revision) FROM inbox_recipient_state").Scan(&value.seen, &value.ack, &value.revisions); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := readState()

	// Execute the short recommendation actually supplied to an agent. If the skill
	// changes its recommendation, this test follows that operation's arguments.
	skill, err := os.ReadFile(filepath.Join("..", "..", "skills", "firecontroller-use", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, messageSection, ok := strings.Cut(string(skill), "## Messages and handoffs")
	if !ok {
		t.Fatal("usage skill has no message coordination section")
	}
	_, recommendation, ok := strings.Cut(messageSection, "Read `")
	if !ok {
		t.Fatal("usage skill has no recommended coordination read")
	}
	command, _, ok := strings.Cut(recommendation, "`")
	args := strings.Fields(command)
	if !ok || len(args) < 2 || args[0] != "inbox" || args[1] != "list" {
		t.Fatal("usage skill recommendation is not an inbox-list command")
	}
	t.Run("recommended-cli-read", func(t *testing.T) {
		var page client.Page[client.InboxItem]
		readCLI(&page, args...)
		assertPending(page)
	})
	t.Run("plain-cli-list-and-snapshot", func(t *testing.T) {
		var page client.Page[client.InboxItem]
		readCLI(&page, "inbox", "list")
		assertPending(page)
		var snapshot client.InboxSnapshot
		readCLI(&snapshot, "inbox", "snapshot")
		assertPending(snapshot.Inbox)
		if snapshot.EventCursor == "" {
			t.Fatal("snapshot lost event watermark")
		}
	})
	for _, tool := range []string{"inbox_list", "inbox_snapshot"} {
		t.Run(tool, func(t *testing.T) {
			read := func(args any) client.Page[client.InboxItem] {
				t.Helper()
				if tool == "inbox_snapshot" {
					var value client.InboxSnapshot
					decodeResult(t, call(t, session, tool, args), &value)
					if value.EventCursor == "" {
						t.Fatal("snapshot lost watermark")
					}
					return value.Inbox
				}
				var value client.Page[client.InboxItem]
				decodeResult(t, call(t, session, tool, args), &value)
				return value
			}
			assertPending(read(nil))
			assertPending(read(map[string]any{"unacknowledged": true}))
			page := read(map[string]any{"limit": 1})
			if len(page.Items) != 1 || page.Items[0].ID != seen || !page.HasMore || page.Cursor == "" {
				t.Fatal("pending pagination failed", page)
			}
			next := read(map[string]any{"limit": 1, "cursor": page.Cursor})
			if len(next.Items) != 1 || next.Items[0].ID != unread || next.HasMore {
				t.Fatal("pending continuation failed", next)
			}
			errorResult(t, call(t, session, tool, map[string]any{"limit": 1, "cursor": page.Cursor, "unacknowledged": false}), "validation_failed")
			all := read(json.RawMessage(`{"unacknowledged":false,"limit":20}`))
			if len(all.Items) != 20 || !all.HasMore || all.Cursor == "" || all.Items[0].ID != archived[0] || !all.Items[0].Acknowledged {
				t.Fatal("explicit false did not preserve bounded archive", all)
			}
			errorResult(t, call(t, session, tool, map[string]any{"limit": 20, "cursor": all.Cursor}), "validation_failed")
			archiveNext := read(map[string]any{"limit": 20, "cursor": all.Cursor, "unacknowledged": false})
			if len(archiveNext.Items) != 7 || archiveNext.HasMore || archiveNext.Items[5].ID != seen || archiveNext.Items[6].ID != unread {
				t.Fatal("archive continuation incomplete", archiveNext)
			}
			unreadOnly := read(map[string]any{"unread": true})
			if len(unreadOnly.Items) != 1 || unreadOnly.Items[0].ID != unread {
				t.Fatal("explicit unread filter changed", unreadOnly)
			}
		})
	}
	for _, operation := range []string{"list", "snapshot"} {
		t.Run("cli-archive-"+operation, func(t *testing.T) {
			read := func(args ...string) client.Page[client.InboxItem] {
				t.Helper()
				if operation == "snapshot" {
					var value client.InboxSnapshot
					readCLI(&value, append([]string{"inbox", operation}, args...)...)
					return value.Inbox
				}
				var value client.Page[client.InboxItem]
				readCLI(&value, append([]string{"inbox", operation}, args...)...)
				return value
			}
			all := read("--all", "--limit", "20")
			if len(all.Items) != 20 || !all.HasMore || all.Items[0].ID != archived[0] {
				t.Fatal("CLI archive unavailable", all)
			}
			next := read("--all", "--limit", "20", "--cursor", all.Cursor)
			if len(next.Items) != 7 || next.HasMore || next.Items[5].ID != seen || next.Items[6].ID != unread {
				t.Fatal("CLI archive continuation unavailable", next)
			}
			code, _, diagnostic := runCLI("inbox", operation, "--limit", "20", "--cursor", all.Cursor)
			if code == cli.ExitSuccess || !strings.Contains(diagnostic, "validation_failed") {
				t.Fatal("CLI accepted archive cursor for pending work", code, diagnostic)
			}
		})
	}
	t.Run("full-selected-bodies", func(t *testing.T) {
		for id, body := range map[string]string{seen: seenBody, unread: unreadBody} {
			var message client.Message
			readCLI(&message, "inbox", "read", id)
			if message.ID != id || message.Body != body {
				t.Fatal("CLI selected message body lost")
			}
			decodeResult(t, call(t, session, "inbox_message_read", map[string]any{"message_id": id}), &message)
			if message.ID != id || message.Body != body {
				t.Fatal("MCP selected message body lost")
			}
		}
	})
	t.Run("raw-api-and-typed-client-retain-explicit-filter-contract", func(t *testing.T) {
		api := apiRequest(t, f.handler, "GET", base+"/inbox/"+f.recipient+"?limit=20", f.admin, "", f.recipient, "", 200)
		if len(api["items"].([]any)) != 20 || api["items"].([]any)[0].(map[string]any)["id"] != archived[0] {
			t.Fatal("raw API changed to pending-only")
		}
		c, err := client.New(t.Context(), profile.Binding{Context: binding, CredentialFile: f.credential})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		page, err := c.Inbox(t.Context(), client.PageOptions{Limit: 20}, client.InboxFilter{})
		if err != nil || len(page.Items) != 20 || page.Items[0].ID != archived[0] {
			t.Fatal("typed-client zero filter changed", page, err)
		}
	})
	if after := readState(); after != before || writes.Load() != 0 || f.providers.Load() != 1 {
		t.Fatalf("coordination reads mutated durable receipts or contacted providers: before=%+v after=%+v writes=%d providers=%d", before, after, writes.Load(), f.providers.Load())
	}
	if requests.Load() == 0 {
		t.Fatal("proof did not exercise loopback HTTP")
	}
}

func TestPendingInboxArchiveConflictsRejectBeforeConnection(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	for _, operation := range []string{"list", "snapshot"} {
		for _, flags := range [][]string{
			{"--all", "--unread"}, {"--unread", "--all"},
			{"--all", "--unacknowledged"}, {"--unacknowledged", "--all"},
			{"--all", "--all"}, {"--all", "true"},
		} {
			var out, diagnostic bytes.Buffer
			args := append([]string{"--profiles-file", filepath.Join(t.TempDir(), "absent.json"), "--profile", "missing", "inbox", operation}, flags...)
			if code := cli.Run(args, map[string]string{"FC_SERVER": service.URL}, &out, &diagnostic); code != cli.ExitUsage || out.Len() != 0 || !strings.Contains(diagnostic.String(), "invalid_arguments") {
				t.Fatalf("%v did not reject before configuration: code=%d out=%s error=%s", args, code, &out, &diagnostic)
			}
		}
	}
	if requests.Load() != 0 {
		t.Fatal("malformed archive selector reached a service")
	}
}
