package client

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestAgentPolicyExplainsChannelsWithoutChangingFacts(t *testing.T) {
	for _, tc := range []struct {
		name, reason, next string
		channel            ReviewPolicyAssessmentChannel
	}{
		{"rounds", "review_rounds_incomplete", "complete_authorized_review_rounds", ReviewPolicyAssessmentChannel{Channel: "cli", Covered: true, RoundsConfigured: true}},
		{"pending-dominates", "review_work_pending", "finish_retained_review_work", ReviewPolicyAssessmentChannel{Channel: "hosted", Pending: true, RoundsConfigured: true}},
		{"pending-with-coverage", "review_work_pending", "finish_retained_review_work", ReviewPolicyAssessmentChannel{Channel: "cli", Covered: true, Pending: true, RoundsConfigured: true, RoundsComplete: true}},
		{"coverage", "review_coverage_missing", "complete_authorized_review_work", ReviewPolicyAssessmentChannel{Channel: "cli", RoundsConfigured: true}},
		{"complete", "", "", ReviewPolicyAssessmentChannel{Channel: "hosted", Covered: true, RoundsConfigured: true, RoundsComplete: true}},
		{"without-round-controls", "", "", ReviewPolicyAssessmentChannel{Channel: "cli", Covered: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channels := []ReviewPolicyAssessmentChannel{tc.channel}
			captured := ReviewPolicyAssessment{ProjectID: "original_project", EvidenceID: "original_evidence", State: "held", Reason: "source_obligations_pending", SourceObligationsPending: 1, Channels: channels}
			source := ReviewSourcePolicyAssessment{ProjectID: "original_project", RefreshID: "original_refresh", State: "held", Reason: "source_obligations_pending", SourceObligationsPending: 1, Channels: channels}
			for _, pair := range []struct{ original, output any }{
				{captured, AgentReviewPolicy(captured)},
				{source, AgentReviewSourcePolicy(source)},
			} {
				body, err := json.Marshal(pair.output)
				if err != nil || len(body) > 2048 {
					t.Fatal("unbounded assessment", err)
				}
				var got struct {
					Channels []struct {
						RoundsComplete   *bool  `json:"rounds_complete"`
						RoundsConfigured *bool  `json:"rounds_configured"`
						Reason           string `json:"reason"`
						NextAction       string `json:"next_action"`
					} `json:"channels"`
				}
				if json.Unmarshal(body, &got) != nil || len(got.Channels) != 1 {
					t.Fatal("lost channel", string(body))
				}
				c := got.Channels[0]
				if c.RoundsComplete == nil || *c.RoundsComplete != tc.channel.RoundsComplete || c.RoundsConfigured == nil || *c.RoundsConfigured != tc.channel.RoundsConfigured || c.Reason != tc.reason || c.NextAction != tc.next {
					t.Fatal("missing or misleading channel explanation", string(body))
				}
				var actual, original map[string]any
				_ = json.Unmarshal(body, &actual)
				before, _ := json.Marshal(pair.original)
				_ = json.Unmarshal(before, &original)
				channel := actual["channels"].([]any)[0].(map[string]any)
				delete(channel, "reason")
				delete(channel, "next_action")
				originalChannel := original["channels"].([]any)[0].(map[string]any)
				originalChannel["rounds_configured"] = tc.channel.RoundsConfigured
				originalChannel["rounds_complete"] = tc.channel.RoundsComplete
				if !reflect.DeepEqual(actual, original) || channels[0] != tc.channel {
					t.Fatal("explanation changed policy facts or input")
				}
			}
		})
	}
}

func TestAgentPolicyKeepsEmptyChannelsAndSourceHold(t *testing.T) {
	for _, channels := range [][]ReviewPolicyAssessmentChannel{nil, {}} {
		value := ReviewSourcePolicyAssessment{State: "held", Reason: "source_configuration_changed", Channels: channels}
		out := AgentReviewSourcePolicy(value)
		if len(out.Channels) != 0 || (out.Channels == nil) != (channels == nil) || !reflect.DeepEqual(out.ReviewSourcePolicyAssessment, value) {
			t.Fatal("source hold changed or invented a channel", out)
		}
	}
}
