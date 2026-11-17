package client

import (
	"encoding/json"
	"errors"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type ReviewPullDiff struct {
	Files     int64 `json:"files"`
	Additions int64 `json:"additions"`
	Deletions int64 `json:"deletions"`
}

func (d *ReviewPullDiff) UnmarshalJSON(raw []byte) error {
	type wire ReviewPullDiff
	var fields map[string]json.RawMessage
	var value wire
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 3 || fields["files"] == nil || fields["additions"] == nil || fields["deletions"] == nil {
		return errors.New("incomplete diff")
	}
	for _, name := range []string{"files", "additions", "deletions"} {
		if string(fields[name]) == "null" {
			return errors.New("null diff count")
		}
	}
	if err := strictjson.Decode(raw, &value); err != nil {
		return err
	}
	*d = ReviewPullDiff(value)
	return nil
}

func validReviewPullMetadata(p ReviewPullSummary) bool {
	if p.Diff != nil {
		for _, n := range []int64{p.Diff.Files, p.Diff.Additions, p.Diff.Deletions} {
			if n < 0 || n > 2147483647 {
				return false
			}
		}
	}
	return p.MergedAt == "" || p.State == "merged" && validTimestamp(p.MergedAt)
}
