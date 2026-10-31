package mcpadapter_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
)

func TestJobRevisionActualCLIAndMCPReadOneRetainedState(t *testing.T) {
	f := mutationFixture(t)
	var calls, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	c := profile.Context{Profile: "chosen", ServerURL: service.URL, InstanceID: f.store.InstanceID(), ProjectID: f.project, WorkerID: f.sender}
	file := profiles(t, map[string]profile.Context{"chosen": c}, f.credential)
	session := connect(t, adapter(t, file, "chosen"), "")
	var created client.JobReceipt
	decodeResult(t, call(t, session, "jobs_create", map[string]any{"title": "Historical job", "brief": "Original **standing instructions** π", "idempotency_key": "revision-original"}), &created)
	decodeResult(t, call(t, session, "job_update", map[string]any{"job_id": created.ID, "expected_revision": int64(1), "brief": "Different current instructions", "idempotency_key": "revision-next"}), &client.JobReceipt{})
	beforeCalls, beforeWrites := calls.Load(), writes.Load()
	var historical client.JobHistoryEntry
	decodeResult(t, call(t, session, "job_revision_read", map[string]any{"job_id": created.ID, "revision": int64(1)}), &historical)
	if historical.Revision != 1 || historical.State.ID != created.ID || historical.State.Brief != "Original **standing instructions** π" || calls.Load()-beforeCalls != 2 || writes.Load() != beforeWrites {
		t.Fatal("MCP did not perform one identity check and one exact historical read")
	}
	beforeCalls = calls.Load()
	var out, errout bytes.Buffer
	code := cli.RunContext(t.Context(), []string{"--profiles-file", file, "--profile", "chosen", "jobs", "revision", created.ID, "1"}, nil, &out, &errout)
	var throughCLI client.JobHistoryEntry
	if code != cli.ExitSuccess || errout.Len() != 0 || json.Unmarshal(out.Bytes(), &throughCLI) != nil || throughCLI.State.Brief != historical.State.Brief || calls.Load()-beforeCalls != 2 || writes.Load() != beforeWrites {
		t.Fatal("CLI changed requested state or expanded history", code, errout.String())
	}
	diagnosticResult(t, call(t, session, "job_revision_read", map[string]any{"job_id": created.ID, "revision": 999}), "not_found", false)
	beforeCalls = calls.Load()
	for _, bad := range []any{map[string]any{"job_id": created.ID}, map[string]any{"job_id": created.ID, "revision": 0}, map[string]any{"job_id": created.ID, "revision": 1.5}, map[string]any{"job_id": created.ID, "revision": 1, "cursor": "other"}} {
		errorResult(t, call(t, session, "job_revision_read", bad), "invalid_arguments", token)
	}
	for _, rev := range []string{"0", "-1", "01", "+1", "1.5", "9223372036854775808"} {
		out.Reset()
		errout.Reset()
		if cli.RunContext(t.Context(), []string{"--profiles-file", "/missing/profile", "jobs", "revision", created.ID, rev}, nil, &out, &errout) != cli.ExitUsage || strings.Contains(errout.String(), "profile_required") {
			t.Fatal("invalid selector escaped local validation")
		}
	}
	if calls.Load() != beforeCalls {
		t.Fatal("malformed history selectors contacted service")
	}
}
