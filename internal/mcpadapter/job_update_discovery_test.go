package mcpadapter_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/profile"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestJobUpdateDiscoveryKeepsRootAndProgressFieldsTyped(t *testing.T) {
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": localContext()}, credential(t)), "alpha"), "")
	listed, err := allDiscoveredTools(t, session)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "job_update" {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if json.Unmarshal(raw, &schema) != nil {
			t.Fatal("invalid discovered schema")
		}
		if schema["type"] != "object" || schema["anyOf"] != nil || schema["oneOf"] != nil || schema["additionalProperties"] != false {
			t.Fatal("host would receive an ambiguous object union")
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatal("root properties missing")
		}
		for _, name := range []string{"job_id", "idempotency_key", "expected_revision", "title", "brief", "worker_id", "status", "progress"} {
			if props[name] == nil {
				t.Fatal("undiscoverable field", name)
			}
		}
		if !reflect.DeepEqual(schema["required"], []any{"job_id", "idempotency_key", "expected_revision"}) {
			t.Fatal("required guard fields changed")
		}
		progress := props["progress"].(map[string]any)
		members := progress["properties"].(map[string]any)
		if progress["type"] != "object" || progress["additionalProperties"] != false || members["next"].(map[string]any)["type"] != "string" || members["evidence"].(map[string]any)["type"] != "array" {
			t.Fatal("progress members lost their types")
		}
		if !strings.Contains(tool.Description, "at least one edit") || !strings.Contains(tool.Description, "progress is an object") {
			t.Fatal("patch shape is not explained at discovery")
		}
		return
	}
	t.Fatal("job_update missing")
}

func TestJobUpdateNamesInvalidFieldWithoutCredentialsOrHTTP(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	binding := localContext()
	binding.ServerURL = server.URL
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"alpha": binding}, "/missing-private-credential"), "alpha"), "")
	base := map[string]any{"job_id": "job-one", "idempotency_key": "job-patch", "expected_revision": 1}
	cases := []struct {
		field string
		edits map[string]any
		want  string
	}{
		{"expected_revision", map[string]any{"expected_revision": 0, "brief": "ok"}, "expected_revision"},
		{"brief bytes", map[string]any{"brief": strings.Repeat("雪", 10923)}, "brief"},
		{"flat next", map[string]any{"next": "PRIVATE_BODY"}, "progress.next"},
		{"flat evidence", map[string]any{"evidence": []string{"PRIVATE_BODY"}}, "progress.evidence"},
		{"progress string", map[string]any{"progress": "PRIVATE_BODY"}, "progress"},
		{"next type", map[string]any{"progress": map[string]any{"next": 123}}, "progress.next"},
		{"next bytes", map[string]any{"progress": map[string]any{"next": strings.Repeat("雪", 2731)}}, "progress.next"},
		{"evidence type", map[string]any{"progress": map[string]any{"evidence": "PRIVATE_BODY"}}, "progress.evidence"},
		{"evidence null member", map[string]any{"progress": map[string]any{"evidence": []any{nil}}}, "progress.evidence"},
		{"status", map[string]any{"status": "PRIVATE_BODY"}, "status"},
		{"null brief", map[string]any{"brief": nil}, "brief"},
		{"private unknown key", map[string]any{"PRIVATE_BODY": "value"}, "job_update"},
		{"empty edit", map[string]any{}, "at least one"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			args := map[string]any{}
			for k, v := range base {
				args[k] = v
			}
			for k, v := range tc.edits {
				args[k] = v
			}
			d := diagnosticResult(t, call(t, session, "job_update", args), "invalid_arguments", false)
			if !strings.Contains(d.Message, tc.want) || !strings.Contains(d.Action, "No request was sent") || strings.Contains(d.Message+d.Action, "PRIVATE_BODY") || strings.Contains(d.Action, "startup binding") {
				t.Fatal("unhelpful or private diagnostic", d)
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("rejected patch accessed HTTP")
	}
}
