package cli

import (
	"strings"
	"testing"
)

func TestReviewOperationStatusParseCompactAndDiagnostics(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	for _, args := range [][]string{{id}, {id, "--wait-seconds", "900"}, {"--diagnostics", id, "--wait-seconds=50"}} {
		v, ok := parseReviewOperationStatus(args)
		if !ok || v.ID != id {
			t.Fatal(args, v, ok)
		}
		if args[0] == "--diagnostics" && (!v.Diagnostics || v.WaitSeconds != 50) {
			t.Fatal("diagnostic output view lost", v)
		}
	}
	for _, args := range [][]string{{}, {"bad"}, {id, id}, {id, "--wait-seconds", "901"}, {id, "--wait-seconds", "01"}, {id, "--wait-seconds", "-1"}, {id, "--wait-seconds"}, {id, "--diagnostics=true"}, {id, "--diagnostics", "--diagnostics"}, {id, "--cursor", "x"}, {id, "--wait-seconds=1", "--wait-seconds=2"}} {
		if _, ok := parseReviewOperationStatus(args); ok {
			t.Fatal("invalid arguments accepted", args)
		}
	}
}
