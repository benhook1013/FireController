package mcpadapter

import (
	"encoding/json"
	"errors"
	"fmt"
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
	if !strings.Contains(string(body), "diagnostics") || !strings.Contains(string(body), fmt.Sprint(maxToolWaitSeconds)) || !strings.Contains(d.description, fmt.Sprintf("0-%d seconds (default 0)", maxToolWaitSeconds)) {
		t.Fatal("compact/diagnostic discovery missing", string(body))
	}
}

func TestOperationStatusWaitRangeDiagnosticAndValidBounds(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	for _, seconds := range []int{-1, 51, 60, 901} {
		_, err := operationStatusArguments(json.RawMessage(fmt.Sprintf(`{"operation_id":%q,"wait_seconds":%d}`, id, seconds)))
		var diagnostic *Diagnostic
		if !errors.As(err, &diagnostic) || diagnostic.Code != "invalid_arguments" || !strings.Contains(diagnostic.Message, "wait_seconds") || !strings.Contains(diagnostic.Message, fmt.Sprintf("0 to %d", maxToolWaitSeconds)) || !strings.Contains(diagnostic.Message, "CLI") || !strings.Contains(diagnostic.Message, "900") || !strings.Contains(diagnostic.Action, "No operation request was sent") {
			t.Fatal(seconds, err)
		}
	}
	for _, seconds := range []int{0, maxToolWaitSeconds} {
		got, err := operationStatusArguments(json.RawMessage(fmt.Sprintf(`{"operation_id":%q,"wait_seconds":%d}`, id, seconds)))
		if err != nil || got.WaitSeconds != seconds {
			t.Fatal("valid bound changed", got, err)
		}
	}
	got, err := operationStatusArguments(json.RawMessage(fmt.Sprintf(`{"operation_id":%q}`, id)))
	if err != nil || got.WaitSeconds != 0 {
		t.Fatal("default wait changed", got, err)
	}
}
