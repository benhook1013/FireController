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

func TestDraftFrontSelectionSkipsOnlyIndependentDraft(t *testing.T) {
	for _, mode := range []string{"independent", "stacked"} {
		t.Run(mode, func(t *testing.T) {
			f := selectionFixture(t)
			if mode == "stacked" {
				one := int64(1)
				if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stacked-draft", Configuration{ExpectedRevision: &one, CapabilityID: "test-capability", Mode: mode, Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
					t.Fatal(err)
				}
			}
			draft := true
			ready := false
			parent := reviewprovider.Pull{Number: 2, RepositoryID: 801, State: "open", IsDraft: &draft, HeadSHA: strings.Repeat("a", 40), HeadRef: "draft-parent", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
			child := reviewprovider.Pull{Number: 4, RepositoryID: 801, State: "open", IsDraft: &ready, HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}
			if mode == "stacked" {
				child.BaseRef, child.BaseSHA = parent.HeadRef, parent.HeadSHA
			}
			reader := &refreshTestReader{parents: map[string]reviewprovider.Pull{parent.HeadRef: parent}, pull: func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
				if number == parent.Number {
					return parent, nil
				}
				return child, nil
			}}
			for _, channel := range []string{"cli", "hosted"} {
				receipt, _ := selectionEnqueue(t, f, "draft-before-ready-"+channel, channel, nil)
				selectionNext(t, f, reader)
				got := selectionRead(t, f, receipt.ID)
				if mode == "stacked" {
					if got.State != "held" || got.Reason != "pull_draft" || got.OperationID != "" {
						t.Fatal("Draft dependency became eligible", got)
					}
					continue
				}
				if got.State != "selected" || got.OperationID == "" {
					t.Fatal("unrelated Draft blocked the ready independent target", got)
				}
				operation, err := f.e.ReadOperation(t.Context(), selectionScope(t, f, attemptProject, f.writer, "read"), got.OperationID)
				if err != nil || operation.State != "queued" || operation.PullNumber != child.Number || operation.HeadSHA != child.HeadSHA || operation.Channel != channel || operation.AllowOutOfOrder {
					t.Fatal("selected child bypassed or changed the normal target binding", operation, err)
				}
			}
			for _, table := range []string{"review_execution_attempts", "review_execution_reservations", "review_execution_evidence", "review_round_completions"} {
				var count int
				if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("selection executed or credited a review", table, count, err)
				}
			}
		})
	}
}
