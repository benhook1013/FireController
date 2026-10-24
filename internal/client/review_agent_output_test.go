package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAgentRunOutputKeepsRunDatesAndCursor(t *testing.T) {
	v := ReviewRuns{ProjectID: "p", Revision: 7, SnapshotID: "s", Source: "old source", ObservedAt: "old origin", RecordedAt: "old save", Items: []ReviewRunSummary{{StartedAt: "new start", FinishedAt: "new finish"}}, Cursor: "next", HasMore: true}
	out := AgentReviewRuns(v)
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "old ") || !strings.Contains(string(b), "new finish") || out.Cursor != v.Cursor || !out.HasMore || !reflect.DeepEqual(out.Items, v.Items) {
		t.Fatal(string(b))
	}
}
func TestAgentCIDiagnosticPreservesRefusal(t *testing.T) {
	for _, reason := range []string{"ci_merge_identity_unproven", "ci_merge_pending", "other_reason"} {
		v := ReviewSourceCIAssessment{State: "unproven", Reason: reason}
		out := AgentReviewCI(v)
		if !reflect.DeepEqual(out.ReviewSourceCIAssessment, v) || (out.Diagnostic != nil) != (reason != "other_reason") {
			t.Fatal(out)
		}
	}
	if AgentReviewCI(ReviewSourceCIAssessment{State: "held", Reason: "ci_merge_pending"}).Diagnostic != nil {
		t.Fatal("misdiagnosed local hold")
	}
}
