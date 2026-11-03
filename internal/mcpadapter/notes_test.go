package mcpadapter_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNotesToolsSDKAndCLICrossAdapterOriginalKeyRecovery(t *testing.T) {
	f := mutationFixture(t)
	var lose atomic.Bool
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/notes") {
			posts.Add(1)
			if lose.CompareAndSwap(true, false) {
				actual := httptest.NewRecorder()
				f.handler.ServeHTTP(actual, r)
				if actual.Code != 201 {
					t.Errorf("create did not commit %d %s", actual.Code, actual.Body.String())
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(actual.Code)
				_, _ = io.WriteString(w, "{")
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	identity := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	path := profiles(t, map[string]profile.Context{"chosen": identity}, f.credential)
	session := reviewControlsStdio(t, path, "chosen")
	tools, err := allDiscoveredTools(t, session)
	if err != nil || len(tools.Tools) == 0 || tools.NextCursor != "" {
		t.Fatal("discovery", err)
	}
	want := map[string]int{"note_create": 6, "note_update": 10, "note_read": 1, "note_list": 9, "note_history": 3, "note_revision_read": 2}
	reads, writes := 0, 0
	for _, tool := range tools.Tools {
		count, ok := want[tool.Name]
		if !ok {
			continue
		}
		delete(want, tool.Name)
		if tool.Annotations.ReadOnlyHint {
			reads++
		} else {
			writes++
		}

		var schema struct {
			Properties map[string]any
			Additional bool `json:"additionalProperties"`
		}
		raw, _ := json.Marshal(tool.InputSchema)
		write := tool.Name == "note_create" || tool.Name == "note_update"
		if json.Unmarshal(raw, &schema) != nil || len(schema.Properties) != count || schema.Additional || tool.Annotations.ReadOnlyHint == write || !tool.Annotations.IdempotentHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != (tool.Name == "note_update") {
			t.Fatal("schema/annotation", tool.Name)
		}
	}
	if len(want) != 0 || reads != 4 || writes != 2 {
		t.Fatal("tool census", want, reads, writes)
	}
	body := strings.Repeat("<", 16<<10)
	request := map[string]any{"idempotency_key": "note-cross-adapter", "body": body}
	lose.Store(true)
	diagnosticResult(t, call(t, session, "note_create", request), "invalid_response", true)
	if posts.Load() != 1 {
		t.Fatal("SDK auto retried")
	}
	var stdout, stderr bytes.Buffer
	code := cli.RunContext(t.Context(), []string{"--profiles-file", path, "--profile", "chosen", "notes", "add", "--body", body, "--key", "note-cross-adapter"}, nil, &stdout, &stderr)
	var original client.NoteReceipt
	if code != cli.ExitSuccess || stderr.Len() != 0 || json.Unmarshal(stdout.Bytes(), &original) != nil || original.Revision != 1 {
		t.Fatal("CLI recovery", code, stderr.String())
	}
	var current client.Note
	decodeResult(t, call(t, session, "note_read", map[string]any{"note_id": original.ID}), &current)
	if current.Body != body || current.WorkerID != f.sender || current.Kind != "reminder" {
		t.Fatal("defaults/full escaped body")
	}
	var updated client.NoteReceipt
	decodeResult(t, call(t, session, "note_update", map[string]any{"note_id": original.ID, "idempotency_key": "consume-note", "expected_revision": 1, "status": "consumed"}), &updated)
	if updated.Revision != 2 {
		t.Fatal(updated)
	}
	decodeResult(t, call(t, session, "note_update", map[string]any{"note_id": original.ID, "idempotency_key": "consume-noop", "expected_revision": 2, "status": "consumed"}), &updated)
	if updated.Revision != 2 || updated.Outcome != "unchanged" {
		t.Fatal(updated)
	}
	diagnosticResult(t, call(t, session, "note_update", map[string]any{"note_id": original.ID, "idempotency_key": "consume-stale", "expected_revision": 1, "status": "consumed"}), "note_revision_conflict", false)
	var replay client.NoteReceipt
	decodeResult(t, call(t, session, "note_create", request), &replay)
	if replay != original {
		t.Fatal("original key changed", replay)
	}
	var page client.Page[client.NoteSummary]
	decodeResult(t, call(t, session, "note_list", map[string]any{}), &page)
	if len(page.Items) != 0 {
		t.Fatal("default status not pending")
	}
	decodeResult(t, call(t, session, "note_list", map[string]any{"all_workers": true, "status": "all"}), &page)
	if len(page.Items) != 1 || !page.Items[0].BodyTruncated || len(page.Items[0].BodyPreview) != 240 {
		t.Fatal(page)
	}
	decodeResult(t, call(t, session, "note_list", map[string]any{"worker_id": f.recipient, "direct_only": true, "status": "all"}), &page)
	if len(page.Items) != 0 {
		t.Fatal("explicit project worker crossed scope", page)
	}
	var history client.Page[client.NoteHistorySummary]
	decodeResult(t, call(t, session, "note_history", map[string]any{"note_id": original.ID, "limit": 1}), &history)
	if len(history.Items) != 1 || history.Items[0].Revision != 2 || !history.HasMore || history.Items[0].ActorWorkerID != f.sender {
		t.Fatal(history)
	}
	var revision client.NoteRevision
	decodeResult(t, call(t, session, "note_revision_read", map[string]any{"note_id": original.ID, "revision": 1}), &revision)
	if revision.Body != body || revision.Status != "pending" || revision.RecordedAt != revision.UpdatedAt {
		t.Fatal("immutable revision")
	}
	phaseResult := call(t, session, "note_create", map[string]any{"idempotency_key": "phase-only", "worker_id": "", "phase": "Later", "body": "Phase-only scope"})
	diagnosticResult(t, phaseResult, "validation_failed", false)
	var effects int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM note_revisions`).Scan(&effects); err != nil || effects != 2 || f.providers.Load() != 1 {
		t.Fatal("duplicate effects/provider", effects, err)
	}
}
func TestNotesToolsRejectMalformedBeforeCredentialAndHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	identity := localContext()
	identity.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": identity}, "missing-private-credential"), "alpha"), "")
	for _, v := range []struct{ name, raw string }{
		{"note_create", `{"idempotency_key":"k","body":"x","worker_id":null}`}, {"note_create", `{"idempotency_key":"k","body":"x","kind":"unknown"}`}, {"note_create", `{"idempotency_key":"k","body":"\ud800"}`}, {"note_create", `{"idempotency_key":"k","body":"x","body":"y"}`}, {"note_create", `{"idempotency_key":"k","Body":"x"}`}, {"note_create", `{"idempotency_key":"k","body":"x","worker_id":""}`},
		{"note_update", `{"note_id":"n","idempotency_key":"k","expected_revision":1}`}, {"note_update", `{"note_id":"n","idempotency_key":"k","expected_revision":1,"status":"dismissed"}`}, {"note_update", `{"note_id":"n","idempotency_key":"k","expected_revision":1.0,"body":"x"}`}, {"note_update", `{"note_id":"n","idempotency_key":"k","expected_revision":0,"body":"x"}`}, {"note_update", `{"note_id":"n","idempotency_key":"k","expected_revision":1,"body":null}`},
		{"note_list", `{"all_workers":true,"direct_only":true}`}, {"note_list", `{"all_workers":null}`}, {"note_list", `{"all_workers":1}`}, {"note_list", `{"worker_id":""}`}, {"note_list", `{"worker_id":"other","all_workers":true}`}, {"note_list", `{"status":""}`}, {"note_list", `{"phase":"\t"}`}, {"note_history", `{"note_id":"n","limit":0}`}, {"note_revision_read", `{"note_id":"n","revision":0}`}, {"note_read", `{"note_id":"n","profile":"other"}`},
	} {
		diagnosticResult(t, call(t, session, v.name, json.RawMessage(v.raw)), "invalid_arguments", false)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid input used credentials/HTTP", requests.Load())
	}
}
func TestNotesToolsBoundedCancellationNoFanout(t *testing.T) {
	f := mutationFixture(t)
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			f.handler.ServeHTTP(w, r)
			return
		}
		requests.Add(1)
		<-r.Context().Done()
	}))
	defer service.Close()
	a := f.adapter(t, service.URL, f.sender)
	a.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, r mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				ctx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
				defer cancel()
				return next(ctx, method, r)
			}
			return next(ctx, method, r)
		}
	})
	session := connect(t, a, "")
	started := time.Now()
	diagnosticResult(t, call(t, session, "note_list", map[string]any{}), "deadline_exceeded", false)
	if requests.Load() != 1 || time.Since(started) > time.Second {
		t.Fatal("cancel/retry", requests.Load(), time.Since(started))
	}
}

func TestNoteSizeDiagnosticsBeforeCredentialsOrNetwork(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	bound := localContext()
	bound.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": bound}, "missing-private-credential"), "chosen"), "")
	for _, tc := range []struct{ body, field, measurement string }{
		{"PRIVATE_NOTE_BODY" + strings.Repeat("雪", client.MaxNoteBodyBytes/3+1), "body", "16384 decoded UTF-8 bytes"},
		{strings.Repeat("PRIVATE_NOTE_BODY", 9000), "arguments", "131072 encoded JSON bytes"},
	} {
		d := diagnosticResult(t, call(t, session, "note_update", map[string]any{"note_id": "note-one", "idempotency_key": "original-key", "expected_revision": 1, "body": tc.body}), "invalid_arguments", false)
		raw, _ := json.Marshal(d)
		if !strings.Contains(d.Message, tc.measurement) || !strings.Contains(d.Action, "No request was sent") || !strings.Contains(string(raw), `"field":"`+tc.field+`"`) || strings.Contains(string(raw), "PRIVATE_NOTE_BODY") || strings.Contains(string(raw), "missing-private-credential") {
			t.Fatal("unsafe or unhelpful size diagnostic", string(raw))
		}
	}
	if requests.Load() != 0 {
		t.Fatal("invalid note reached credential/network work")
	}
}

func TestNoteByteBoundPatchPreservesBodyAndOmittedFields(t *testing.T) {
	body := strings.Repeat("雪", client.MaxNoteBodyBytes/3) + "x"
	var writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			jsonResponse(w, map[string]string{"instance_id": "instance-one"})
			return
		}
		writes.Add(1)
		var fields map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&fields) != nil || len(fields) != 2 || string(fields["expected_revision"]) != "1" || r.Method != "PATCH" || r.URL.Path != "/api/v1/projects/project-one/notes/note-one" || r.Header.Get("Idempotency-Key") != "original-key" || r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("note patch changed guards, key or omitted fields")
		}
		var got string
		if json.Unmarshal(fields["body"], &got) != nil || got != body {
			t.Error("valid multibyte note text changed")
		}
		jsonResponse(w, client.NoteReceipt{ID: "note-one", Revision: 2, Outcome: "updated"})
	}))
	defer service.Close()
	bound := localContext()
	bound.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": bound}, credential(t)), "chosen"), "")
	var receipt client.NoteReceipt
	decodeResult(t, call(t, session, "note_update", map[string]any{"note_id": "note-one", "idempotency_key": "original-key", "expected_revision": 1, "body": body}), &receipt)
	if receipt.ID != "note-one" || receipt.Revision != 2 || writes.Load() != 1 {
		t.Fatal("valid boundary changed or repeated mutation", receipt, writes.Load())
	}
}
