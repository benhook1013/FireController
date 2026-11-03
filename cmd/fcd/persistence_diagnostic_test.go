package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestDaemonPersistenceDiagnosticLogsOnlySafeFailureFacts(t *testing.T) {
	op := "review_operation_" + strings.Repeat("a", 32)
	attempt := "review_attempt_" + strings.Repeat("b", 32)
	for _, tc := range []struct {
		name, code string
		details    map[string]string
		want       int
	}{
		{"original facts", "review_persistence_failed", map[string]string{"stage": "outcome_write", "cause": "overloaded", "operation_id": op, "attempt_id": attempt, "raw": "private SQL secret"}, 4},
		{"unknown values", "review_persistence_failed", map[string]string{"stage": "private SQL", "cause": "private SQL", "operation_id": "private SQL", "attempt_id": "private SQL"}, 0},
		{"other error", "storage_unavailable", map[string]string{"stage": "outcome_write", "cause": "overloaded", "operation_id": op}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			diagnosticError(&output, &core.Error{Code: tc.code, Message: "Safe failure.", Status: 503, Details: tc.details})
			var body struct {
				Error struct {
					Code, Message string
					Details       map[string]string
				}
			}
			if err := json.Unmarshal(output.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != tc.code || body.Error.Message != "Safe failure." || len(body.Error.Details) != tc.want || strings.Contains(output.String(), "private") {
				t.Fatal(output.String())
			}
			if tc.want > 0 && (body.Error.Details["operation_id"] != op || body.Error.Details["attempt_id"] != attempt || body.Error.Details["stage"] != "outcome_write" || body.Error.Details["cause"] != "overloaded") {
				t.Fatal(output.String())
			}
		})
	}
}
