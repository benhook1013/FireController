package mcpadapter_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The selected wait remains positive and never exceeds the MCP request's 50s
// bound, even after setup consumes part of its shared deadline.
func boundedPositiveReviewWait(t *testing.T, preference string) bool {
	t.Helper()
	seconds, err := strconv.Atoi(strings.TrimPrefix(preference, "wait="))
	if !strings.HasPrefix(preference, "wait=") || err != nil || seconds < 0 || seconds > 50 {
		t.Error("invalid bounded review wait", preference)
		return false
	}
	return seconds > 0
}

func TestReviewWaitSDKFitsAfterSetupAndPreservesErrors(t *testing.T) {
	const id = "review_operation_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var mode atomic.Value
	mode.Store("pending")
	var identities, reads, mutations, ordinary atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			mutations.Add(1)
			t.Error("wait sent mutation")
			return
		}
		if r.URL.Path == "/api/v1/instance" {
			identities.Add(1)
			select {
			case <-time.After(20 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"instance_id": "instance-one"})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/pause") {
			ordinary.Add(1)
			if r.Header.Get("Prefer") != "" {
				t.Error("ordinary read gained wait preference")
			}
			_ = json.NewEncoder(w).Encode(client.WorkerPause{WorkerID: "worker-one"})
			return
		}
		reads.Add(1)
		if r.URL.Path != "/api/v1/projects/project-one/reviews/operations/"+id+"/status" || r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wait changed selected read/binding")
		}
		// On the old client this waits until the injected tool deadline, reproducing
		// deadline_exceeded instead of a valid quiet pending response.
		seconds, _ := strconv.Atoi(strings.TrimPrefix(r.Header.Get("Prefer"), "wait="))
		if r.Header.Get("Prefer") != "wait=0" {
			select {
			case <-time.After(time.Duration(seconds) * time.Second):
			case <-r.Context().Done():
				return
			}
		}
		switch mode.Load().(string) {
		case "malformed":
			_, _ = io.WriteString(w, "not JSON")
			return
		case "unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "overloaded", "message": "synthetic unavailable"}, "request_id": "request-one"})
			return
		case "timeout":
			<-r.Context().Done()
			return
		}
		_ = json.NewEncoder(w).Encode(client.ReviewOperationStatus{ID: id, PullNumber: 2, Channel: "hosted", State: "queued", Revision: 1, Stage: "queued", UpdatedAt: "2026-01-01T00:00:00Z", NextAction: "wait"})
	}))
	defer server.Close()
	credential := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(credential, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	binding := localContext()
	binding.Profile = "chosen"
	binding.ServerURL = server.URL
	a := adapter(t, profiles(t, map[string]profile.Context{"chosen": binding}, credential), "chosen")
	a.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				if mode.Load().(string) == "cancelled" {
					cancel()
				}
				return next(ctx, method, request)
			}
			return next(ctx, method, request)
		}
	})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	lifetime, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ss, err := a.Connect(lifetime, &mcp.IOTransport{Reader: inR, Writer: outW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sdk := mcp.NewClient(&mcp.Implementation{Name: "wait-budget-sdk", Version: "1"}, nil)
	session, err := sdk.Connect(lifetime, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		ss.Close()
		t.Fatal(err)
	}
	defer func() {
		ss.Close()
		session.Close()
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
	}()
	for _, tc := range []struct{ name, code string }{{"pending", ""}, {"malformed", "invalid_response"}, {"unavailable", "overloaded"}, {"timeout", "deadline_exceeded"}, {"cancelled", "canceled"}} {
		t.Run(tc.name, func(t *testing.T) {
			mode.Store(tc.name)
			beforeIdentity, beforeRead := identities.Load(), reads.Load()
			result, err := session.CallTool(lifetime, &mcp.CallToolParams{Name: "review_operation_read", Arguments: map[string]any{"operation_id": id, "wait_seconds": 50}})
			if err != nil {
				t.Fatal("RPC failed before tool diagnostic", err)
			}
			if tc.code != "" {
				diagnosticResult(t, result, tc.code, false)
			} else {
				var got client.ReviewOperationStatusOutput
				decodeResult(t, result, &got)
				if got.ID != id || got.State != "queued" || got.Stage != "queued" || got.Revision != 1 || got.NextAction != "wait" {
					t.Fatal("quiet wait invented terminal state", got)
				}
			}
			want := int64(1)
			if tc.name == "cancelled" {
				want = 0
			}
			if identities.Load()-beforeIdentity != want || reads.Load()-beforeRead != want || mutations.Load() != 0 {
				t.Fatal("wait retried or mutated", identities.Load()-beforeIdentity, reads.Load()-beforeRead, mutations.Load())
			}
		})
	}
	mode.Store("ordinary")
	beforeIdentity := identities.Load()
	var got struct {
		profile.Context
		Verified    bool               `json:"verified"`
		WorkerPause client.WorkerPause `json:"worker_pause"`
	}
	decodeResult(t, call(t, session, "doctor", nil), &got)
	if !got.Verified || got.WorkerID != "worker-one" || ordinary.Load() != 1 || identities.Load() != beforeIdentity+1 || mutations.Load() != 0 {
		t.Fatal("unrelated read changed", got)
	}
}
