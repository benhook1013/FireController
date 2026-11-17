package mcpadapter

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/client"
	"testing"
)

func TestActionsRunDiagnosticMCPStrictArgumentsAndBudget(t *testing.T) {
	for _, raw := range []string{`{"run_id":50,"run_attempt":2}`, `{"run_id":50,"run_attempt":2,"limit":20}`} {
		args, err := actionsRunDiagnosticArguments(json.RawMessage(raw))
		if err != nil || toolCallBudget("actions_run_read", args) < client.ActionsDiagnosticBudget {
			t.Fatal(raw, args, err)
		}
	}
	for _, raw := range []string{`{}`, `null`, `[]`, `{"run_id":50}`, `{"run_id":null,"run_attempt":2}`, `{"run_id":50,"run_attempt":0}`, `{"run_id":50,"run_attempt":2,"limit":0}`, `{"run_id":50,"run_attempt":2,"limit":21}`, `{"run_id":50,"run_attempt":2,"limit":null}`, `{"run_id":50,"run_attempt":2,"run_id":51}`, `{"run_id":50,"run_attempt":2,"repository":"foreign/repo"}`, `{"run_id":50,"run_attempt":2,"cancel":true}`, `{"run_id":50,"run_attempt":2.5}`} {
		if _, err := actionsRunDiagnosticArguments(json.RawMessage(raw)); err == nil {
			t.Fatal("unsafe args accepted", raw)
		}
	}
}
