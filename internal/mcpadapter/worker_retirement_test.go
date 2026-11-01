package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestWorkerRetirementSDKAndCLIExplicitRecipientWithoutIdentitySwitch(t *testing.T) {
	f := mutationFixture(t)
	var retirePosts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/retire") {
			retirePosts.Add(1)
		}
		if r.Method == "POST" && (strings.HasSuffix(r.URL.Path, "/retire") || strings.HasSuffix(r.URL.Path, "/ack")) && r.Header.Get("X-FC-Worker") != f.sender {
			t.Error("selected resource impersonated recipient")
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	binding := profile.Context{Profile: "writer", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	adminFile := filepath.Join(t.TempDir(), "admin.token")
	if err := os.WriteFile(adminFile, []byte(f.admin), 0600); err != nil {
		t.Fatal(err)
	}
	writerPath := profiles(t, map[string]profile.Context{"writer": binding}, f.credential)
	adminPath := profiles(t, map[string]profile.Context{"admin": binding}, adminFile)
	sdk := reviewControlsStdio(t, writerPath, "writer")
	invoke := func(path, selected string, args ...string) []byte {
		t.Helper()
		var out, diag bytes.Buffer
		full := append([]string{"--profiles-file", path, "--profile", selected}, args...)
		if code := cli.RunContext(t.Context(), full, nil, &out, &diag); code != cli.ExitSuccess || diag.Len() != 0 {
			t.Fatal(code, diag.String())
		}
		return out.Bytes()
	}
	var message client.MessageReceipt
	decodeResult(t, call(t, sdk, "inbox_send", map[string]any{"recipient_worker_id": f.recipient, "body": "Retirement must not silently acknowledge this", "idempotency_key": "retirement-message"}), &message)
	var preview client.WorkerRetirement
	decodeResult(t, call(t, sdk, "worker_retirement_read", map[string]any{"worker_id": f.recipient}), &preview)
	if preview.CanRetire || preview.Retired || preview.Blockers.Messages.Count != 1 || !reflect.DeepEqual(preview.Blockers.Messages.IDs, []string{message.ID}) {
		t.Fatal("unacknowledged message not visible", preview)
	}
	blocked := diagnosticResult(t, call(t, sdk, "worker_retire", map[string]any{"worker_id": f.recipient, "idempotency_key": "blocked-retirement"}), "worker_retirement_blocked", false)
	var blockers client.WorkerRetirementBlockers
	data, _ := json.Marshal(blocked.Details)
	if json.Unmarshal(data, &blockers) != nil || blockers.Messages.Count != 1 || !reflect.DeepEqual(blockers.Messages.IDs, []string{message.ID}) {
		t.Fatal("blocked retirement lost current safe handles", blocked)
	}
	diagnosticResult(t, call(t, sdk, "inbox_ack", map[string]any{"message_id": message.ID, "worker_id": f.recipient, "idempotency_key": "writer-cannot-ack-other"}), "forbidden", false)
	diagnosticResult(t, call(t, sdk, "inbox_seen", map[string]any{"message_id": message.ID, "worker_id": f.recipient, "idempotency_key": "seen-stays-own"}), "invalid_arguments", false)
	var ack client.MessageReceipt
	if json.Unmarshal(invoke(adminPath, "admin", "inbox", "ack", message.ID, "--worker", f.recipient, "--key", "manager-selected-ack"), &ack) != nil || ack.Outcome != "acknowledged" {
		t.Fatal("manager recipient ACK failed", ack)
	}
	if json.Unmarshal(invoke(writerPath, "writer", "workers", "retirement", f.recipient), &preview) != nil || !preview.CanRetire {
		t.Fatal(preview)
	}
	var retired client.WorkerRetirementReceipt
	request := map[string]any{"worker_id": f.recipient, "idempotency_key": "shared-retirement"}
	decodeResult(t, call(t, sdk, "worker_retire", request), &retired)
	var replay client.WorkerRetirementReceipt
	if json.Unmarshal(invoke(writerPath, "writer", "workers", "retire", f.recipient, "--key", "shared-retirement"), &replay) != nil || replay != retired || retired.Outcome != "retired" || retired.Revision != 2 {
		t.Fatal("CLI/SDK replay diverged", retired, replay)
	}
	if json.Unmarshal(invoke(writerPath, "writer", "workers", "retire", f.recipient, "--key", "already-retired"), &replay) != nil || replay.Outcome != "unchanged" {
		t.Fatal(replay)
	}
	var active, archive client.Page[client.Worker]
	decodeResult(t, call(t, sdk, "workers_list", map[string]any{}), &active)
	decodeResult(t, call(t, sdk, "workers_list", map[string]any{"include_retired": true}), &archive)
	if len(active.Items) != 1 || active.Items[0].ID != f.sender || len(archive.Items) != 2 {
		t.Fatal("active/archive list wrong", active, archive)
	}
	found := false
	for _, w := range archive.Items {
		if w.ID == f.recipient {
			found = w.Retired
		}
	}
	if !found {
		t.Fatal("retired identity lost", archive)
	}
	var cliArchive client.Page[client.Worker]
	if json.Unmarshal(invoke(writerPath, "writer", "workers", "list", "--include-retired"), &cliArchive) != nil || !reflect.DeepEqual(cliArchive, archive) {
		t.Fatal("CLI archive differs", cliArchive, archive)
	}
	if retirePosts.Load() != 4 || f.providers.Load() != 1 {
		t.Fatal("automatic retry/provider work", retirePosts.Load(), f.providers.Load())
	}
	var retirementEvents int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM events WHERE project_id=? AND event_type='workers.retired'", f.project).Scan(&retirementEvents); err != nil || retirementEvents != 1 {
		t.Fatal("retirement duplicated", retirementEvents, err)
	}
}

func TestWorkerRetirementSDKAndCLIInvalidStayOffline(t *testing.T) {
	sdk := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"chosen": localContext()}, "missing-private-credential"), "chosen")
	for _, tc := range []struct{ name, raw string }{
		{"worker_retirement_read", `{"worker_id":null}`},
		{"worker_retire", `{"worker_id":"worker-one"}`},
		{"worker_retire", `{"worker_id":"worker-one","idempotency_key":"x","expected_revision":1}`},
		{"workers_list", `{"include_retired":null}`},
		{"inbox_ack", `{"message_id":"message-one","worker_id":null,"idempotency_key":"x"}`},
	} {
		diagnosticResult(t, call(t, sdk, tc.name, json.RawMessage(tc.raw)), "invalid_arguments", false)
	}
	for _, operation := range []string{"retirement", "retire"} {
		var out, diag bytes.Buffer
		if code := cli.RunContext(t.Context(), []string{"workers", operation, "--help"}, nil, &out, &diag); code != 0 || diag.Len() != 0 || !strings.Contains(out.String(), "unacknowledged") || !strings.Contains(out.String(), "nothing is moved") {
			t.Fatal("missing offline retirement help", code, out.String(), diag.String())
		}
	}
	for _, args := range [][]string{{"workers", "retirement"}, {"workers", "retire", "worker-one"}, {"workers", "list", "--include-retired=false"}, {"inbox", "seen", "message-one", "--worker", "worker-other", "--key", "x"}, {"inbox", "ack", "message-one", "--worker", "", "--key", "x"}} {
		var out, diag bytes.Buffer
		if code := cli.RunContext(t.Context(), append([]string{"--profile", "absent", "--profiles-file", "/unavailable"}, args...), nil, &out, &diag); code != cli.ExitUsage || out.Len() != 0 {
			t.Fatal(args, code, diag.String())
		}
	}
}
