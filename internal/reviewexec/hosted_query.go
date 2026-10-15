package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const hostedQueryLimit = 16 << 10

// HostedAllowanceQuery is the durable intent for one fixed, non-review metadata
// query. An existing record never grants another post or review admission.
type HostedAllowanceQuery struct {
	ID                    string                       `json:"id"`
	ProjectID             string                       `json:"project_id"`
	AttemptID             string                       `json:"attempt_id"`
	OperationID           string                       `json:"operation_id"`
	Anchor                Anchor                       `json:"anchor"`
	Intent                string                       `json:"intent"`
	CapabilityFingerprint string                       `json:"capability_fingerprint"`
	PostingActor          reviewprovider.Author        `json:"posting_actor"`
	ProviderActor         reviewprovider.Author        `json:"provider_actor"`
	EarliestPosting       string                       `json:"earliest_posting"`
	Revision              int64                        `json:"revision"`
	Comment               *reviewprovider.IssueComment `json:"comment,omitempty"`
}

func hostedQueryUnavailable() error {
	return core.Fail("storage_unavailable", "Stored hosted allowance query is unavailable.", 500)
}
func hostedQueryIntent(a Attempt, o Operation) (reviewprovider.HostedTriggerIntent, string, error) {
	intent := reviewprovider.HostedTriggerIntent{OperationID: o.ID, AttemptID: a.ID, HeadSHA: o.HeadSHA}
	body, err := reviewprovider.HostedAllowanceQueryBody(intent)
	return intent, body, err
}
func validHostedQueryActors(intent reviewprovider.HostedTriggerIntent, posting, provider reviewprovider.Author, at time.Time) bool {
	_, err := reviewprovider.ResolveHostedAllowance(reviewprovider.HostedAllowanceSnapshot{Complete: false, ObservedAt: at}, intent, posting, provider, at)
	return err == nil
}
func validHostedQueryComment(query HostedAllowanceQuery, comment reviewprovider.IssueComment) bool {
	earliest, valid := hostedObservationTime(query.EarliestPosting)
	intent := reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: query.AttemptID, HeadSHA: query.Anchor.CandidateSHA}
	return valid && comment.ID > 0 && comment.Body == query.Intent && comment.Author.ID == query.PostingActor.ID && comment.Author.Kind == query.PostingActor.Kind &&
		validHostedQueryActors(intent, comment.Author, query.ProviderActor, earliest) && comment.CreatedAt.Location() == time.UTC && comment.UpdatedAt.Location() == time.UTC &&
		!comment.CreatedAt.IsZero() && comment.CreatedAt.Year() >= 1 && comment.CreatedAt.Year() <= 9999 && comment.UpdatedAt.Equal(comment.CreatedAt) &&
		!comment.CreatedAt.Before(earliest.Truncate(time.Second)) && !comment.CreatedAt.After(time.Now().Add(5*time.Second))
}

// readHostedQuery validates indexed facts against canonical owned JSON and the
// original operation. Later review admission may have a separately refreshed
// anchor; it cannot rewrite this earlier query's source observation.
func readHostedQuery(ctx context.Context, tx *sql.Tx, a Attempt, o Operation) (HostedAllowanceQuery, error) {
	var record HostedAllowanceQuery
	var id, project, attempt, operation, fingerprint, body string
	var revision int64
	err := tx.QueryRowContext(ctx, `SELECT id,project_id,attempt_id,operation_id,capability_fingerprint,revision,body_json FROM review_hosted_allowance_queries WHERE attempt_id=?`, a.ID).Scan(&id, &project, &attempt, &operation, &fingerprint, &revision, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return record, core.Fail("not_found", "The hosted allowance query is unavailable in this project.", 404)
	}
	if err != nil {
		return record, err
	}
	if len(body) > hostedQueryLimit || strictjson.Validate([]byte(body)) != nil || json.Unmarshal([]byte(body), &record) != nil || encoded(record) != body {
		return HostedAllowanceQuery{}, hostedQueryUnavailable()
	}
	intent, command, intentErr := hostedQueryIntent(a, o)
	earliest, validTime := hostedObservationTime(record.EarliestPosting)
	_, validObserved := hostedObservationTime(record.Anchor.ObservedAt)
	if intentErr != nil || !hostedObservationID(id, "review_hosted_query_") || id != record.ID || project != a.ProjectID || project != record.ProjectID || attempt != a.ID || attempt != record.AttemptID || operation != o.ID || operation != record.OperationID || o.ProjectID != project || a.OperationID != o.ID || o.AttemptID != a.ID || o.Channel != "hosted" ||
		revision != record.Revision || fingerprint != record.CapabilityFingerprint || !hash.MatchString(fingerprint) || fingerprint != o.CapabilityFingerprint || record.Intent != command || !validEvidenceAnchor(record.Anchor) || !validObserved || record.Anchor.PullNumber != o.PullNumber || record.Anchor.CandidateSHA != o.HeadSHA || !validTime || earliest.After(time.Now().Add(5*time.Second)) || !validHostedQueryActors(intent, record.PostingActor, record.ProviderActor, earliest) ||
		(record.Revision != 1 || record.Comment != nil) && (record.Revision != 2 || record.Comment == nil) || record.Comment != nil && !validHostedQueryComment(record, *record.Comment) {
		return HostedAllowanceQuery{}, hostedQueryUnavailable()
	}
	var repositoryID int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, project).Scan(&repositoryID); err != nil {
		return HostedAllowanceQuery{}, err
	}
	if repositoryID != record.Anchor.RepositoryID {
		return HostedAllowanceQuery{}, hostedQueryUnavailable()
	}
	return record, nil
}

// BeginHostedAllowanceQuery records the sole opportunity to post this metadata
// query. Only created=true after a successful commit grants that one opportunity;
// a lost response or an existing intent must be recovered, never blindly reposted.
func (e *Engine) BeginHostedAllowanceQuery(ctx context.Context, project, attemptID string, expectedAttemptRevision int64, anchor Anchor, postingActor, providerActor reviewprovider.Author) (HostedAllowanceQuery, bool, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") || expectedAttemptRevision < 1 || !validEvidenceAnchor(anchor) {
		return HostedAllowanceQuery{}, false, invalid()
	}
	observed, canonical := hostedObservationTime(anchor.ObservedAt)
	if !canonical {
		return HostedAllowanceQuery{}, false, invalid()
	}
	var result HostedAllowanceQuery
	created := false
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		intent, body, err := hostedQueryIntent(a, o)
		if err != nil || !validHostedQueryActors(intent, postingActor, providerActor, time.Now().UTC()) {
			return invalid()
		}
		prior, err := readHostedQuery(ctx, tx, a, o)
		if err == nil {
			if expectedAttemptRevision != 1 || prior.Anchor != anchor || prior.PostingActor != postingActor || prior.ProviderActor != providerActor {
				return conflict()
			}
			result = prior
			return nil
		}
		var missing *core.Error
		if !errors.As(err, &missing) || missing.Code != "not_found" {
			return err
		}
		if a.State != "preparing" || a.Revision != expectedAttemptRevision || a.Revision != 1 || a.Anchor != nil || a.Intent != "" || a.ExternalID != "" || o.State != "preparing" || o.Channel != "hosted" || o.AttemptID != a.ID || a.ProjectID != project || o.ProjectID != project || o.PullNumber != anchor.PullNumber || o.HeadSHA != anchor.CandidateSHA {
			return conflict()
		}
		current, err := e.currentExecutionContext(ctx, tx, o)
		if err != nil {
			return err
		}
		capability := e.capabilities[current.Configuration.CapabilityID]
		if a.Provider != capability.Provider || a.Account != capability.Account || current.RepositoryID != anchor.RepositoryID {
			return conflict()
		}
		now := time.Now().UTC()
		if observed.Before(now.Add(-time.Minute)) || observed.After(now.Add(5*time.Second)) {
			return invalid()
		}
		result = HostedAllowanceQuery{ID: core.NewID("review_hosted_query"), ProjectID: project, AttemptID: a.ID, OperationID: o.ID, Anchor: anchor, Intent: body, CapabilityFingerprint: o.CapabilityFingerprint, PostingActor: postingActor, ProviderActor: providerActor, EarliestPosting: now.Format(time.RFC3339Nano), Revision: 1}
		canonical, err := json.Marshal(result)
		if err != nil || len(canonical) > hostedQueryLimit {
			return invalid()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_hosted_allowance_queries(id,project_id,attempt_id,operation_id,capability_fingerprint,revision,body_json) VALUES(?,?,?,?,?,1,?)`, result.ID, project, a.ID, o.ID, o.CapabilityFingerprint, string(canonical)); err != nil {
			return err
		}
		if err = appendHostedQueryEvent(ctx, tx, o, result, "review.hosted_query_started"); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return HostedAllowanceQuery{}, false, err
	}
	return result, created, nil
}

// ReadHostedAllowanceQuery reads retained intent without evaluating current
// policy, access or installed capability; the result grants no new action.
func (e *Engine) ReadHostedAllowanceQuery(ctx context.Context, project, attemptID string) (HostedAllowanceQuery, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") {
		return HostedAllowanceQuery{}, invalid()
	}
	var result HostedAllowanceQuery
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		result, err = readHostedQuery(ctx, tx, a, o)
		return err
	})
	if err != nil {
		return HostedAllowanceQuery{}, err
	}
	return result, nil
}

// ConfirmHostedAllowanceQuery retains the exact posted or recovered query comment
// even after preparation stops. It authorizes neither another post nor a review.
func (e *Engine) ConfirmHostedAllowanceQuery(ctx context.Context, project, attemptID string, expectedQueryRevision int64, comment reviewprovider.IssueComment) (HostedAllowanceQuery, error) {
	if ctx == nil || e == nil || e.store == nil || !identifier.MatchString(project) || !hostedObservationID(attemptID, "review_attempt_") || expectedQueryRevision < 1 {
		return HostedAllowanceQuery{}, invalid()
	}
	var result HostedAllowanceQuery
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, err := readAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		o, err := readOperation(ctx, tx, project, a.OperationID)
		if err != nil {
			return err
		}
		prior, err := readHostedQuery(ctx, tx, a, o)
		if err != nil {
			return err
		}
		if expectedQueryRevision != 1 || !validHostedQueryComment(prior, comment) {
			return conflict()
		}
		if prior.Comment != nil {
			if *prior.Comment != comment {
				return conflict()
			}
			result = prior
			return nil
		}
		prior.Revision = 2
		owned := comment
		prior.Comment = &owned
		body, err := json.Marshal(prior)
		if err != nil || len(body) > hostedQueryLimit {
			return invalid()
		}
		changed, err := tx.ExecContext(ctx, `UPDATE review_hosted_allowance_queries SET revision=2,body_json=? WHERE id=? AND revision=1`, string(body), prior.ID)
		if err != nil {
			return err
		}
		n, err := changed.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return conflict()
		}
		if err = appendHostedQueryEvent(ctx, tx, o, prior, "review.hosted_query_posted"); err != nil {
			return err
		}
		result = prior
		return nil
	})
	if err != nil {
		return HostedAllowanceQuery{}, err
	}
	return result, nil
}
func appendHostedQueryEvent(ctx context.Context, tx *sql.Tx, o Operation, query HostedAllowanceQuery, event string) error {
	payload := struct {
		AttemptID   string `json:"attempt_id"`
		OperationID string `json:"operation_id"`
		CommentID   int64  `json:"comment_id,omitempty"`
	}{AttemptID: query.AttemptID, OperationID: query.OperationID}
	if query.Comment != nil {
		payload.CommentID = query.Comment.ID
	}
	return core.AppendEvent(ctx, tx, core.Scope{ProjectID: o.ProjectID, PrincipalID: o.ActorID, WorkerID: o.WorkerID}, event, query.ID, query.Revision, payload)
}
