package mcpadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperationStatusArgumentsAndDefinition(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	for _, raw := range []string{`{"operation_id":"` + id + `"}`, `{"operation_id":"` + id + `","wait_seconds":50,"diagnostics":true}`, `{"operation_id":"` + id + `","diagnostics":false}`} {
		v, err := operationStatusArguments(json.RawMessage(raw))
		if err != nil || v.ID != id {
			t.Fatal(raw, v, err)
		}
	}
	for _, raw := range []string{`{}`, `{"operation_id":"bad"}`, `{"operation_id":"` + id + `","wait_seconds":51}`, `{"operation_id":"` + id + `","wait_seconds":null}`, `{"operation_id":"` + id + `","diagnostics":null}`, `{"operation_id":"` + id + `","diagnostics":"true"}`, `{"operation_id":"` + id + `","diagnostics":true,"diagnostics":false}`, `{"operation_id":"` + id + `","extra":true}`} {
		if _, err := operationStatusArguments(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid status arguments accepted", raw)
		}
	}
	d := operationStatusDefinition()
	body, _ := json.Marshal(d.schema)
	if !strings.Contains(string(body), "diagnostics") || !strings.Contains(string(body), "50") {
		t.Fatal("compact/diagnostic discovery missing", string(body))
	}
}
