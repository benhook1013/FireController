package reviewexec

import (
	"context"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestDraftFrontSelectionHoldsWithoutChildOrAttempt(t *testing.T) {
	f := selectionFixture(t)
	draft := true
	reader := &refreshTestReader{pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", IsDraft: &draft, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
	}}
	for _, channel := range []string{"cli", "hosted"} {
		receipt, _ := selectionEnqueue(t, f, "draft-front-"+channel, channel, nil)
		selectionNext(t, f, reader)
		got := selectionRead(t, f, receipt.ID)
		if got.State != "held" || got.Reason != "pull_draft" || got.OperationID != "" {
			t.Fatal("draft front created review work or lost its reason", got)
		}
	}
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_reservations", "review_execution_evidence", "review_round_completions"} {
		var count int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("draft selection created native review effects", table, count, err)
		}
	}
}
