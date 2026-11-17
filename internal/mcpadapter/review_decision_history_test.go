package mcpadapter

import (
	"encoding/json"
	"testing"
)

func TestReviewDecisionHistoryArguments(t *testing.T) {
	args, err := reviewSchedulingArguments("review_selection_decision_history", json.RawMessage(`{"channel":"cli","before":42,"limit":20}`))
	if err != nil || args.decisionBefore != 42 || args.limit != 20 {
		t.Fatal(args, err)
	}
	for _, raw := range []string{`{"channel":"cli","limit":0}`, `{"channel":"cli","before":-1}`, `{"channel":"other"}`, `{"channel":"cli","limit":101}`, `{"channel":"cli","extra":1}`} {
		if _, err := reviewSchedulingArguments("review_selection_decision_history", json.RawMessage(raw)); err == nil {
			t.Fatal(raw)
		}
	}
}
