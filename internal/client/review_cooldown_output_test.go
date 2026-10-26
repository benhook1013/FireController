package client

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAgentCooldownStatusExplainsOnlyKnownHostedHold(t *testing.T) {
	base := ReviewOperationStatus{ID: "original", Channel: "hosted", State: "held", Stage: "held", Reason: "hosted_allowance_cooldown", AttemptID: "original_attempt", Revision: 3, UpdatedAt: "original_time", NextAction: "read_diagnostics"}
	for _, change := range []func(*ReviewOperationStatus){nil,
		func(v *ReviewOperationStatus) { v.Channel = "cli" },
		func(v *ReviewOperationStatus) { v.State = "uncertain" },
		func(v *ReviewOperationStatus) { v.Reason = "hosted_allowance_unproven" },
	} {
		v := base
		if change != nil {
			change(&v)
		}
		before := v
		out := AgentReviewOperationStatus(v)
		want := v
		if change == nil {
			want.NextAction = "new_request_after_cooldown"
			if out.Diagnostic == nil || !strings.Contains(out.Diagnostic.Explanation, "no resume command") || !strings.Contains(out.Diagnostic.NextAction, "Normal admission") || !strings.Contains(out.Diagnostic.NextAction, "unknown outcome") {
				t.Fatal("missing retry identity or safety distinction", out)
			}
		} else if v.State != "held" || v.Channel != "hosted" {
			if out.Diagnostic != nil {
				t.Fatal("invented a proven cooldown", out)
			}
		}
		if out.ReviewOperationStatus != want || v != before {
			t.Fatal("retry hint changed retained facts or input", out)
		}
	}
}

func TestAgentReviewResultHintsPreserveReceiptsAndInvalidUnions(t *testing.T) {
	r := &ReviewRequestReceipt{ID: "original_receipt", State: "queued", Revision: 1}
	op := &ReviewOperation{ID: "original_operation", State: "held", Channel: "hosted", Reason: "hosted_allowance_cooldown"}
	s := &ReviewRunState{Selection: ReviewRunSelection{ID: "original_selection"}, Operation: op}
	for _, pair := range []struct{ original, output any }{
		{ReviewRequestResult{}, AgentReviewRequestResult(ReviewRequestResult{})},
		{ReviewRequestResult{Receipt: r}, AgentReviewRequestResult(ReviewRequestResult{Receipt: r})},
		{ReviewRequestResult{Receipt: r, Operation: op}, AgentReviewRequestResult(ReviewRequestResult{Receipt: r, Operation: op})},
		{ReviewRunResult{}, AgentReviewRunResult(ReviewRunResult{})},
		{ReviewRunResult{Receipt: r}, AgentReviewRunResult(ReviewRunResult{Receipt: r})},
		{ReviewRunResult{Receipt: r, Result: s}, AgentReviewRunResult(ReviewRunResult{Receipt: r, Result: s})},
	} {
		want, wantErr := json.Marshal(pair.original)
		got, gotErr := json.Marshal(pair.output)
		if (wantErr == nil) != (gotErr == nil) || string(want) != string(got) {
			t.Fatal("receipt expanded or invalid union accepted", string(got), gotErr)
		}
	}
	for _, pair := range []struct{ original, output any }{
		{*op, AgentReviewRequestResult(ReviewRequestResult{Operation: op})},
		{*s, AgentReviewRunResult(ReviewRunResult{Result: s})},
		{*s, AgentReviewSelection(*s)},
		{ReviewRunState{}, AgentReviewSelection(ReviewRunState{})},
	} {
		before, _ := json.Marshal(pair.original)
		after, err := json.Marshal(pair.output)
		if err != nil {
			t.Fatal(err)
		}
		var want, got map[string]any
		_ = json.Unmarshal(before, &want)
		_ = json.Unmarshal(after, &got)
		if nested, ok := got["operation"].(map[string]any); ok {
			delete(nested, "diagnostic")
		} else {
			delete(got, "diagnostic")
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatal("hint changed original envelope or operation facts", string(after))
		}
	}
}
