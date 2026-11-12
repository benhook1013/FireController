package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// errAllowanceObservationAbsent marks only the missing top-level preflight
// record. It still wraps sql.ErrNoRows for older preparation/recovery paths
// that intentionally treat an absent observation as a normal no-op.
var errAllowanceObservationAbsent = fmt.Errorf("allowance observation absent: %w", sql.ErrNoRows)

// These private classifications describe unusable retained evidence. Keep the
// public storage error unchanged while separating it from actual lookup errors.
type storedUnavailableKind uint8

const (
	storedWaitingIntent storedUnavailableKind = iota + 1
	storedAllowanceObservation
	storedHostedQuery
	storedHostedObservation
	storedReviewAdmission
	storedConfigurationHistory
)

type storedUnavailableError struct {
	kind  storedUnavailableKind
	cause *core.Error
}

func (e *storedUnavailableError) Error() string { return e.cause.Error() }
func (e *storedUnavailableError) Unwrap() error { return e.cause }

func newStoredUnavailable(kind storedUnavailableKind, message string) error {
	return &storedUnavailableError{kind: kind, cause: &core.Error{Code: "storage_unavailable", Message: message, Status: 500}}
}

func isStoredUnavailable(err error, kind storedUnavailableKind) bool {
	var unavailable *storedUnavailableError
	return errors.As(err, &unavailable) && unavailable.kind == kind
}

// IsInvalidHostedPreparationEvidence identifies malformed retained source only.
// Database access and cancellation errors still require normal recovery.
func IsInvalidHostedPreparationEvidence(err error) bool {
	return isStoredUnavailable(err, storedHostedQuery) || isStoredUnavailable(err, storedHostedObservation)
}

func allowanceUnavailable() error {
	return newStoredUnavailable(storedAllowanceObservation, "Stored allowance observation is unavailable.")
}

func validAllowanceOwners(o Operation, a Attempt) bool {
	return hostedObservationID(o.ID, "review_operation_") && hostedObservationID(a.ID, "review_attempt_") && identifier.MatchString(o.ProjectID) && identifier.MatchString(o.ActorID) && (o.WorkerID == "" || identifier.MatchString(o.WorkerID)) && identifier.MatchString(a.Provider) && identifier.MatchString(a.Account) && o.ConfigurationRevision > 0 && o.PolicyRevision > 0 && o.Revision > 0 && a.Revision > 0 && o.AttemptID == a.ID && a.OperationID == o.ID && a.ProjectID == o.ProjectID && (o.Channel == "cli" || o.Channel == "hosted")
}

func readCLIAllowanceRecord(ctx context.Context, q EvidenceQuery, o Operation, a Attempt) (cliAllowanceRecord, error) {
	var record cliAllowanceRecord
	var project, attempt string
	var body sql.NullString
	err := q.QueryRowContext(ctx, `SELECT project_id,attempt_id,CASE WHEN typeof(body_json)='text' AND length(CAST(body_json AS BLOB))<=8192 THEN body_json END FROM review_cli_preflight WHERE attempt_id=?`, a.ID).Scan(&project, &attempt, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return record, errAllowanceObservationAbsent
	}
	if err != nil {
		return record, err
	}
	if !body.Valid {
		return record, allowanceUnavailable()
	}
	if o.AttemptID != a.ID {
		var err error
		o, err = historicalCLIExhaustionOperation(ctx, q, o, a)
		if err != nil {
			return record, allowanceDependencyError(err)
		}
	}
	if !validAllowanceOwners(o, a) {
		return record, allowanceUnavailable()
	}
	if strictjson.Decode([]byte(body.String), &record) != nil || encoded(record) != body.String || !validCLIAllowanceObservation(record.Observation) || project != o.ProjectID || attempt != a.ID || record.ProjectID != project || record.AttemptID != attempt || record.OperationID != o.ID || a.OperationID != o.ID || a.ProjectID != project || o.AttemptID != a.ID || o.Channel != "cli" || record.ActorID != o.ActorID || record.WorkerID != o.WorkerID || record.ConfigurationRevision != o.ConfigurationRevision || record.PolicyRevision != o.PolicyRevision || record.CapabilityFingerprint != o.CapabilityFingerprint || !hash.MatchString(record.CapabilityFingerprint) || record.Provider != a.Provider || record.Account != a.Account || !identifier.MatchString(record.Provider) || !identifier.MatchString(record.Account) || record.AttemptRevision != 1 || record.AttemptRevision > a.Revision || record.Observation.Anchor.PullNumber != o.PullNumber || record.Observation.Anchor.CandidateSHA != o.HeadSHA {
		return record, allowanceUnavailable()
	}
	var repo int64
	if err := q.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, o.ProjectID).Scan(&repo); err != nil {
		return record, allowanceDependencyError(err)
	}
	if record.Observation.Anchor.RepositoryID != repo {
		return record, allowanceUnavailable()
	}
	return record, nil
}

func readHostedAllowanceRecord(ctx context.Context, q EvidenceQuery, o Operation, a Attempt) (hostedAllowanceRecord, error) {
	var project, attempt string
	var body sql.NullString
	err := q.QueryRowContext(ctx, `SELECT project_id,attempt_id,CASE WHEN typeof(body_json)='text' AND length(CAST(body_json AS BLOB))<=16384 THEN body_json END FROM review_hosted_preflight WHERE attempt_id=?`, a.ID).Scan(&project, &attempt, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return hostedAllowanceRecord{}, errAllowanceObservationAbsent
	}
	if err != nil {
		return hostedAllowanceRecord{}, err
	}
	if !body.Valid {
		return hostedAllowanceRecord{}, allowanceUnavailable()
	}
	if o.AttemptID != a.ID {
		var err error
		o, err = historicalCooldownOperation(ctx, q, o, a)
		if err != nil {
			return hostedAllowanceRecord{}, allowanceDependencyError(err)
		}
	}
	if !validAllowanceOwners(o, a) {
		return hostedAllowanceRecord{}, allowanceUnavailable()
	}
	record, err := decodeHostedAllowance(body.String, project, attempt)
	if err != nil {
		return record, err
	}
	query, err := readHostedQuery(ctx, q, a, o)
	if err != nil {
		return record, allowanceDependencyError(err)
	}
	if project != o.ProjectID || attempt != a.ID || !hostedAllowanceBinding(record, a, o, query) {
		return record, allowanceUnavailable()
	}
	if record.Input.Observation.State == "cooldown_observed" {
		if err := validateCooldownReference(ctx, q, a, o, query, record.Input); err != nil {
			return record, err
		}
	}
	return record, nil
}

func allowanceDependencyError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return allowanceUnavailable()
	}
	var failure *core.Error
	if errors.As(err, &failure) && (failure.Code == "not_found" || isStoredUnavailable(err, storedHostedQuery)) {
		return allowanceUnavailable()
	}
	return err
}

func storedAllowanceUnavailable(err error) bool {
	return isStoredUnavailable(err, storedAllowanceObservation)
}

func validateCooldownReference(ctx context.Context, q EvidenceQuery, a Attempt, o Operation, query HostedAllowanceQuery, in HostedAllowanceInput) error {
	var id, project, attempt, operation, checksum string
	var body sql.NullString
	var position, size int64
	err := q.QueryRowContext(ctx, `SELECT id,project_id,attempt_id,operation_id,position,capture_sha256,capture_bytes,CASE WHEN typeof(body_json)='text' AND length(CAST(body_json AS BLOB))<=24576 THEN body_json END FROM review_hosted_observations WHERE attempt_id=? AND capture_sha256=?`, a.ID, in.CaptureSHA256).Scan(&id, &project, &attempt, &operation, &position, &checksum, &size, &body)
	if err != nil {
		return allowanceDependencyError(err)
	}
	if !body.Valid {
		return allowanceUnavailable()
	}
	observation, err := decodeHostedObservation(body.String, id, project, attempt, operation, position, checksum, size, a, o)
	if err != nil {
		// The optional capture is unusable; lookup failures above still retain
		// their database or context classification.
		return allowanceUnavailable()
	}
	source := observation.Input
	if project != o.ProjectID || attempt != a.ID || operation != o.ID || source.Kind != "allowance_query" || source.CaptureID != in.CaptureID || source.CaptureBytes != in.CaptureBytes || source.CaptureSHA256 != in.CaptureSHA256 || source.ObservedAt != in.ObservedAt || source.ExpectedAnchor != query.Anchor || source.Intent != query.Intent {
		return allowanceUnavailable()
	}
	return nil
}

// readHeldAllowance projects only original held observations, never current
// account capacity. All queries use the caller's authorized read transaction.
func readHeldAllowance(ctx context.Context, q EvidenceQuery, o Operation) (*AllowanceObservation, error) {
	if o.State != "held" || o.AttemptID == "" || !(o.Channel == "cli" && o.Reason == "allowance_exhausted" || o.Channel == "hosted" && (o.Reason == "hosted_allowance_cooldown" || o.Reason == "hosted_chat_cooldown")) {
		return nil, nil
	}
	a, err := readAttempt(ctx, q, o.ProjectID, o.AttemptID)
	if err != nil {
		return nil, err
	}
	if a.OperationID != o.ID || a.State != o.State {
		return nil, allowanceUnavailable()
	}
	if o.Channel == "cli" {
		record, err := readCLIAllowanceRecord(ctx, q, o, a)
		if errors.Is(err, errAllowanceObservationAbsent) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		in := record.Observation
		if in.Remaining != 0 {
			return nil, nil
		}
		return &AllowanceObservation{Kind: "cli_exhausted", ObservedAt: in.ObservedAt, Remaining: &in.Remaining, Limit: &in.Limit, FullCapacityInMS: &in.FullCapacityInMS, RollingWindowMS: &in.RollingWindowMS}, nil
	}
	record, err := readHostedAllowanceRecord(ctx, q, o, a)
	if errors.Is(err, errAllowanceObservationAbsent) {
		return nil, nil
	}
	if err != nil {
		if isStoredUnavailable(err, storedHostedObservation) {
			return nil, allowanceUnavailable()
		}
		return nil, err
	}
	if record.Input.Observation.State != "cooldown_observed" {
		return nil, nil
	}
	kind := "hosted_cooldown"
	if record.Input.Observation.Reason == reviewprovider.HostedChatCooldownReason {
		kind = "hosted_chat_cooldown"
	}
	return &AllowanceObservation{Kind: kind, ObservedAt: record.Input.ObservedAt, RetryNotBefore: record.Input.Observation.RetryNotBefore.Format("2006-01-02T15:04:05.999999999Z07:00")}, nil
}

// ValidateAllowanceSnapshot checks retained facts without applying ingestion
// freshness. It opens no connection, filesystem artifact or provider request.
func ValidateAllowanceSnapshot(ctx context.Context, q EvidenceQuery, executionVersion int) error {
	tables := []string{}
	if executionVersion >= 4 {
		tables = append(tables, "review_cli_preflight")
	}
	if executionVersion >= 6 {
		tables = append(tables, "review_hosted_preflight")
	}
	for _, table := range tables {
		rows, err := q.QueryContext(ctx, `SELECT project_id,attempt_id FROM `+table)
		if err != nil {
			return err
		}
		var owners [][2]string
		for rows.Next() {
			var owner [2]string
			if err := rows.Scan(&owner[0], &owner[1]); err != nil {
				rows.Close()
				return err
			}
			owners = append(owners, owner)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		// Finish the scan before further queries: snapshot readers can have a
		// single connection, while transaction readers share their connection.
		for _, owner := range owners {
			a, err := readAttempt(ctx, q, owner[0], owner[1])
			if err != nil {
				return err
			}
			o, err := readOperationBase(ctx, q, owner[0], a.OperationID)
			if err != nil {
				return err
			}
			if table == "review_cli_preflight" {
				_, err = readCLIAllowanceRecord(ctx, q, o, a)
			} else {
				_, err = readHostedAllowanceRecord(ctx, q, o, a)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
