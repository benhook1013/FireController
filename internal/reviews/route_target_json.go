package reviews

import (
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// Zero is explicit unassigned responsibility, never a missing receiver default.
func (r *FindingRoute) UnmarshalJSON(data []byte) error {
	type plain FindingRoute
	var decoded plain
	if err := strictjson.Decode(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var target *int64
	if raw, ok := fields["target_pull_number"]; !ok {
		return errors.New("route target must be explicit")
	} else if err := json.Unmarshal(raw, &target); err != nil {
		return err
	}
	if target == nil {
		return errors.New("route target must not be null")
	}
	*r = FindingRoute(decoded)
	return nil
}
