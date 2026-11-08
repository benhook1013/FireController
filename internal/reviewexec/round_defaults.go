package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type RoundBounds struct {
	Minimum int64 `json:"minimum"`
	Maximum int64 `json:"maximum"`
}

// RoundDefaults affect only the first installation of a PR/channel control.
type RoundDefaults struct {
	Hosted RoundBounds `json:"hosted"`
	CLI    RoundBounds `json:"cli"`
}

func (d *RoundDefaults) UnmarshalJSON(body []byte) error {
	type plain RoundDefaults
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
	*d = RoundDefaults(value)
	return nil
}

func validRoundDefaults(value *RoundDefaults) bool {
	if value == nil {
		return true
	}
	for _, bounds := range []RoundBounds{value.Hosted, value.CLI} {
		if bounds.Minimum < 0 || bounds.Maximum < 1 || bounds.Maximum > 1000 || bounds.Minimum > bounds.Maximum {
			return false
		}
	}
	return true
}

func enrollmentRoundDefaults(ctx context.Context, tx *sql.Tx, project string) (RoundDefaults, error) {
	defaults := RoundDefaults{Hosted: RoundBounds{Minimum: 2, Maximum: 3}, CLI: RoundBounds{Minimum: 2, Maximum: 6}}
	policy, err := readPolicy(ctx, tx, project)
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && failure.Code == "reviews_policy_unconfigured" {
			return defaults, nil
		}
		return RoundDefaults{}, err
	}
	if policy.Policy.RoundDefaults != nil {
		defaults = *policy.Policy.RoundDefaults
	}
	return defaults, nil
}
