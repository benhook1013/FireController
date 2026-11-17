package cli

import (
	"strings"
	"testing"
)

func TestReviewHelpDistinguishesAcceptedRetryFromAnotherRound(t *testing.T) {
	for _, operation := range []string{"reviews start", "reviews request", "reviews operation"} {
		help, ok := operationHelp(operation)
		text := strings.ToLower(strings.Join(help.Semantics, " "))
		if !ok || !strings.Contains(text, "same") || !strings.Contains(text, "cli quota") || !strings.Contains(text, "automatic") {
			t.Fatal("help omits durable retry", operation, help)
		}
		if operation != "reviews operation" && !strings.Contains(text, "cli pr or round") {
			t.Fatal("help omits explicit intent boundary", operation, help)
		}
		if strings.Contains(text, "no automatic next review or retry") {
			t.Fatal("help contradicts durable retry", operation, help)
		}
	}
}
