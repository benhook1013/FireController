package client

import (
	"encoding/json"

	"github.com/benhook1013/FireController/internal/strictjson"
)

type ReviewRoundBounds struct {
	Minimum int64 `json:"minimum"`
	Maximum int64 `json:"maximum"`
}

// ReviewRoundDefaults affect only the first installation of a PR/channel control.
type ReviewRoundDefaults struct {
	Hosted ReviewRoundBounds `json:"hosted"`
	CLI    ReviewRoundBounds `json:"cli"`
}

func (d *ReviewRoundDefaults) UnmarshalJSON(body []byte) error {
	type plain ReviewRoundDefaults
	var value plain
	if strictjson.Decode(body, &value) != nil {
		return strictjson.ErrSyntax
	}
	var channels map[string]map[string]json.RawMessage
	if json.Unmarshal(body, &channels) != nil || len(channels) != 2 {
		return strictjson.ErrSyntax
	}
	for _, channel := range []string{"hosted", "cli"} {
		bounds := channels[channel]
		if len(bounds) != 2 || string(bounds["minimum"]) == "null" || len(bounds["minimum"]) == 0 || string(bounds["maximum"]) == "null" || len(bounds["maximum"]) == 0 {
			return strictjson.ErrSyntax
		}
	}
	*d = ReviewRoundDefaults(value)
	return nil
}

func validReviewRoundDefaults(value *ReviewRoundDefaults) bool {
	if value == nil {
		return true
	}
	for _, bounds := range []ReviewRoundBounds{value.Hosted, value.CLI} {
		if bounds.Minimum < 0 || bounds.Maximum < 1 || bounds.Maximum > 1000 || bounds.Minimum > bounds.Maximum {
			return false
		}
	}
	return true
}
