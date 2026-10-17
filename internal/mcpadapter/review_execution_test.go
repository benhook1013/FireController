package mcpadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func executionArguments() map[string]any {
	return map[string]any{"idempotency_key": "explicit-native-review", "expected_configuration_revision": 1, "pull_number": 2, "channel": "cli", "head_sha": strings.Repeat("a", 40), "wait_seconds": 0}
}

func TestReviewExecutionStdioDiscoveryAndStrictArgumentsBeforeCredentials(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	c := localContext()
	c.ServerURL = service.URL
	session := reviewControlsStdio(t, profiles(t, map[string]profile.Context{"alpha": c}, "absent-private-token"), "alpha")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) == 0 || listed.NextCursor != "" {
		t.Fatal("discovery count changed")
	}
	names := map[string]int{"review_execution_read": 0, "review_operation_read": 1, "review_request": 5}
	for _, tool := range listed.Tools {
		count, ok := names[tool.Name]
		if !ok {
			continue
		}
		delete(names, tool.Name)
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
			Additional bool           `json:"additionalProperties"`
		}
		data, _ := json.Marshal(tool.InputSchema)
		properties := count
		if tool.Name == "review_request" || tool.Name == "review_operation_read" {
			properties++
		}
		if json.Unmarshal(data, &schema) != nil || len(schema.Properties) != properties || len(schema.Required) != count || schema.Additional {
			t.Fatal("execution schema fields changed", tool.Name)
		}
		a := tool.Annotations
		write := tool.Name == "review_request"
		if a == nil || a.ReadOnlyHint == write || !a.IdempotentHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint != write {
			t.Fatal("execution annotations contradict effects", tool.Name)
		}
		if write && (!strings.Contains(tool.Description, "consume included allowance asynchronously") || !strings.Contains(tool.Description, "does not mean started or completed") || !strings.Contains(tool.Description, "exact idempotency key")) {
			t.Fatal("request omits asynchronous spend/recovery meaning")
		}
	}
	if len(names) != 0 {
		t.Fatal("missing execution tools")
	}
	var got profile.Context
	decodeResult(t, call(t, session, "context", nil), &got)
	if got != c || requests.Load() != 0 {
		t.Fatal("discovery/context read credentials or changed binding")
	}
	id := "review_operation_" + strings.Repeat("a", 32)
	base := map[string]map[string]any{"review_execution_read": {}, "review_operation_read": {"operation_id": id}, "review_request": executionArguments()}
	for name, valid := range base {
		t.Run(name, func(t *testing.T) {
			check := func(value any) {
				t.Helper()
				diagnosticResult(t, call(t, session, name, value), "invalid_arguments", false)
			}
			check(json.RawMessage(`null`))
			check(json.RawMessage(`[]`))
			for field := range valid {
				copy := map[string]any{}
				for key, value := range valid {
					if key != field {
						copy[key] = value
					}
				}
				if field != "wait_seconds" {
					check(copy)
				}
				copy[field] = nil
				check(copy)
			}
			for _, bad := range []struct {
				field  string
				values []any
			}{
				{"operation_id", []any{"", id + "/other", "review_run_" + strings.Repeat("a", 32), "review_operation_" + strings.Repeat("A", 32), 7}},
				{"idempotency_key", []any{"", " padded", "key\n", "é", strings.Repeat("x", 129)}},
				{"expected_configuration_revision", []any{0, -1, "1", 1.5, json.Number("9223372036854775808")}},
				{"pull_number", []any{0, -1, 2147483648, "2", 2.5}},
				{"channel", []any{"", "CLI", "manual", 3}},
				{"wait_seconds", []any{-1, 901, "1", 1.5, true}},
				{"head_sha", []any{"", strings.Repeat("A", 40), strings.Repeat("a", 39), "../other", 4}},
			} {
				if _, exists := valid[bad.field]; !exists {
					continue
				}
				for _, value := range bad.values {
					copy := map[string]any{}
					for key, v := range valid {
						copy[key] = v
					}
					copy[bad.field] = value
					check(copy)
				}
			}
			data, _ := json.Marshal(valid)
			for key := range valid {
				encoded, _ := json.Marshal(valid[key])
				check(json.RawMessage(strings.TrimSuffix(string(data), "}") + `,"` + key + `":` + string(encoded) + `}`))
				check(json.RawMessage(strings.Replace(string(data), `"`+key+`"`, `"`+strings.ToUpper(key)+`"`, 1)))
			}
			for _, selector := range []string{"profile", "project_id", "worker_id", "server_url", "head", "expected_config"} {
				copy := map[string]any{}
				for key, v := range valid {
					copy[key] = v
				}
				copy[selector] = "other"
				check(copy)
			}
		})
	}
	diagnosticResult(t, call(t, session, "review_request", json.RawMessage(`{"idempotency_key":"bad\ud800","expected_configuration_revision":1,"pull_number":2,"channel":"cli","head_sha":"`+strings.Repeat("a", 40)+`"}`)), "invalid_arguments", false)
	if requests.Load() != 0 {
		t.Fatal("invalid args reached credential/HTTP")
	}
}

func TestReviewExecutionStdioConfiguredReadsAndQueuedReplayNoProviders(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	var posts, reads atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews/") {
			if r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.Header.Get("Authorization") != "Bearer "+token || r.URL.RawQuery != "" {
				t.Error("execution call lost profile binding")
			}
			if r.Method == "POST" {
				posts.Add(1)
			} else {
				reads.Add(1)
			}
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	beforeProviders := f.providers.Load()
	var config client.ReviewExecutionConfiguration
	decodeResult(t, call(t, session, "review_execution_read", nil), &config)
	if config.ProjectID != f.project || config.Revision != 1 || config.CapabilityID != "fixture" {
		t.Fatal("configuration read changed scope")
	}
	args := executionArguments()
	var first, replay client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_request", args), &first)
	decodeResult(t, call(t, session, "review_request", args), &replay)
	if first != replay || first.State != "queued" || first.Revision != 1 {
		t.Fatal("queued receipt changed on replay")
	}
	var operation client.ReviewOperation
	decodeResult(t, call(t, session, "review_operation_read", map[string]any{"operation_id": first.ID}), &operation)
	if operation.ProjectID != f.project || operation.WorkerID != f.sender || operation.State != "queued" || operation.AttemptID != "" {
		t.Fatal("operation read invented provider progress or changed worker")
	}
	args["head_sha"] = strings.Repeat("b", 40)
	diagnosticResult(t, call(t, session, "review_request", args), "idempotency_conflict", false)
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations WHERE id=?`, first.ID).Scan(&count); err != nil || count != 1 || posts.Load() != 3 || reads.Load() != 2 || f.providers.Load() != beforeProviders {
		t.Fatal("execution controls polled, repeated effect or called providers", err)
	}
}

func TestReviewExecutionStdioUnknownReceiptAndWholeBudget(t *testing.T) {
	for _, mode := range []string{"dropped", "malformed", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f, handler, _ := reviewControlsFixture(t)
			var posts atomic.Int64
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "deadline" && r.URL.Path == "/api/v1/instance" {
					select {
					case <-time.After(1200 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
				}
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews/requests") && posts.Add(1) == 1 {
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, r)
					if rec.Code != 202 {
						t.Error("fixture request did not commit")
						return
					}
					if mode == "dropped" {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
						return
					}
					if mode == "deadline" {
						select {
						case <-r.Context().Done():
						case <-time.After(6 * time.Second):
						}
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(202)
					w.Write([]byte(`{"id":"invalid"}`))
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer service.Close()
			session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
			start := time.Now()
			result := call(t, session, "review_request", executionArguments())
			elapsed := time.Since(start)
			code := "connection_failed"
			if mode == "malformed" {
				code = "invalid_response"
			}
			if mode == "deadline" {
				code = "deadline_exceeded"
			}
			failure := diagnosticResult(t, result, code, true)
			if posts.Load() != 1 || !strings.Contains(failure.Action, "same idempotency key") {
				t.Fatal("unknown request automatically retried or lost recovery meaning")
			}
			if mode == "deadline" && (elapsed < 4500*time.Millisecond || elapsed > 5500*time.Millisecond) {
				t.Fatal("request got a new budget after handshake", elapsed)
			}
			var receipt client.ReviewRequestReceipt
			decodeResult(t, call(t, session, "review_request", executionArguments()), &receipt)
			var count int
			if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_operations WHERE id=?`, receipt.ID).Scan(&count); err != nil || count != 1 || posts.Load() != 2 {
				t.Fatal("same-key recovery duplicated durable request", err)
			}
		})
	}
}

func TestReviewExecutionStdioCancellationReleasesOwnedHTTP(t *testing.T) {
	f, handler, _ := reviewControlsFixture(t)
	entered, released := make(chan struct{}), make(chan struct{})
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reviews/requests") {
			if posts.Add(1) > 1 {
				handler.ServeHTTP(w, r)
				return
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			if rec.Code != 202 {
				t.Error("fixture request did not commit")
				return
			}
			close(entered)
			select {
			case <-r.Context().Done():
				close(released)
			case <-time.After(3 * time.Second):
			}
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := reviewControlsStdio(t, reviewControlsProfile(t, f, service.URL), "chosen")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "review_request", Arguments: executionArguments()})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request not admitted")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled call reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("SDK cancellation did not return")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("MCP cancellation retained owned HTTP call")
	}
	if posts.Load() != 1 {
		t.Fatal("cancelled call retried provider intent")
	}
	var receipt client.ReviewRequestReceipt
	decodeResult(t, call(t, session, "review_request", executionArguments()), &receipt)
}
