package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// SourceRefusal is original preparation evidence, never admission authority.
// A missing parent retains only the checked child and branch, not invented SHAs.
type SourceRefusal struct {
	PullNumber        int64  `json:"pull_number"`
	ParentPullNumber  int64  `json:"parent_pull_number,omitempty"`
	ObservedBaseSHA   string `json:"observed_base_sha,omitempty"`
	ExpectedParentSHA string `json:"expected_parent_sha,omitempty"`
	BaseRef           string `json:"base_ref,omitempty"`
	Cause             string `json:"cause,omitempty"`
}

func validSourceRefusal(v *SourceRefusal, state, reason string, selected int64) bool {
	if v == nil {
		return true
	}
	if state != "held" || v.PullNumber <= 0 || v.PullNumber > math.MaxInt32 {
		return false
	}
	switch reason {
	case "parent_moved", "predecessor_parent_moved":
		selectedBinding := v.PullNumber != selected
		if reason == "parent_moved" {
			selectedBinding = v.PullNumber == selected
		}
		return v.Cause == "" && v.BaseRef == "" && selectedBinding && v.ParentPullNumber > 0 && v.ParentPullNumber <= math.MaxInt32 && v.ParentPullNumber != v.PullNumber && sha.MatchString(v.ObservedBaseSHA) && sha.MatchString(v.ExpectedParentSHA) && v.ObservedBaseSHA != v.ExpectedParentSHA
	case "parent_unavailable":
		return v.Cause == "" && v.ParentPullNumber == 0 && v.ObservedBaseSHA == "" && v.ExpectedParentSHA == "" && validRefusalBranch(v.BaseRef) && len(sourceRefusalJSON(v)) <= 512
	case "predecessor_unfinished":
		return v.PullNumber != selected && v.ParentPullNumber == 0 && v.ObservedBaseSHA == "" && v.ExpectedParentSHA == "" && v.BaseRef == "" && (v.Cause == "not_in_queue" || v.Cause == "discovery_incomplete" || v.Cause == "incoming_route_pending")
	}
	return false
}
func validRefusalBranch(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}
func sourceRefusalReason(reason string) bool {
	return reason == "parent_moved" || reason == "predecessor_parent_moved" || reason == "parent_unavailable" || reason == "predecessor_unfinished"
}

func sourceRefusalFromError(err error, state, reason string, selected int64) *SourceRefusal {
	if state != "held" || !sourceRefusalReason(reason) {
		return nil
	}
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "review_held" {
		return nil
	}
	d, ok := failure.Details.(map[string]string)
	if !ok || d["reason"] != reason {
		return nil
	}
	pull, e1 := strconv.ParseInt(d["pull_number"], 10, 64)
	v := &SourceRefusal{PullNumber: pull}
	var e2 error
	if reason == "predecessor_unfinished" {
		v.Cause = d["cause"]
		if d["parent_pull_number"] != "" || d["observed_base_sha"] != "" || d["expected_parent_sha"] != "" || d["base_ref"] != "" || strconv.FormatInt(pull, 10) != d["pull_number"] {
			return nil
		}
	} else if d["cause"] != "" {
		return nil
	} else if reason == "parent_moved" || reason == "predecessor_parent_moved" {
		if d["base_ref"] != "" {
			return nil
		}
		v.ParentPullNumber, e2 = strconv.ParseInt(d["parent_pull_number"], 10, 64)
		v.ObservedBaseSHA, v.ExpectedParentSHA = d["observed_base_sha"], d["expected_parent_sha"]
		if reason == "parent_moved" && (strconv.FormatInt(pull, 10) != d["pull_number"] || strconv.FormatInt(v.ParentPullNumber, 10) != d["parent_pull_number"]) {
			return nil
		}
	} else {
		v.BaseRef = d["base_ref"]
		// Contradictory comparison fields do not become missing-parent evidence.
		if d["parent_pull_number"] != "" || d["observed_base_sha"] != "" || d["expected_parent_sha"] != "" {
			return nil
		}
	}
	if e1 != nil || e2 != nil || !validSourceRefusal(v, state, reason, selected) {
		return nil
	}
	return v
}

func readSourceRefusal(ctx context.Context, tx *sql.Tx, o Operation) (*SourceRefusal, error) {
	if o.State != "held" || !sourceRefusalReason(o.Reason) || o.AttemptID == "" {
		return nil, nil
	}
	var version string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_source_refusals'`).Scan(&version); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil || !supportedSourceRefusalVersion(version) {
		return nil, adjudicationStorageError()
	}
	var body string
	err := tx.QueryRowContext(ctx, `SELECT t.source_refusal_json FROM review_execution_transitions t JOIN review_execution_attempts a ON a.id=t.attempt_id AND a.revision=t.revision WHERE a.project_id=? AND a.id=? AND a.operation_id=? AND a.state='held' AND t.state='held' AND t.reason=?`, o.ProjectID, o.AttemptID, o.ID, o.Reason).Scan(&body)
	if err != nil {
		return nil, err
	}
	if body == "null" {
		return nil, nil
	}
	var v *SourceRefusal
	if len(body) > 512 || strictjson.Decode([]byte(body), &v) != nil || !validSourceRefusal(v, o.State, o.Reason, o.PullNumber) || !sourceRefusalVersionAllows(v, o.Reason, version) {
		return nil, adjudicationStorageError()
	}
	return v, nil
}

func sourceRefusalJSON(v *SourceRefusal) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ValidateSourceRefusalsSnapshot checks retained diagnostics without provider work.
func ValidateSourceRefusalsSnapshot(ctx context.Context, tx *sql.Tx) error {
	var version string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_source_refusals'`).Scan(&version); err != nil {
		return err
	}
	if !supportedSourceRefusalVersion(version) {
		return adjudicationStorageError()
	}
	rows, err := tx.QueryContext(ctx, `SELECT t.source_refusal_json,t.state,t.reason,o.pull_number FROM review_execution_transitions t JOIN review_execution_attempts a ON a.id=t.attempt_id JOIN review_operations o ON o.id=a.operation_id AND o.project_id=a.project_id WHERE t.source_refusal_json<>'null'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var body, state, reason string
		var selected int64
		if err := rows.Scan(&body, &state, &reason, &selected); err != nil {
			return err
		}
		var v *SourceRefusal
		if len(body) > 512 || strictjson.Decode([]byte(body), &v) != nil || v == nil || !validSourceRefusal(v, state, reason, selected) || !sourceRefusalVersionAllows(v, reason, version) {
			return adjudicationStorageError()
		}
	}
	return rows.Err()
}

func sourceRefusalRequested(value string) bool {
	return value == "true" || value == "v2" || value == "v3" || value == "v4"
}

// New variants are negotiated independently of the retained original facts.
func sourceRefusalForResponse(v *SourceRefusal, header, reason string) *SourceRefusal {
	if v != nil && (reason == "parent_moved" && header != "v4" || v.Cause != "" && header != "v3" && header != "v4" || v.BaseRef != "" && header != "v2" && header != "v3" && header != "v4") {
		return nil
	}
	return v
}

// The semantic format gates each retained union variant, independent of table
// shape. Older helpers must not claim to validate newer original facts.
func supportedSourceRefusalVersion(version string) bool {
	return version == "1" || version == "2" || version == "3" || version == "4"
}
func sourceRefusalVersionAllows(v *SourceRefusal, reason, version string) bool {
	if !supportedSourceRefusalVersion(version) {
		return false
	}
	if v == nil {
		return true
	}
	if reason == "parent_moved" {
		return version == "4"
	}
	if v.Cause != "" {
		return version == "3" || version == "4"
	}
	return v.BaseRef == "" || version != "1"
}
