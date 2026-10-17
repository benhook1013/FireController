package jobs

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/server"
)

func TestJobContextHelperSQLiteLocksRemainRetryable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if err = inbox.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	mux := server.New(store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: 991, FullName: "synthetic/context"}, nil
	}))
	Register(mux, store)
	inbox.Register(mux, store)
	request := func(path string, body any) string {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		r.Header.Set("Idempotency-Key", core.NewID("fixture"))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code < 200 || w.Code > 299 {
			t.Fatalf("fixture setup status=%d", w.Code)
		}
		var receipt struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || receipt.ID == "" {
			t.Fatal("fixture identity unavailable")
		}
		return receipt.ID
	}
	project := request("/api/v1/projects", map[string]any{"name": "Context storage", "repository": "synthetic/context"})
	worker := request("/api/v1/projects/"+project+"/workers", map[string]any{"name": "Context reader"})
	job := request("/api/v1/projects/"+project+"/jobs", map[string]any{"title": "Retained context", "brief": "Literal selected job", "worker_id": worker})
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	r.Header.Set("X-FC-Worker", worker)
	scope, err := store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	// A temporary view routes just one helper table to an attached disposable
	// SQLite database. An actual exclusive file lock blocks that helper while
	// the selected job and authorization continue to read from the main DB.
	// No driver fake or production injection seam is needed.
	store.DB().SetMaxOpenConns(1)
	store.DB().SetMaxIdleConns(1)
	read := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/projects/"+project+"/jobs/"+job+"/context", nil)
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		req.Header.Set("X-FC-Instance", store.InstanceID())
		req.Header.Set("X-FC-Worker", worker)
		result := httptest.NewRecorder()
		mux.ServeHTTP(result, req)
		return result
	}
	for _, table := range []string{"job_checkpoints", "job_checklists", "worker_primary_jobs", "worker_pause_state"} {
		t.Run(table, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "helper.sqlite")
			if _, err := store.DB().Exec(`ATTACH DATABASE ? AS context_locked`, path); err != nil {
				t.Fatal(err)
			}
			defer store.DB().Exec(`DETACH DATABASE context_locked`)
			if _, err := store.DB().Exec("CREATE TABLE context_locked." + table + " AS SELECT * FROM main." + table); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().Exec("CREATE TEMP VIEW " + table + " AS SELECT * FROM context_locked." + table); err != nil {
				t.Fatal(err)
			}
			defer store.DB().Exec("DROP VIEW temp." + table)
			if result := read(); result.Code != 200 {
				t.Fatalf("unlocked helper fixture status=%d", result.Code)
			}
			locker, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer locker.Close()
			locker.SetMaxOpenConns(1)
			if _, err = locker.Exec("BEGIN EXCLUSIVE"); err != nil {
				t.Fatal(err)
			}
			defer locker.Exec("ROLLBACK")
			// Confirm this is a real driver busy code and that the selected job still
			// reads successfully before exercising the complete public context route.
			var n int
			err = store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n)
			var coded interface{ Code() int }
			if !errors.As(err, &coded) || coded.Code()&255 != 5 {
				t.Fatalf("fixture did not produce SQLite busy: %T", err)
			}
			err = store.Read(t.Context(), scope, func(ctx context.Context, tx *sql.Tx) error { _, err := readJob(ctx, tx, project, job); return err })
			if err != nil {
				t.Fatal("helper lock blocked initial job/auth reads", err)
			}
			result := read()
			var failure struct {
				Error core.Error `json:"error"`
			}
			if err = json.Unmarshal(result.Body.Bytes(), &failure); err != nil {
				t.Fatal(err)
			}
			if result.Code != http.StatusServiceUnavailable || failure.Error.Code != "overloaded" {
				t.Fatalf("helper lock status=%d code=%s; wanted retryable overloaded", result.Code, failure.Error.Code)
			}
			if _, err = locker.Exec("ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			if result = read(); result.Code != 200 {
				t.Fatalf("context did not recover after helper lock release: status=%d", result.Code)
			}
		})
	}
}
