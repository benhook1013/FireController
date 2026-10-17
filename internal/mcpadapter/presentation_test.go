package mcpadapter_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/presentation"
	"github.com/benhook1013/FireController/internal/profile"
)

func presentationSDKFixture(t *testing.T) *mutationAPI {
	t.Helper()
	f := mutationFixture(t)
	if e := presentation.EnsureSchema(t.Context(), f.store); e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	presentation.Register(mux, f.store)
	f.handler = mux
	return f
}
func TestPresentationReadOfficialSDKPublicAPIAndDiscovery(t *testing.T) {
	f := presentationSDKFixture(t)
	var reads, writes atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/presentation") {
			if r.Header.Get("X-FC-Worker") != f.sender || r.Header.Get("X-FC-Instance") != f.store.InstanceID() {
				t.Error("lost profile binding")
			}
			if r.Method == "GET" {
				reads.Add(1)
			} else {
				writes.Add(1)
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	session := connect(t, f.adapter(t, service.URL, f.sender), "")
	tools, e := allDiscoveredTools(t, session)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, tool := range tools.Tools {
		if tool.Name != "presentation_read" {
			continue
		}
		found = true
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || *a.OpenWorldHint || a.DestructiveHint == nil || *a.DestructiveHint {
			t.Fatal("incorrect read annotations", a)
		}
		var schema struct {
			Properties map[string]any `json:"properties"`
			Required   []string       `json:"required"`
			Additional bool           `json:"additionalProperties"`
		}
		raw, _ := json.Marshal(tool.InputSchema)
		if json.Unmarshal(raw, &schema) != nil || len(schema.Properties) != 0 || len(schema.Required) != 0 || schema.Additional {
			t.Fatal("presentation schema accepts selectors", string(raw))
		}
	}
	if !found {
		t.Fatal("presentation_read missing across SDK discovery pages")
	}
	before := mapSDKCounts(t, f)
	var zero client.PresentationSnapshot
	decodeResult(t, call(t, session, "presentation_read", map[string]any{}), &zero)
	if zero.Presentation.ProjectID != f.project || zero.Presentation.Revision != 0 || zero.Presentation.Name != "project" || zero.EventCursor == "" || reads.Load() != 1 || writes.Load() != 0 || !reflect.DeepEqual(before, mapSDKCounts(t, f)) {
		t.Fatal("read not single provider-free snapshot", zero)
	}
	// The administrator changes configuration via its public handler, independently
	// of the read-only tool's credential. The following SDK read sees that state.
	r := httptest.NewRequest("PUT", "/api/v1/projects/"+f.project+"/presentation", strings.NewReader(`{"expected_revision":0,"presentation":{"name":"SDK <literal>","headings":{"jobs":"Tasks"}}}`))
	r.Header.Set("Authorization", "Bearer "+f.admin)
	r.Header.Set("X-FC-Worker", f.sender)
	r.Header.Set("Idempotency-Key", "sdk-fixture-change")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	before = mapSDKCounts(t, f)
	var configured client.PresentationSnapshot
	decodeResult(t, call(t, session, "presentation_read", map[string]any{}), &configured)
	if configured.Presentation.Revision != 1 || configured.Presentation.Name != "SDK <literal>" || configured.Presentation.Headings["jobs"] != "Tasks" || !reflect.DeepEqual(before, mapSDKCounts(t, f)) || f.providers.Load() != 1 || writes.Load() != 0 {
		t.Fatal(configured)
	}
	for _, args := range []any{map[string]any{"project_id": "foreign"}, map[string]any{"worker_id": f.recipient}, map[string]any{"profile": "other"}, map[string]any{"refresh": true}, json.RawMessage(`null`), json.RawMessage(`[]`)} {
		diagnosticResult(t, call(t, session, "presentation_read", args), "invalid_arguments", false)
	}
	if reads.Load() != 2 || !reflect.DeepEqual(before, mapSDKCounts(t, f)) {
		t.Fatal("badargs contacted domain")
	}
}
func TestPresentationReadSDKInvalidBeforeCredentialAndCrossProject(t *testing.T) {
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer service.Close()
	binding := localContext()
	binding.ServerURL = service.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-presentation-credential"), "chosen"), "")
	for _, args := range []any{map[string]any{"project_id": "foreign"}, map[string]any{"worker_id": "foreign"}, map[string]any{"cursor": "secret"}, json.RawMessage(`null`)} {
		diagnosticResult(t, call(t, session, "presentation_read", args), "invalid_arguments", false)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid args read credential or HTTP")
	}
	f := presentationSDKFixture(t)
	real := httptest.NewServer(f.handler)
	defer real.Close()
	setup := httptest.NewRequest("POST", "/api/v1/projects", strings.NewReader(`{"name":"Other","repository":"example/other-presentation"}`))
	setup.Header.Set("Authorization", "Bearer "+f.admin)
	setup.Header.Set("Idempotency-Key", "other-presentation-project")
	record := httptest.NewRecorder()
	f.handler.ServeHTTP(record, setup)
	var created struct {
		ID string `json:"id"`
	}
	if record.Code != 200 || json.Unmarshal(record.Body.Bytes(), &created) != nil || created.ID == "" {
		t.Fatal("other project setup", record.Code, record.Body.String())
	}
	foreign := profile.Context{Profile: "foreign", ServerURL: real.URL, InstanceID: f.store.InstanceID(), ProjectID: created.ID, WorkerID: f.sender}
	otherSession := connect(t, adapter(t, profiles(t, map[string]profile.Context{"foreign": foreign}, f.credential), "foreign"), "")
	before := mapSDKCounts(t, f)
	result := call(t, otherSession, "presentation_read", map[string]any{})
	diagnosticResult(t, result, "forbidden", false)
	if !result.IsError || !reflect.DeepEqual(before, mapSDKCounts(t, f)) {
		t.Fatal("cross-project tool exposed state", result)
	}
}
