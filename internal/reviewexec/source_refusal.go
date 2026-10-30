package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// SourceRefusal is original preparation evidence, never admission authority.
type SourceRefusal struct {
	PullNumber        int64  `json:"pull_number"`
	ParentPullNumber  int64  `json:"parent_pull_number"`
	ObservedBaseSHA   string `json:"observed_base_sha"`
	ExpectedParentSHA string `json:"expected_parent_sha"`
}

func validSourceRefusal(v *SourceRefusal, state, reason string, selected int64) bool {
	return v == nil || state == "held" && reason == "predecessor_parent_moved" && v.PullNumber > 0 && v.PullNumber <= math.MaxInt32 && v.PullNumber != selected && v.ParentPullNumber > 0 && v.ParentPullNumber <= math.MaxInt32 && v.ParentPullNumber != v.PullNumber && sha.MatchString(v.ObservedBaseSHA) && sha.MatchString(v.ExpectedParentSHA) && v.ObservedBaseSHA != v.ExpectedParentSHA
}

func sourceRefusalFromError(err error, state, reason string, selected int64) *SourceRefusal {
	if state != "held" || reason != "predecessor_parent_moved" {
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
	parent, e2 := strconv.ParseInt(d["parent_pull_number"], 10, 64)
	v := &SourceRefusal{pull, parent, d["observed_base_sha"], d["expected_parent_sha"]}
	if e1 != nil || e2 != nil || !validSourceRefusal(v, state, reason, selected) {
		return nil
	}
	return v
}

func readSourceRefusal(ctx context.Context, tx *sql.Tx, o Operation) (*SourceRefusal, error) {
	if o.State != "held" || o.Reason != "predecessor_parent_moved" || o.AttemptID == "" {
		return nil, nil
	}
	var version string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:review_source_refusals'`).Scan(&version); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil || version != "1" {
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
	if len(body) > 512 || strictjson.Decode([]byte(body), &v) != nil || !validSourceRefusal(v, o.State, o.Reason, o.PullNumber) {
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
		if len(body) > 512 || strictjson.Decode([]byte(body), &v) != nil || v == nil || !validSourceRefusal(v, state, reason, selected) {
			return adjudicationStorageError()
		}
	}
	return rows.Err()
}
