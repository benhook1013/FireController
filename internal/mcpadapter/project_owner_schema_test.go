package mcpadapter

import (
	"encoding/json"
	"testing"
)

func TestOwnerManagementSchemasNeverEmitNullRequired(t *testing.T) {
	for _, def := range ownerManagementDefinitions() {
		t.Run(def.name, func(t *testing.T) {
			raw, err := json.Marshal(def.schema)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			required, present := decoded["required"]
			if def.name == "review_policy_read" {
				if present {
					t.Fatalf("argument-free tool unexpectedly has required: %v", required)
				}
				return
			}
			values, ok := required.([]any)
			if !present || !ok || len(values) == 0 {
				t.Fatalf("required arguments lost: %s", raw)
			}
			props := decoded["properties"].(map[string]any)
			for _, value := range values {
				name, ok := value.(string)
				if !ok {
					t.Fatalf("non-string required argument: %v", value)
				}
				if _, ok := props[name]; !ok {
					t.Fatalf("missing required property %q", name)
				}
			}
		})
	}
}
