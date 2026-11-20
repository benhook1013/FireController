package retention

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

type historyDiagnosticTestError int

func (e historyDiagnosticTestError) Error() string { return "private diagnostic" }
func (e historyDiagnosticTestError) Code() int     { return int(e) }

func TestHistoryFailureDiagnosticsStayOutOfDurableReport(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, "deadline"},
		{context.Canceled, "cancelled"},
		{core.Fail("overloaded", "private context", 503), "deadline_or_busy"},
		{core.Fail("storage_unavailable", "private context", 500), "storage"},
		{historyDiagnosticTestError(787), "constraint"},
		{historyDiagnosticTestError(5), "busy"},
		{historyDiagnosticTestError(9), "interrupted"},
		{errors.New("private context"), "other"},
	} {
		err := historyFailure("snapshot_batch", tc.err)
		var phase *historyPhaseError
		if !errors.As(err, &phase) || !errors.Is(err, tc.err) || strings.Contains(err.Error(), "private") ||
			historyErrorClass(err) != tc.want {
			t.Fatalf("classification mismatch for %s", tc.want)
		}
	}
	r := Report{Deleted: map[string]int64{}, Error: "review_history_failed", HistoryFailurePhase: "snapshot_batch", HistoryFailureClass: "deadline_or_busy"}
	raw, err := json.Marshal(r)
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 5 ||
		string(fields["error"]) != "\"review_history_failed\"" {
		t.Fatal("durable retention report format changed")
	}
	if historyFailure("snapshot_batch", nil) != nil {
		t.Fatal("successful history phase reported a failure")
	}
}
