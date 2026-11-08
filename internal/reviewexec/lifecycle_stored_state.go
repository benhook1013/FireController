package reviewexec

import (
	"context"
	"database/sql"
	"errors"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type lifecycleConfigurationError struct {
	project string
	cause   error
}

func (e *lifecycleConfigurationError) Error() string { return e.cause.Error() }
func (e *lifecycleConfigurationError) Unwrap() error { return e.cause }

func decodeLifecycleConfiguration(project string, body any) (Configuration, error) {
	var c Configuration
	raw, ok := body.(string)
	if !ok || len(raw) > 8192 || strictjson.Decode([]byte(raw), &c) != nil || validateConfigurationShape(c) != nil {
		return c, &lifecycleConfigurationError{project, refreshStorageError()}
	}
	return c, nil
}

func readLifecycleConfiguration(ctx context.Context, tx *sql.Tx, project string) (Configuration, error) {
	var body any
	err := tx.QueryRowContext(ctx, `SELECT body_json FROM review_execution_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, project).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return Configuration{}, core.Fail("reviews_unconfigured", "Review execution is not configured for this project.", 409)
	}
	if err != nil {
		return Configuration{}, err
	}
	return decodeLifecycleConfiguration(project, body)
}

func lifecycleStoredStateCode(err error, project string, number int64) string {
	var configuration *lifecycleConfigurationError
	if errors.As(err, &configuration) && configuration.project == project {
		return "stored_configuration_invalid"
	}
	var metadata *reviews.SourceMetadataError
	if errors.As(err, &metadata) && metadata.ProjectID == project && metadata.PullNumber == number {
		return "stored_metadata_invalid"
	}
	return ""
}

func (s *lifecycleSchedule) holdStoredState(project string, number int64, lane lifecycleLane, wake lifecycleComparison, err error) {
	now := s.options.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := lifecycleKey{project, number}
	due := s.due[key]
	code := lifecycleStoredStateCode(err, project, number)
	if lane == lifecycleMetadata {
		due.checkedAt, due.lastErrorCode, due.next = now, code, now.Add(s.options.OpenInterval)
	} else {
		due.enrichmentNext, due.enrichmentError = now.Add(s.options.OpenInterval), code
		// Consume only the comparison that selected this failed claim. A later
		// metadata update must still wake enrichment independently of the hold.
		due.enrichmentHead, due.enrichmentBaseSHA, due.enrichmentBaseRef = wake.head, wake.baseSHA, wake.baseRef
	}
	s.due[key] = due
}
