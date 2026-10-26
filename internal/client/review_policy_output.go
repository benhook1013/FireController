package client

// Agent policy output explains existing channel holds without another read.
// It neither changes the assessment nor grants permission to request a review.
type ReviewPolicyChannelOutput struct {
	ReviewPolicyAssessmentChannel
	RoundsConfigured bool   `json:"rounds_configured"`
	RoundsComplete   bool   `json:"rounds_complete"`
	Reason           string `json:"reason,omitempty"`
	NextAction       string `json:"next_action,omitempty"`
}

type ReviewPolicyAssessmentOutput struct {
	ReviewPolicyAssessment
	Channels []ReviewPolicyChannelOutput `json:"channels"`
}

type ReviewSourcePolicyAssessmentOutput struct {
	ReviewSourcePolicyAssessment
	Channels []ReviewPolicyChannelOutput `json:"channels"`
}

func AgentReviewPolicy(value ReviewPolicyAssessment) ReviewPolicyAssessmentOutput {
	return ReviewPolicyAssessmentOutput{value, agentPolicyChannels(value.Channels)}
}

func AgentReviewSourcePolicy(value ReviewSourcePolicyAssessment) ReviewSourcePolicyAssessmentOutput {
	return ReviewSourcePolicyAssessmentOutput{value, agentPolicyChannels(value.Channels)}
}

func agentPolicyChannels(channels []ReviewPolicyAssessmentChannel) []ReviewPolicyChannelOutput {
	if channels == nil {
		return nil
	}
	out := make([]ReviewPolicyChannelOutput, 0, len(channels))
	for _, channel := range channels {
		item := ReviewPolicyChannelOutput{ReviewPolicyAssessmentChannel: channel, RoundsConfigured: channel.RoundsConfigured, RoundsComplete: channel.RoundsComplete}
		switch {
		case channel.Pending:
			item.Reason, item.NextAction = "review_work_pending", "finish_retained_review_work"
		case !channel.Covered:
			item.Reason, item.NextAction = "review_coverage_missing", "complete_authorized_review_work"
		case channel.RoundsConfigured && !channel.RoundsComplete:
			item.Reason, item.NextAction = "review_rounds_incomplete", "complete_authorized_review_rounds"
		}
		out = append(out, item)
	}
	return out
}
