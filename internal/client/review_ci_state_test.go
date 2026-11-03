package client_test

import (
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	domain "github.com/benhook1013/FireController/internal/reviews"
)

func TestReviewCINotRequiredReadAndWriteContracts(t *testing.T) {
	f := newReviewClientFixture(t)
	f.importSnapshot(t, func(r *domain.ObservationRequest) {
		r.Queue[0].State = "open"
		r.Queue[0].CIState = "not_required"
	})
	pull, err := f.client.ReviewPull(t.Context(), 1, "")
	if err != nil || pull.Pull.CIState != "not_required" {
		t.Fatal(pull, err)
	}
	queue, err := f.client.ReviewQueue(t.Context(), client.PageOptions{})
	if err != nil || len(queue.Items) != 1 || queue.Items[0].CIState != "not_required" {
		t.Fatal(queue, err)
	}
	active, err := f.client.ReviewActiveQueue(t.Context(), client.PageOptions{})
	if err != nil || len(active.Items) != 1 || active.Items[0].CIState != "not_required" {
		t.Fatal(active, err)
	}
	for _, state := range []string{"not_required", "unknown", "optional", "NOT_REQUIRED"} {
		edit := []byte(strings.Replace(string(queueEditDocument(0)), `"ci_state":"unknown"`, `"ci_state":"`+state+`"`, 1))
		transfer := []byte(strings.ReplaceAll(string(transferDocument()), `"ci_state":"unknown"`, `"ci_state":"`+state+`"`))
		want := state == "not_required" || state == "unknown"
		if err := client.ValidateReviewQueueEditDocument(edit); (err == nil) != want {
			t.Fatal("queue edit enum", state, err)
		}
		if err := client.ValidateReviewTransferDocument(transfer); (err == nil) != want {
			t.Fatal("transfer enum", state, err)
		}
	}
}
