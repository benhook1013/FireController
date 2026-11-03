package client_test

import (
	"encoding/json"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

func TestReviewRoundDefaultsStrictPolicyInput(t *testing.T) {
	input := client.ConfigureReviewPolicyRequest{RequiredChannels: []string{"cli"}}
	body, _ := json.Marshal(input)
	if string(body) != `{"expected_revision":0,"required_channels":["cli"],"max_review_files":0}` {
		t.Fatal("default omission changed old mutation body", string(body))
	}
	for _, bounds := range []client.ReviewRoundBounds{{0, 1}, {2, 5}, {1000, 1000}} {
		input.RoundDefaults = &client.ReviewRoundDefaults{Hosted: bounds, CLI: bounds}
		if err := client.ValidateConfigureReviewPolicy(input); err != nil {
			t.Fatal("valid minimum/maximum rejected", bounds, err)
		}
	}
	for _, bounds := range []client.ReviewRoundBounds{{-1, 1}, {0, 0}, {0, 1001}, {2, 1}} {
		input.RoundDefaults = &client.ReviewRoundDefaults{Hosted: bounds, CLI: client.ReviewRoundBounds{Minimum: 0, Maximum: 1}}
		if client.ValidateConfigureReviewPolicy(input) == nil {
			t.Fatal("invalid minimum/maximum accepted", bounds)
		}
	}
	for _, raw := range []string{`null`, `{}`, `{"hosted":{"minimum":0,"maximum":1}}`, `{"hosted":{"maximum":1},"cli":{"minimum":0,"maximum":1}}`, `{"hosted":{"minimum":null,"maximum":1},"cli":{"minimum":0,"maximum":1}}`, `{"hosted":{"minimum":0,"maximum":1},"cli":null}`, `{"hosted":{"minimum":0,"maximum":1,"taper":1},"cli":{"minimum":0,"maximum":1}}`} {
		var decoded client.ConfigureReviewPolicyRequest
		if err := strictjson.Decode([]byte(`{"expected_revision":0,"required_channels":["cli"],"max_review_files":0,"round_defaults":`+raw+`}`), &decoded); err == nil {
			t.Fatal("malformed optional policy defaults accepted", raw)
		}
	}
}
