package cli_test

import (
	"encoding/json"
	"github.com/benhook1013/FireController/internal/cli"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefreshIDCLIRejectsBeforeProfileWithNamedDiagnostic(t *testing.T) {
	for _, id := range []string{strings.Repeat("a", 32), "review_refresh_" + strings.Repeat("A", 32), "review_operation_" + strings.Repeat("a", 32), "PRIVATE_INVALID_REFRESH_ID"} {
		code, out, stderr := mutationInvoke(t, filepath.Join(t.TempDir(), "missing.json"), "chosen", "reviews", "source-operation", id)
		var v struct {
			Error struct {
				Code, Message, Action string
				Details               map[string]string
			}
		}
		if code != cli.ExitUsage || out != "" || json.Unmarshal([]byte(stderr), &v) != nil || v.Error.Code != "invalid_arguments" || !strings.Contains(v.Error.Message, "refresh_id") || v.Error.Details["field"] != "refresh_id" || !strings.Contains(v.Error.Action, "No request was sent") || strings.Contains(stderr, "PRIVATE_INVALID_REFRESH_ID") {
			t.Fatal("malformed ID did not get precise offline diagnostic", code, stderr)
		}
	}
}
