package mcpadapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestSchedulingToolSchemasMatchReasonAndKeyValidation(t *testing.T) {
	for _, definition := range reviewSchedulingDefinitions() {
		if !reviewSchedulingWrite(definition.name) {
			continue
		}
		input, err := json.Marshal(definition.schema)
		if err != nil {
			t.Fatal(err)
		}
		var schema jsonschema.Schema
		if err = json.Unmarshal(input, &schema); err != nil {
			t.Fatal(err)
		}
		rule, err := schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, key string
			paused    bool
			reason    *string
			want      bool
		}{
			{"one ASCII", "x", false, nil, true},
			{"retain interior spaces", "with spaces", true, nil, true},
			{"flag-shaped literal", "--literal=value", false, nil, true},
			{"at byte limit", strings.Repeat("x", 128), true, nil, true},
			{"too long", strings.Repeat("x", 129), true, nil, false},
			{"non ASCII", "pr-é", true, nil, false},
			{"leading space", " bad", true, nil, false},
			{"trailing space", "bad ", true, nil, false},
			{"line break", "bad\n", true, nil, false},
			{"paused reason", "key", true, ptrString(""), true},
			{"resume reason", "key", false, ptrString(""), false},
		} {
			args := map[string]any{"expected_revision": 1, "idempotency_key": tc.key}
			if definition.name == "review_pull_scheduling_update" {
				args["pull_number"], args["paused"] = 4, tc.paused
				if tc.reason != nil {
					args["reason"] = *tc.reason
				}
			} else {
				if tc.reason != nil {
					continue
				}
				args["automatic_hosted"] = false
			}
			b, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			var instance any
			if err = json.Unmarshal(b, &instance); err != nil {
				t.Fatal(err)
			}
			_, parsed := reviewSchedulingArguments(definition.name, b)
			if (rule.Validate(instance) == nil) != tc.want || (parsed == nil) != tc.want {
				t.Fatalf("%s/%s: schema %v parser %v", definition.name, tc.name, rule.Validate(instance), parsed)
			}
		}
		if definition.name == "review_pull_scheduling_update" {
			if !strings.Contains(string(input), "256 UTF-8 bytes") {
				t.Fatal("reason limit missing")
			}
			b, _ := json.Marshal(map[string]any{"expected_revision": 1, "pull_number": 4, "paused": true, "reason": strings.Repeat("é", 129), "idempotency_key": "key"})
			if _, err := reviewSchedulingArguments(definition.name, b); err == nil {
				t.Fatal("invalid UTF-8 byte budget accepted")
			}
		}
	}
}

func ptrString(value string) *string { return &value }
