package cli_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
)

func writeEventJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func TestInboxSnapshotAndEventsCLIUseBoundReadOnlyFeed(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "message-events")
	sender := f.worker(t, project, "Builder")
	recipient := f.worker(t, project, "Overseer")
	const token = "cli-message-events-private-token"
	f.grant(t, project, token)
	firstBody := strings.Repeat("PRIVATE_FIRST_BODY_", 12)
	secondBody := strings.Repeat("PRIVATE_SECOND_BODY_", 12)
	first, _ := f.sendMessage(t, project, sender, recipient, firstBody)
	_, _ = f.sendMessage(t, project, sender, recipient, secondBody)
	providerCalls := f.resolverHit.Load()
	service := httptest.NewServer(f.handler)
	t.Cleanup(service.Close)
	configDir := t.TempDir()
	credential := writeCredential(t, configDir, "events", token)
	profilesPath := filepath.Join(configDir, "profiles.json")
	config := map[string]any{"version": 1, "profiles": map[string]any{"events": map[string]any{
		"server_url": service.URL, "instance_id": f.store.InstanceID(), "project_id": project, "worker_id": recipient, "credential_file": credential,
	}}}
	if err := os.WriteFile(profilesPath, encode(t, config), 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (int, string, string) {
		t.Helper()
		return run(append([]string{"--profile", "events", "--profiles-file", profilesPath}, args...), nil)
	}

	code, stdout, stderr := invoke("inbox", "snapshot", "--unread", "--unacknowledged", "--limit", "1")
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("snapshot CLI code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var snapshot struct {
		Inbox struct {
			Items   []map[string]any `json:"items"`
			Cursor  string           `json:"cursor"`
			HasMore bool             `json:"has_more"`
		} `json:"inbox"`
		EventCursor string `json:"event_cursor"`
	}
	if err := json.Unmarshal([]byte(stdout), &snapshot); err != nil || len(snapshot.Inbox.Items) != 1 || snapshot.Inbox.Items[0]["id"] != first || snapshot.Inbox.Cursor == "" || !snapshot.Inbox.HasMore || snapshot.EventCursor == "" {
		t.Fatalf("snapshot CLI output=%s err=%v", stdout, err)
	}
	if strings.Contains(stdout, firstBody) || strings.Contains(stdout, secondBody) {
		t.Fatal("snapshot exposed a full message body instead of a compact preview")
	}

	// A message committed after the snapshot remains visible after its event
	// watermark. The CLI only reports the compact typed event and its cursor.
	third, _ := f.sendMessage(t, project, sender, recipient, "PRIVATE_THIRD_BODY")
	code, stdout, stderr = invoke("inbox", "events", "--cursor", snapshot.EventCursor, "--limit", "1")
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("events CLI code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	var events struct {
		Items []struct {
			Sequence   int64  `json:"sequence"`
			ProjectID  string `json:"project_id"`
			Type       string `json:"type"`
			ResourceID string `json:"resource_id"`
			Payload    struct {
				ConversationID    string `json:"conversation_id"`
				RecipientWorkerID string `json:"recipient_worker_id"`
				SenderWorkerID    string `json:"sender_worker_id"`
			} `json:"payload"`
		} `json:"items"`
		Cursor   string `json:"cursor"`
		HasMore  bool   `json:"has_more"`
		TimedOut bool   `json:"timed_out"`
	}
	if err := json.Unmarshal([]byte(stdout), &events); err != nil || len(events.Items) != 1 || events.Items[0].ResourceID != third || events.Items[0].Type != "message.sent" || events.Items[0].ProjectID != project || events.Items[0].Sequence < 1 || events.Items[0].Payload.RecipientWorkerID != recipient || events.Items[0].Payload.SenderWorkerID != sender || events.Cursor == "" || events.TimedOut {
		t.Fatalf("events CLI output=%s err=%v", stdout, err)
	}
	if strings.Contains(stdout, "PRIVATE_THIRD_BODY") || strings.Contains(stdout, "body") {
		t.Fatal("message event output contained a body field")
	}

	// Bounded wait consumes the same explicit cursor; a concurrent committed
	// send returns once, with no automatic acknowledgment or further reads.
	var waitedCode int
	var waitedOut, waitedErr string
	done := make(chan struct{})
	go func() {
		waitedCode, waitedOut, waitedErr = invoke("inbox", "events", "--cursor", events.Cursor, "--wait-ms", "1500")
		close(done)
	}()
	fourth, _ := f.sendMessage(t, project, sender, recipient, "PRIVATE_FOURTH_BODY")
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("CLI event wait did not return after a committed message")
	}
	if waitedCode != cli.ExitSuccess || waitedErr != "" {
		t.Fatalf("wait CLI code=%d stdout=%q stderr=%q", waitedCode, waitedOut, waitedErr)
	}
	var waitPage struct {
		Items []struct {
			ResourceID string `json:"resource_id"`
		} `json:"items"`
		TimedOut bool `json:"timed_out"`
	}
	if err := json.Unmarshal([]byte(waitedOut), &waitPage); err != nil || len(waitPage.Items) != 1 || waitPage.Items[0].ResourceID != fourth || waitPage.TimedOut || strings.Contains(waitedOut, "PRIVATE_FOURTH_BODY") {
		t.Fatalf("wait output=%s err=%v", waitedOut, err)
	}
	var seen, acknowledged int
	if err := f.store.DB().QueryRow("SELECT seen,acknowledged FROM inbox_recipient_state WHERE project_id=? AND message_id=? AND worker_id=?", project, fourth, recipient).Scan(&seen, &acknowledged); err != nil {
		t.Fatal(err)
	}
	if seen != 0 || acknowledged != 0 {
		t.Fatalf("snapshot/event reads changed receipt state: seen=%d acknowledged=%d", seen, acknowledged)
	}
	if f.resolverHit.Load() != providerCalls {
		t.Fatal("snapshot or event CLI read contacted repository provider")
	}
	if strings.Contains(stdout+waitedOut, token) || strings.Contains(stderr+waitedErr, token) {
		t.Fatal("event CLI exposed credential material")
	}
}

func TestInboxEventCLIRejectsInvalidArgumentsBeforeCredentialOrNetwork(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/api/v1/instance" {
			writeEventJSON(w, 200, map[string]string{"instance_id": "instance-cli-events"})
			return
		}
		writeEventJSON(w, 404, map[string]string{"error": "unused"})
	}))
	defer service.Close()
	configDir := t.TempDir()
	credential := writeCredential(t, configDir, "events", "cli-events-private-token")
	profilesPath := filepath.Join(configDir, "profiles.json")
	config := map[string]any{"version": 1, "profiles": map[string]any{"events": map[string]any{
		"server_url": service.URL, "instance_id": "instance-cli-events", "project_id": "project-cli-events", "worker_id": "worker-cli-events", "credential_file": credential,
	}}}
	if err := os.WriteFile(profilesPath, encode(t, config), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"inbox", "events"},
		{"inbox", "events", "--limit", "1"},
		{"inbox", "events", "--cursor", "cursor", "--wait-ms", "-1"},
		{"inbox", "events", "--cursor", "cursor", "--wait-ms", "4001"},
		{"inbox", "events", "--cursor", "cursor", "--cursor", "again"},
		{"inbox", "events", "--cursor", "bad\ncursor"},
		{"inbox", "events", "--cursor", "cursor", "--unread"},
		{"inbox", "snapshot", "--unread=false"},
		{"inbox", "snapshot", "--limit", "101"},
	} {
		full := append([]string{"--profile", "events", "--profiles-file", profilesPath}, args...)
		code, stdout, stderr := run(full, nil)
		if code != cli.ExitUsage || stdout != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatalf("invalid args %v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid CLI arguments contacted service %d times", calls.Load())
	}
}

func TestInboxSnapshotAndEventsUseSingleCLIServiceBudget(t *testing.T) {
	// RunContext preserves caller cancellation instead of extending the event
	// wait past the command's one five-second identity+request budget.
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeEventJSON(w, 200, map[string]string{"instance_id": "instance-cli-events"})
	}))
	defer service.Close()
	configDir := t.TempDir()
	credential := writeCredential(t, configDir, "events", "cli-events-private-token")
	profilesPath := filepath.Join(configDir, "profiles.json")
	config := map[string]any{"version": 1, "profiles": map[string]any{"events": map[string]any{
		"server_url": service.URL, "instance_id": "instance-cli-events", "project_id": "project-cli-events", "worker_id": "worker-cli-events", "credential_file": credential,
	}}}
	if err := os.WriteFile(profilesPath, encode(t, config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr strings.Builder
	code := cli.RunContext(ctx, []string{"--profile", "events", "--profiles-file", profilesPath, "inbox", "events", "--cursor", "cursor", "--wait-ms", "4000"}, nil, &stdout, &stderr)
	if code != cli.ExitCancelled || stdout.Len() != 0 || calls.Load() != 0 {
		t.Fatalf("pre-canceled event command code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
