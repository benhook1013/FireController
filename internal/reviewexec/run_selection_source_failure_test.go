package reviewexec

import (
	"context"
	"errors"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestRunSelectionSourceFailureReasonsAreFinite(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{errRunSourceBudget, "source_budget_exhausted"},
		{context.DeadlineExceeded, "source_fetch_timeout"},
		{&reviewprovider.Error{Code: "provider_rate_limited", Message: "Private provider text"}, "source_rate_limited"},
		{&reviewprovider.Error{Code: "provider_unauthorized", Message: "Private provider text"}, "source_unauthorized"},
		{&reviewprovider.Error{Code: "provider_not_found", Message: "Private provider text"}, "source_not_found"},
		{&reviewprovider.Error{Code: "provider_invalid_response", Message: "Private provider text"}, "source_response_invalid"},
		{&reviewprovider.Error{Code: "provider_unavailable", Message: "Private provider text"}, "source_provider_unavailable"},
		{&reviewprovider.Error{Code: "Private_unknown_code", Message: "Private provider text"}, "source_unavailable"},
		{errors.New("Private raw error"), "source_unavailable"},
		{&core.Error{Code: "review_held", Details: map[string]string{"reason": "Private_unknown_detail"}}, "source_unavailable"},
		{sourceHold("parent_moved"), "parent_moved"},
	} {
		if got := runReason(test.err); got != test.want {
			t.Fatal("unsafe or inaccurate source classification", got, test.want)
		}
	}
	if !errors.Is(errRunSourceBudget, context.DeadlineExceeded) {
		t.Fatal("source budget lost its deadline identity")
	}
}
