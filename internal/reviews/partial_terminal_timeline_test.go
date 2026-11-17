package reviews

import "testing"

func TestPartialTerminalTimelineClassification(t *testing.T) {
	for _, outcome := range []string{"failed", "cancelled", "rate_limited"} {
		r := RunSummary{Source: "native-executor", Channel: "cli", Outcome: outcome, Found: 3}
		if !isPartialCLIResult(r) {
			t.Fatal("retained findings hidden", outcome)
		}
		r.Found = 0
		if isPartialCLIResult(r) {
			t.Fatal("invented findings", outcome)
		}
		r.Found = 3
		r.Complete = true
		if isPartialCLIResult(r) {
			t.Fatal("complete presented partial", outcome)
		}
	}
}
