package mcpadapter

import (
	"encoding/json"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestActionsDiagnosticMCPStrictReadArgumentsAndBudget(t *testing.T) {
	for _, raw := range []string{`{}`, `{"age_hours":1,"limit":1}`, `{"age_hours":720,"limit":20}`} {
		args, err := actionsDiagnosticArguments(json.RawMessage(raw))
		if err != nil || toolCallBudget("actions_liveness_read", args) < client.ActionsDiagnosticBudget {
			t.Fatal(raw, args, err)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{"limit":null}`, `{"limit":0}`, `{"limit":21}`, `{"age_hours":0}`, `{"age_hours":721}`, `{"age_hours":1.5}`, `{"limit":1,"limit":2}`, `{"repository":"foreign/repo"}`, `{"cancel":true}`, `{"Limit":1}`, `{"limit":"1"}`} {
		if _, err := actionsDiagnosticArguments(json.RawMessage(raw)); err == nil {
			t.Fatal("unsafe selector accepted", raw)
		}
	}
	definition := actionsDiagnosticDefinition()
	if definition.name != "actions_liveness_read" || definition.schema["additionalProperties"] != false {
		t.Fatal("diagnostic discovery changed", definition)
	}
}
