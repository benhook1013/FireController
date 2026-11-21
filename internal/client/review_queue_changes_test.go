package client

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestQueueRestoreAndReorderValidationBeforeClientWork(t *testing.T) {
	anchor := int64(17)
	for _, test := range []struct {
		action string
		input  ReviewQueueChange
	}{
		{"restore", ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42}},
		{"restore", ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42, Before: &anchor}},
		{"restore", ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42, After: &anchor}},
		{"restore", ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42, Position: "first"}},
		{"reorder", ReviewQueueChange{ExpectedConfigurationRevision: 1, Order: []int64{42, math.MaxInt32}}},
	} {
		if err := ValidateReviewQueueChange(test.action, test.input); err != nil {
			t.Fatal(test, err)
		}
	}
	for _, action := range []string{"restore", "reorder"} {
		base := ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42}
		if action == "reorder" {
			base.PullNumber, base.Order = 0, []int64{42, 17}
		}
		var invalid []ReviewQueueChange
		for _, revision := range []int64{0, -1, math.MaxInt64} {
			in := base
			in.ExpectedConfigurationRevision = revision
			invalid = append(invalid, in)
		}
		in := base
		in.Reason = "superseded"
		invalid = append(invalid, in)
		if action == "restore" {
			for _, number := range []int64{0, -1, math.MaxInt32 + 1} {
				in = base
				in.PullNumber = number
				invalid = append(invalid, in)
			}
			in = base
			in.Order = []int64{}
			invalid = append(invalid, in)
			in = base
			in.Before, in.After = &anchor, &anchor
			invalid = append(invalid, in)
		} else {
			for _, order := range [][]int64{nil, {}, {42}, {42, 42}, {0, 42}, {-1, 42}, {math.MaxInt32 + 1, 42}, make([]int64, 101)} {
				in = base
				in.Order = order
				invalid = append(invalid, in)
			}
			in = base
			in.PullNumber = 42
			invalid = append(invalid, in)
			in = base
			in.Before = &anchor
			invalid = append(invalid, in)
			in = base
			in.After = &anchor
			invalid = append(invalid, in)
			in = base
			in.Position = "last"
			invalid = append(invalid, in)
		}
		for _, input := range invalid {
			// A nil receiver proves refusal before any credential or transport use.
			if _, err := (*Client)(nil).ChangeReviewQueue(t.Context(), action, "key", input); err == nil {
				t.Fatal("accepted invalid input", action, input)
			}
		}
	}
	order := make([]int64, 100)
	for i := range order {
		order[i] = int64(i + 1)
	}
	if err := ValidateReviewQueueChange("reorder", ReviewQueueChange{ExpectedConfigurationRevision: 1, Order: order}); err != nil {
		t.Fatal("100-entry order refused", err)
	}
}

func TestQueueChangeWireCompatibility(t *testing.T) {
	for _, test := range []struct {
		action string
		input  ReviewQueueChange
		want   string
	}{
		{"add", ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42}, `{"expected_configuration_revision":1,"pull_number":42}`},
		{"move", ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42, Position: "last"}, `{"expected_configuration_revision":1,"pull_number":42,"position":"last"}`},
		{"remove", ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42, Reason: "defunct"}, `{"expected_configuration_revision":1,"pull_number":42,"reason":"defunct"}`},
		{"restore", ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42}, `{"expected_configuration_revision":1,"pull_number":42}`},
		{"reorder", ReviewQueueChange{ExpectedConfigurationRevision: 1, Order: []int64{42, 17}}, `{"expected_configuration_revision":1,"order":[42,17]}`},
	} {
		body, err := json.Marshal(test.input)
		if err != nil || string(body) != test.want || ValidateReviewQueueChange(test.action, test.input) != nil {
			t.Fatal(test.action, string(body), err)
		}
	}
	for _, action := range []string{"add", "move", "remove"} {
		base := ReviewQueueChange{ExpectedConfigurationRevision: 1, PullNumber: 42}
		if action == "move" {
			base.Position = "last"
		} else if action == "remove" {
			base.Reason = "defunct"
		}
		if ValidateReviewQueueChange(action, base) != nil {
			t.Fatal("prior valid action regressed", action)
		}
		for _, order := range [][]int64{{}, {42, 17}} {
			in := base
			in.Order = order
			if ValidateReviewQueueChange(action, in) == nil {
				t.Fatal("prior action accepted order", action, order)
			}
		}
	}
}

func TestQueueMergedRefusalPreservesErrorContract(t *testing.T) {
	e := &Error{Code: "review_queue_merged", Message: "Original refusal", Action: "Original action", Status: 409, RequestID: "request_synthetic", Details: map[string]int64{"current_revision": 7}}
	if queueChangeError(e) != e || e.Code != "review_queue_merged" || e.Status != 409 || e.RequestID != "request_synthetic" || e.OutcomeUnknown || e.Details.(map[string]int64)["current_revision"] != 7 || !strings.Contains(e.Message, "Ordinary merged PRs") || !strings.Contains(e.Action, "reason=superseded") || !strings.Contains(e.Action, "preserves all history") {
		t.Fatal("merged guidance changed the refusal contract", e)
	}
	unknown := &Error{Code: "review_queue_merged", Message: "Preserve original request", Action: "Recover exact key", Status: 503, OutcomeUnknown: true}
	if queueChangeError(unknown) != unknown || unknown.Message != "Preserve original request" || unknown.Action != "Recover exact key" || !unknown.OutcomeUnknown {
		t.Fatal("guidance replaced unknown-write recovery", unknown)
	}
}
