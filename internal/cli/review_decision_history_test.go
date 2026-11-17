package cli

import (
	"testing"
)

func TestReviewSchedulingHistoryArguments(t *testing.T) {
	in, ok := parseReviewScheduling([]string{"history", "--channel", "cli", "--before", "42", "--limit", "20"})
	if !ok || in.before != 42 || in.limit != 20 {
		t.Fatal(in, ok)
	}
	for _, args := range [][]string{{"history", "--channel", "cli", "--limit", "0"}, {"history", "--channel", "cli", "--before", "-1"}, {"history", "--channel", "other"}, {"history", "--channel", "cli", "--limit", "101"}} {
		if _, ok := parseReviewScheduling(args); ok {
			t.Fatal(args)
		}
	}
	h, ok := operationHelp("reviews scheduling history")
	if !ok || h.Documentation != "docs/review-scheduling.md" {
		t.Fatal(h, ok)
	}
}
