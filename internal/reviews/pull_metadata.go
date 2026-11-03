package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// PullDiff is one atomic observed diff size. A nil pointer means unknown.
type PullDiff struct {
	Files     int64 `json:"files"`
	Additions int64 `json:"additions"`
	Deletions int64 `json:"deletions"`
}

func (d *PullDiff) UnmarshalJSON(raw []byte) error {
	type wire PullDiff
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
	*d = PullDiff(value)
	return nil
}

// PullDisplayMetadata contains provider display facts, never review authority.
type PullDisplayMetadata struct {
	Diff             *PullDiff `json:"diff,omitempty"`
	IsDraft          *bool     `json:"is_draft,omitempty"`
	MergedAt         string    `json:"merged_at,omitempty"`
	MergeStateStatus string    `json:"merge_state_status,omitempty"`
}

func (m PullDisplayMetadata) Validate(state string) error {
	if !oneOf(m.MergeStateStatus, "", "BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE") {
		return validationError("Merge state is unsupported.")
	}
	if m.Diff != nil {
		for _, n := range []int64{m.Diff.Files, m.Diff.Additions, m.Diff.Deletions} {
			if n < 0 || n > 2147483647 {
				return validationError("Diff counts must be nonnegative 32-bit integers.")
			}
		}
	}
	if m.MergedAt != "" {
		if state != "merged" {
			return validationError("merged_at belongs only to a merged pull request.")
		}
		if len(m.MergedAt) > 64 {
			return validationError("merged_at must be an RFC3339 instant.")
		}
		if _, err := time.Parse(time.RFC3339Nano, m.MergedAt); err != nil {
			return validationError("merged_at must be an RFC3339 instant.")
		}
	}
	return nil
}
func pullMetadata(p PullInput) PullDisplayMetadata {
	return PullDisplayMetadata{Diff: p.Diff, IsDraft: p.IsDraft, MergedAt: p.MergedAt, MergeStateStatus: p.MergeStateStatus}
}

// ReadPullMetadata rejects malformed, oversized or noncanonical retained metadata.
func ReadPullMetadata(raw, state string) (PullDisplayMetadata, error) {
	var m PullDisplayMetadata
	if len(raw) > 512 || strictjson.Decode([]byte(raw), &m) != nil || m.Validate(state) != nil || mustJSON(m) != raw {
		return m, historyInconsistent()
	}
	return m, nil
}
func inheritedPullMetadata(body []any, p PullInput) (string, error) {
	m := PullDisplayMetadata{}
	if len(body) > 12 && body[2] == p.HeadSHA {
		var err error
		raw, rawOK := body[12].(string)
		state, stateOK := body[1].(string)
		if !rawOK || !stateOK {
			return "", historyInconsistent()
		}
		m, err = ReadPullMetadata(raw, state)
		if err != nil {
			return "", err
		}
	}
	if p.Diff != nil {
		m.Diff = p.Diff
	}
	if len(body) < 5 || body[2] != p.HeadSHA || body[3] != p.BaseRef || body[4] != p.BaseSHA {
		m.MergeStateStatus = ""
	}
	if p.MergeStateStatus != "" && (p.MergeStateStatus != "UNKNOWN" || m.MergeStateStatus != "") {
		m.MergeStateStatus = p.MergeStateStatus
	}
	if p.IsDraft != nil {
		m.IsDraft = p.IsDraft
	}
	if p.MergedAt != "" {
		m.MergedAt = p.MergedAt
	}
	if p.State != "merged" {
		m.MergedAt = ""
	}
	if err := m.Validate(p.State); err != nil {
		return "", err
	}
	return mustJSON(m), nil
}

// ValidatePullMetadataHistory includes invisible historical versions, not just the current queue.
func ValidatePullMetadataHistory(ctx context.Context, q HistoryQuery) error {
	rows, err := q.QueryContext(ctx, `SELECT state,CASE WHEN length(CAST(metadata_json AS BLOB))<=512 THEN metadata_json ELSE NULL END FROM review_pull_versions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var state, raw string
		if rows.Scan(&state, &raw) != nil {
			return historyInconsistent()
		}
		if _, err := ReadPullMetadata(raw, state); err != nil {
			return err
		}
	}
	return rows.Err()
}
