package core_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/server"
)

type admissionFixture struct {
	store                       *core.Store
	mux                         http.Handler
	admin, project, worker, job string
	providerCalls               int
}

func newAdmissionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	f := &admissionFixture{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "private")
	var err error
	f.store, err = core.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	if err := jobs.EnsureSchema(ctx, f.store); err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureSchema(ctx, f.store); err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f.admin = strings.TrimSpace(string(token))
	mux := server.New(f.store, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		f.providerCalls++
		return server.RepositoryIdentity{ID: 1, FullName: "owner/admission"}, nil
	}))
	jobs.Register(mux, f.store)
	inbox.Register(mux, f.store)
	f.mux = mux
	create := func(path, body, key string, status int) string {
		response := f.request(http.MethodPost, path, body, []string{key}, "")
		if response.Code != status {
			t.Fatalf("fixture create: %d %s", response.Code, response.Body.String())
		}
		var receipt struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.ID == "" {
			t.Fatalf("fixture receipt: %v %s", err, response.Body.String())
		}
		return receipt.ID
	}
	f.project = create("/api/v1/projects", `{"name":"Admission project","repository":"owner/admission"}`, "setup-project", 200)
	f.worker = create("/api/v1/projects/"+f.project+"/workers", `{"name":"Builder"}`, "setup-worker", 200)
	f.job = create("/api/v1/projects/"+f.project+"/jobs", `{"title":"Admission job"}`, "setup-job", 201)
	return f
}

func (f *admissionFixture) request(method, path, body string, keys []string, rawQuery string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.URL.RawQuery = rawQuery
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Worker", f.worker)
	if keys != nil {
		r.Header["Idempotency-Key"] = keys
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}

func admissionError(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	var value struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if response.Code != 400 || value.Error.Code != code || value.RequestID == "" {
		t.Fatalf("want safe 400 %s, got %d %s", code, response.Code, response.Body.String())
	}
}

func TestMutationAdmissionAcrossPublicRoutes(t *testing.T) {
	f := newAdmissionFixture(t)
	base := "/api/v1/projects/" + f.project
	routes := []struct{ name, method, path, body, code string }{
		{"project", "POST", "/api/v1/projects", `{"name":"Other","repository":"owner/other"}`, "validation_failed"},
		{"worker", "POST", base + "/workers", `{"name":"Other"}`, "validation_failed"},
		{"job", "POST", base + "/jobs", `{"title":"Other"}`, "idempotency_key_required"},
		{"job-edit", "PATCH", base + "/jobs/" + f.job, `{"expected_revision":1,"title":"Other"}`, "idempotency_key_required"},
		{"message", "POST", base + "/messages", `{"recipient_worker_id":"` + f.worker + `","body":"Private synthetic body"}`, "validation_failed"},
		{"seen", "POST", base + "/messages/missing/seen", `{}`, "validation_failed"},
		{"ack", "POST", base + "/messages/missing/ack", `{}`, "validation_failed"},
	}
	cases := []struct {
		name string
		keys []string
	}{
		{"missing", nil}, {"empty", []string{""}}, {"duplicate", []string{"one", "two"}},
		{"padded", []string{" padded "}}, {"oversized", []string{strings.Repeat("k", 129)}},
		{"line-break", []string{"key\r\nprivate-value"}}, {"nul", []string{"key\x00private-value"}},
		{"invalid-utf8", []string{"key\xff"}},
	}
	for _, route := range routes {
		for _, test := range cases {
			t.Run(route.name+"/"+test.name, func(t *testing.T) {
				admissionError(t, f.request(route.method, route.path, route.body, test.keys, ""), route.code)
			})
		}
	}
	if f.providerCalls != 1 {
		t.Fatalf("rejected input called repository resolver: %d", f.providerCalls)
	}
	var receipts int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM receipts").Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if receipts != 3 {
		t.Fatalf("rejected mutations wrote receipts: %d", receipts)
	}
}

func TestQueryAdmissionAcrossPublicRoutes(t *testing.T) {
	f := newAdmissionFixture(t)
	base := "/api/v1/projects/" + f.project
	for _, route := range []struct{ path, code string }{
		{"/api/v1/projects", "validation_failed"}, {base + "/workers", "validation_failed"},
		{base + "/jobs", "invalid_query"}, {base + "/jobs/" + f.job, "invalid_query"}, {base + "/jobs/missing", "invalid_query"}, {base + "/jobs/missing/history", "invalid_query"}, {base + "/jobs/" + f.job + "/history", "invalid_query"},
		{base + "/inbox/" + f.worker, "validation_failed"}, {base + "/conversations/missing", "validation_failed"},
	} {
		for _, query := range []string{"limit=%ZZ", "limit=1&%ZZ=private-value", "limit=1&limit=2", "cursor=a&cursor=b", "unsupported=private-value", "limit=1;cursor=x"} {
			response := f.request(http.MethodGet, route.path, "", nil, query)
			admissionError(t, response, route.code)
			if strings.Contains(response.Body.String(), "private-value") {
				t.Fatal("query input reflected in diagnostic")
			}
		}
	}
	admissionError(t, f.request(http.MethodGet, base+"/workers", "", nil, "cursor="+strings.Repeat("x", 16<<10)), "validation_failed")
	if f.providerCalls != 1 {
		t.Fatalf("lists called repository resolver: %d", f.providerCalls)
	}
}

func TestSharedAdmissionPreservesValuesAndMutationKeyValidation(t *testing.T) {
	for _, value := range []string{"key", strings.Repeat("k", 128), strings.Repeat("é", 64), "key with internal spaces"} {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Idempotency-Key", value)
		got, err := core.IdempotencyKey(r, "validation_failed")
		if err != nil || got != value {
			t.Fatalf("valid key changed: %q %v", got, err)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.URL.RawQuery = "cursor=a%2Bb&limit=1"
	query, err := core.QueryKeys(r, "invalid_query", "cursor", "limit")
	if err != nil || query.Get("cursor") != "a+b" || query.Get("limit") != "1" {
		t.Fatalf("valid query changed: %v %v", query, err)
	}
	f := newAdmissionFixture(t)
	for _, key := range []string{"", " padded", strings.Repeat("k", 129), "key\r", "key\n", "key\x00", "key\xff"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := f.store.Mutate(ctx, core.Scope{}, core.Mutation{Operation: "probe", Target: "target", Key: key}, func(context.Context, *sql.Tx) (any, error) {
			t.Fatal("invalid key reached mutation callback")
			return nil, nil
		})
		cancel()
		w := httptest.NewRecorder()
		core.WriteError(w, err)
		admissionError(t, w, "validation_failed")
	}
}

func TestJobMutationQueriesCannotWrite(t *testing.T) {
	f := newAdmissionFixture(t)
	base := "/api/v1/projects/" + f.project
	for _, route := range []struct{ method, path, body string }{
		{"POST", base + "/jobs", `{"title":"Query must not create"}`},
		{"PATCH", base + "/jobs/" + f.job, `{"expected_revision":1,"title":"Query must not edit"}`},
	} {
		for _, query := range []string{"unknown=private-value", "unknown=1&unknown=2", "status=blocked", "x=%ZZ"} {
			admissionError(t, f.request(route.method, route.path, route.body, []string{"reject-query"}, query), "invalid_query")
		}
	}
	var receipts, events, jobs, history int
	for table, dest := range map[string]*int{"receipts": &receipts, "events": &events, "jobs": &jobs, "job_history": &history} {
		if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if receipts != 3 || events != 3 || jobs != 1 || history != 1 || f.providerCalls != 1 {
		t.Fatalf("rejected query wrote state or called provider: %d %d %d %d %d", receipts, events, jobs, history, f.providerCalls)
	}
}
