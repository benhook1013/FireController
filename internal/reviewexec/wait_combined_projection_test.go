package reviewexec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCombinedWaitProjectionKeepsEveryLegacyBoundary(t *testing.T) {
	zero, limit, window := int64(0), int64(8), int64(3600000)
	original := &OperationWait{BaselineAttemptID: "review_attempt_" + strings.Repeat("b", 32), RetryNotBefore: "2026-01-01T00:10:00Z", Paused: true, QueueAheadPullNumber: 9, CLIAllowance: &AllowanceObservation{Kind: "cli_exhausted", ObservedAt: "2026-01-01T00:00:00Z", Remaining: &zero, Limit: &limit, FullCapacityInMS: &zero, RollingWindowMS: &window}, SourceRetry: &SourceRetry{AttemptID: "review_attempt_" + strings.Repeat("c", 32), ObservedAt: "2026-01-01T00:00:00Z", RetryNotBefore: "2026-01-01T00:01:00Z", Sequence: 1}}
	before, _ := json.Marshal(original)
	for _, channel := range []string{"cli", "hosted"} {
		for _, version := range []string{"", "1", "2", "3", "4", "5", "6", "7", "8", "unknown"} {
			t.Run(channel+"/"+version, func(t *testing.T) {
				got := operationWaitForResponse(original, channel, version)
				if version == "" || version == "8" || version == "unknown" {
					if got != nil {
						t.Fatal("unnegotiated facts leaked")
					}
					return
				}
				expected := *original
				if version != "5" {
					expected.QueueAheadPullNumber = 0
				}
				if channel == "hosted" && version != "7" || version != "4" && version != "6" && version != "7" {
					expected.SourceRetry = nil
				}
				if version != "3" && version != "5" && version != "6" && version != "7" {
					expected.CLIAllowance = nil
				}
				if version == "1" && channel == "cli" {
					expected = OperationWait{Paused: true}
				}
				if got == original || !reflect.DeepEqual(got, &expected) {
					t.Fatal("projection changed boundary", version, got)
				}
				after, _ := json.Marshal(original)
				if string(before) != string(after) {
					t.Fatal("projection mutated retained facts")
				}
			})
		}
	}
	if operationWaitForResponse(nil, "cli", "6") != nil {
		t.Fatal("absent facts invented")
	}
}
