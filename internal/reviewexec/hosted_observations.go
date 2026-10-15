package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const hostedObservationLimit = 200
const hostedObservationInputLimit = 16 << 10

// HostedObservationInput references immutable private source, never uploaded
// bodies or a filesystem path. Intent is the complete retained fixed command.
type HostedObservationInput struct {
	CaptureID      string `json:"capture_id"`
	CaptureSHA256  string `json:"capture_sha256"`
	CaptureBytes   int64  `json:"capture_bytes"`
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at"`
	ObservedAt     string `json:"observed_at"`
	ExpectedAnchor Anchor `json:"expected_anchor"`
	Intent         string `json:"intent"`
}

type HostedObservation struct {
	ID          string                 `json:"id"`
	ProjectID   string                 `json:"project_id"`
	AttemptID   string                 `json:"attempt_id"`
	OperationID string                 `json:"operation_id"`
	Position    int64                  `json:"position"`
	Input       HostedObservationInput `json:"input"`
	RecordedAt  string                 `json:"recorded_at"`
}

func hostedObservationID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+32 {
		return false
	}
	for _, r := range value[len(prefix):] {
		if r < '0' || r > '9' {
			if r < 'a' || r > 'f' {
				return false
			}
		}
	}
	return true
}

func hostedObservationTime(value string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, value)
	return t, err == nil && !t.IsZero() && value == t.UTC().Format(time.RFC3339Nano)
}

// Stored observations retain their validity across clock corrections; this
// validator checks immutable UTC timing, structure and identity fields only.
func validHostedObservationInput(in HostedObservationInput) bool {
	start, startOK := hostedObservationTime(in.StartedAt)
	finish, finishOK := hostedObservationTime(in.FinishedAt)
	observed, observedOK := hostedObservationTime(in.ObservedAt)
	return hash.MatchString(in.CaptureSHA256) && in.CaptureID == "review_capture_"+in.CaptureSHA256[:32] &&
		in.CaptureBytes > 0 && in.CaptureBytes <= 48<<20 && validEvidenceAnchor(in.ExpectedAnchor) &&
		safeText(in.Intent, 4096) && startOK && finishOK && observedOK && !observed.Before(start) && !finish.Before(observed)
}

func hostedObservationIdentity(a Attempt, o Operation, in HostedObservationInput) bool {
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: o.ID, AttemptID: a.ID, HeadSHA: o.HeadSHA})
	return err == nil && o.Channel == "hosted" && o.AttemptID == a.ID && a.OperationID == o.ID && a.ProjectID == o.ProjectID &&
		a.Anchor != nil && *a.Anchor == in.ExpectedAnchor && a.Anchor.PullNumber == o.PullNumber && a.Anchor.CandidateSHA == o.HeadSHA &&
		hash.MatchString(o.CapabilityFingerprint) && identifier.MatchString(a.Provider) && identifier.MatchString(a.Account) && in.Intent == intent && a.Intent == intent
}

func hostedObservationUnavailable() error {
	return core.Fail("storage_unavailable", "Stored hosted observation is unavailable.", 500)
}

// decodeHostedObservation checks the canonical body against every indexed fact;
// the bounded journal never trusts body JSON to override its scope or position.
func decodeHostedObservation(body, id, project, attempt, operation string, position int64, checksum string, bytes int64, a Attempt, o Operation) (HostedObservation, error) {
	var record HostedObservation
	if len(body) > hostedObservationInputLimit+2048 || json.Unmarshal([]byte(body), &record) != nil {
		return record, hostedObservationUnavailable()
	}
	canonical, err := json.Marshal(record)
	_, recordedOK := hostedObservationTime(record.RecordedAt)
	input, inputErr := json.Marshal(record.Input)
	if err != nil || string(canonical) != body || inputErr != nil || len(input) > hostedObservationInputLimit ||
		!hostedObservationID(record.ID, "review_hosted_observation_") || record.ID != id || record.ProjectID != project || record.AttemptID != attempt || record.OperationID != operation ||
		record.Position != position || position < 1 || position > hostedObservationLimit || record.Input.CaptureSHA256 != checksum || record.Input.CaptureBytes != bytes ||
		!recordedOK || !validHostedObservationInput(record.Input) || !hostedObservationIdentity(a, o, record.Input) {
		return HostedObservation{}, hostedObservationUnavailable()
	}
	return record, nil
}

// RecordHostedObservation retains a trusted private reference without changing
// attempt state, reserving work, interpreting findings or recording final credit.
func (e *Engine) RecordHostedObservation(ctx context.Context, project, attemptID string, input HostedObservationInput) (Receipt, error) {
	return e.recordHostedObservationAt(ctx, project, attemptID, input, time.Now())
}

func (e *Engine) recordHostedObservationAt(ctx context.Context, project, attemptID string, input HostedObservationInput, now time.Time) (Receipt, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") || !validHostedObservationInput(input) {
		return Receipt{}, invalid()
	}
	canonical, err := json.Marshal(input)
	if err != nil || len(canonical) > hostedObservationInputLimit {
		return Receipt{}, invalid()
	}
	var owned HostedObservationInput
	if json.Unmarshal(canonical, &owned) != nil {
		return Receipt{}, invalid()
	}
	var receipt Receipt
	err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		var id, rowProject, rowAttempt, rowOperation, body string
		var position, bytes int64
		err = tx.QueryRowContext(ctx, `SELECT id,project_id,attempt_id,operation_id,position,capture_bytes,body_json FROM review_hosted_observations WHERE attempt_id=? AND capture_sha256=?`, attemptID, owned.CaptureSHA256).Scan(&id, &rowProject, &rowAttempt, &rowOperation, &position, &bytes, &body)
		if err == nil {
			if rowProject != project || rowAttempt != attemptID || rowOperation != o.ID {
				return hostedObservationUnavailable()
			}
			prior, err := decodeHostedObservation(body, id, project, attemptID, o.ID, position, owned.CaptureSHA256, bytes, a, o)
			if err != nil {
				return err
			}
			if prior.Input != owned {
				return conflict()
			}
			receipt = Receipt{ID: id, State: "observed", Revision: 1}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// Fresh ingestion is clock-bounded; an exact retained replay above
		// remains available even after the recording host's clock moves back.
		finished, _ := hostedObservationTime(owned.FinishedAt)
		if finished.After(now.Add(5 * time.Second)) {
			return invalid()
		}
		if !hostedObservationIdentity(a, o, owned) || a.State != "posting" && a.State != "active" && a.State != "uncertain" {
			return conflict()
		}
		var repositoryID int64
		if err = tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
			return err
		}
		if repositoryID != a.Anchor.RepositoryID {
			return conflict()
		}
		// An indexed bounded scan detects both overflow and noncontiguous corruption;
		// exact retries above are still available when this finite journal is full.
		rows, err := tx.QueryContext(ctx, `SELECT position FROM review_hosted_observations WHERE project_id=? AND attempt_id=? ORDER BY position LIMIT 201`, project, attemptID)
		if err != nil {
			return err
		}
		count := int64(0)
		for rows.Next() {
			var prior int64
			if err = rows.Scan(&prior); err != nil {
				rows.Close()
				return err
			}
			count++
			if prior != count {
				rows.Close()
				return hostedObservationUnavailable()
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count >= hostedObservationLimit {
			return core.Fail("review_capacity", "The hosted observation reference limit is reached; retained observations remain available.", 409)
		}
		record := HostedObservation{ID: core.NewID("review_hosted_observation"), ProjectID: project, AttemptID: a.ID, OperationID: o.ID, Position: count + 1, Input: owned, RecordedAt: stamp()}
		bodyBytes, err := json.Marshal(record)
		if err != nil {
			return invalid()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_hosted_observations(id,project_id,attempt_id,operation_id,position,capture_sha256,capture_bytes,body_json) VALUES(?,?,?,?,?,?,?,?)`, record.ID, project, a.ID, o.ID, record.Position, owned.CaptureSHA256, owned.CaptureBytes, string(bodyBytes)); err != nil {
			return err
		}
		if err = core.AppendEvent(ctx, tx, core.Scope{PrincipalID: o.ActorID, ProjectID: project, WorkerID: o.WorkerID}, "review.capture_observed", record.ID, 1, struct {
			AttemptID   string `json:"attempt_id"`
			OperationID string `json:"operation_id"`
			CaptureID   string `json:"capture_id"`
		}{a.ID, o.ID, owned.CaptureID}); err != nil {
			return err
		}
		receipt = Receipt{ID: record.ID, State: "observed", Revision: 1}
		return nil
	})
	if err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// ReadHostedObservations is a bounded trusted recovery read, not a provider
// observation or public upload API. Retained references survive settlement.
func (e *Engine) ReadHostedObservations(ctx context.Context, project, attemptID string) ([]HostedObservation, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") {
		return nil, invalid()
	}
	records := []HostedObservation{}
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,project_id,attempt_id,operation_id,position,capture_sha256,capture_bytes,body_json FROM review_hosted_observations WHERE project_id=? AND attempt_id=? ORDER BY position LIMIT 201`, project, attemptID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, rowProject, rowAttempt, operation, checksum, body string
			var position, bytes int64
			if err = rows.Scan(&id, &rowProject, &rowAttempt, &operation, &position, &checksum, &bytes, &body); err != nil {
				return err
			}
			if len(records) >= hostedObservationLimit || rowProject != project || rowAttempt != attemptID || operation != o.ID || position != int64(len(records)+1) {
				return hostedObservationUnavailable()
			}
			record, err := decodeHostedObservation(body, id, project, attemptID, o.ID, position, checksum, bytes, a, o)
			if err != nil {
				return err
			}
			records = append(records, record)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}
