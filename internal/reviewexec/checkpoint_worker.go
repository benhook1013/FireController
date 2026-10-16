package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func checkpointURL(repo string, number, id int64) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d#issuecomment-%d", repo, number, id)
}
func checkpointPersistence() error {
	return core.Fail("checkpoint_publication_persistence_failed", "Checkpoint publication storage failed; retained intent must be recovered before further posting.", 503)
}

// StartCheckpointPublications recovers local interrupted states synchronously,
// then owns a single worker. Empty providers leave this component disabled.
func (e *Engine) StartCheckpointPublications(ctx context.Context, providers map[string]CheckpointProvider) (<-chan error, error) {
	if ctx == nil || e == nil || e.store == nil {
		return nil, invalid()
	}
	if len(providers) == 0 {
		return nil, nil
	}
	owned := map[string]CheckpointProvider{}
	for id, p := range providers {
		_, ok := e.capabilities[id]
		if !ok || p == nil || (reflect.ValueOf(p).Kind() == reflect.Ptr && reflect.ValueOf(p).IsNil()) {
			return nil, invalid()
		}
		owned[id] = p
	}
	if !e.checkpointRunning.CompareAndSwap(false, true) {
		return nil, core.Fail("review_supervisor_running", "A checkpoint worker is already running.", 409)
	}
	fail := func(err error) (<-chan error, error) { e.checkpointRunning.Store(false); return nil, err }
	if err := ctx.Err(); err != nil {
		return fail(invalid())
	}
	var ids []string
	err := retrySourceRefreshLocal(ctx, func() error {
		ids = nil
		return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM review_checkpoint_publications WHERE state IN ('preparing','posting','recovering') ORDER BY created_at,id LIMIT 65`)
			if err != nil {
				return err
			}
			type key struct{ project, id string }
			var keys []key
			for rows.Next() {
				var k key
				if err = rows.Scan(&k.project, &k.id); err != nil {
					rows.Close()
					return err
				}
				keys = append(keys, k)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(keys) > 64 {
				return checkpointStorage()
			}
			for _, k := range keys {
				r, err := readCheckpoint(ctx, tx, k.project, k.id)
				if err != nil {
					return err
				}
				previous := r.View.Revision
				if r.View.State == "preparing" {
					r.View.State = "queued"
				} else {
					r.View.State = "recover_queued"
				}
				r.View.Revision++
				r.View.UpdatedAt = sourceStamp()
				r.View.Reason = "interrupted"
				if err = persistCheckpoint(ctx, tx, r, previous); err != nil {
					return err
				}
				ids = append(ids, k.id)
			}
			return nil
		})
	})
	if err != nil {
		return fail(checkpointPersistence())
	}
	for _, id := range ids {
		e.notifyCheckpoint(id)
	}
	done := make(chan error, 1)
	go func() {
		defer e.checkpointRunning.Store(false)
		defer close(done)
		done <- e.checkpointLoop(ctx, owned)
	}()
	return done, nil
}
func (e *Engine) checkpointLoop(ctx context.Context, providers map[string]CheckpointProvider) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		worked, err := e.checkpointNext(ctx, providers)
		if err != nil {
			var failure *core.Error
			if ctx.Err() != nil && (errors.Is(err, ctx.Err()) || errors.As(err, &failure) && failure.Code == "overloaded") {
				return nil
			}
			return checkpointPersistence()
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-e.checkpointWake:
		case <-ticker.C:
		}
	}
}
func (e *Engine) checkpointNext(ctx context.Context, providers map[string]CheckpointProvider) (bool, error) {
	var project, id string
	err := retrySourceRefreshLocal(ctx, func() error {
		project, id = "", ""
		return e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
			err := tx.QueryRowContext(ctx, `SELECT project_id,id FROM review_checkpoint_publications WHERE state IN ('queued','recover_queued') ORDER BY created_at,id LIMIT 1`).Scan(&project, &id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		})
	})
	if err != nil || id == "" {
		return false, err
	}
	var r checkpointRecord
	claimed := false
	err = retrySourceRefreshLocal(ctx, func() error {
		r, claimed = checkpointRecord{}, false
		return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			r, err = readCheckpoint(ctx, tx, project, id)
			if err != nil {
				return err
			}
			if r.View.State != "queued" && r.View.State != "recover_queued" {
				return nil
			}
			prior := r.View.Revision
			if r.View.State == "queued" {
				r.View.State = "preparing"
			} else {
				r.View.State = "recovering"
			}
			r.View.Revision++
			r.View.UpdatedAt = sourceStamp()
			r.View.Reason = ""
			if err = persistCheckpoint(ctx, tx, r, prior); err != nil {
				return err
			}
			claimed = true
			return nil
		})
	})
	if err != nil || !claimed {
		return claimed, err
	}
	e.notifyCheckpoint(id)
	action, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cap, exists := e.capabilities[r.CapabilityID]
	provider := providers[r.CapabilityID]
	if !exists || provider == nil || cap.Fingerprint != r.Fingerprint || cap.Provider != r.Provider || cap.Account != r.Account {
		return true, e.finishCheckpoint(ctx, r, "held", "capability_changed", r.Unposted, nil)
	}
	if r.View.State == "recovering" {
		return true, e.observeCheckpoint(action, ctx, r, provider)
	}
	repo, err := provider.Repository(action, r.Payload.RepositoryID)
	if err != nil {
		return true, e.preparationFailed(ctx, r, "provider_unavailable")
	}
	if repo.ID != r.Payload.RepositoryID || !repository.MatchString(repo.FullName) {
		return true, e.preparationFailed(ctx, r, "repository_unproven")
	}
	actor, err := provider.CurrentActor(action)
	if err != nil || !validCheckpointActor(actor) {
		return true, e.preparationFailed(ctx, r, "provider_actor_unproven")
	}
	if ctx.Err() != nil {
		return true, nil
	}
	// Commit exact intent and current authorization immediately before the sole
	// POST. A failed or ambiguous local commit grants no posting opportunity.
	var posting checkpointRecord
	var entered, refusedAdmission bool
	err = retrySourceRefreshLocal(action, func() error {
		posting = checkpointRecord{}
		completed := false
		writeErr := e.store.BackgroundWrite(action, func(ctx context.Context, tx *sql.Tx) error {
			entered = true
			current, err := readCheckpoint(ctx, tx, project, id)
			if err != nil {
				return err
			}
			if current.View.Revision != r.View.Revision || current.View.State != "preparing" {
				return conflict()
			}
			allowed, err := actorAllowed(ctx, tx, project, r.ActorID, r.WorkerID)
			if err != nil {
				return err
			}
			if !allowed {
				return checkpointHold("requester_revoked")
			}
			binding, err := e.checkpointBinding(ctx, tx, project, r.Payload.RepositoryID)
			if err != nil {
				return err
			}
			if binding.ID != r.CapabilityID || binding.Fingerprint != r.Fingerprint || binding.Provider != r.Provider || binding.Account != r.Account {
				return checkpointHold("capability_changed")
			}
			_, _, payload, err := checkpointSource(ctx, tx, project, r.View.EvidenceID, id)
			if err != nil {
				return err
			}
			if encoded(payload) != encoded(r.Payload) {
				return checkpointHold("source_changed")
			}
			// Mutate only this transaction's copy. A rolled-back attempt must not
			// advance the retained claim or accumulate an intent revision.
			next := current
			prior := next.View.Revision
			next.View.Revision++
			next.View.State = "posting"
			next.View.UpdatedAt = sourceStamp()
			next.Unposted = false
			render, _ := reviewprovider.RenderCheckpoint(payload)
			next.Intent = &checkpointIntent{Revision: next.View.Revision, CapabilityID: next.CapabilityID, Fingerprint: next.Fingerprint, Provider: next.Provider, Account: next.Account, Repository: repo.FullName, Actor: actor, EarliestPosting: next.View.UpdatedAt, Body: render.Body}
			if _, err = tx.ExecContext(ctx, `INSERT INTO review_checkpoint_intents(publication_id,revision,body_json) VALUES(?,?,?)`, id, next.Intent.Revision, encoded(next.Intent)); err != nil {
				return err
			}
			if err := persistCheckpoint(ctx, tx, next, prior); err != nil {
				return err
			}
			posting = next
			completed = true
			return nil
		})
		var failure *core.Error
		overloaded := errors.As(writeErr, &failure) && failure.Code == "overloaded"
		if ctx.Err() != nil && (errors.Is(writeErr, ctx.Err()) || overloaded) {
			return ctx.Err() // Shutdown retains any intent for startup recovery.
		}
		// A completed callback may have reached Commit. Even an overloaded
		// result must not retry or become proof that no intent was committed.
		if writeErr != nil && completed {
			return checkpointPersistence()
		}
		if !entered && overloaded {
			refusedAdmission = true
		}
		return writeErr
	})
	if err != nil {
		var f *core.Error
		if errors.As(err, &f) && f.Code == "review_publication_held" {
			reason := "publication_guard_changed"
			if details, ok := f.Details.(map[string]string); ok && details["reason"] != "" {
				reason = details["reason"]
			}
			return true, e.preparationFailed(ctx, r, reason)
		}
		// Only refused admission, with no callback ever entered, proves the
		// expired action could not have written or committed a posting intent.
		if !entered && refusedAdmission && ctx.Err() == nil && action.Err() != nil && errors.Is(err, action.Err()) {
			return true, e.preparationFailed(ctx, r, "local_storage_overloaded")
		}
		return true, err
	}
	r = posting
	e.notifyCheckpoint(id)
	comment, err := provider.PostIssueComment(action, repo.FullName, r.Payload.PullNumber, r.Intent.Body)
	if ctx.Err() != nil {
		return true, nil
	} // Durable posting is recovered observationally.
	if err != nil {
		var refusal *reviewprovider.Error
		if errors.As(err, &refusal) && !refusal.OutcomeUnknown {
			return true, e.finishCheckpoint(ctx, r, "held", "provider_refused", true, nil)
		}
		return true, e.finishCheckpoint(ctx, r, "uncertain", "posting_outcome_unknown", false, nil)
	}
	if !validCheckpointComment(r, comment) {
		return true, e.finishCheckpoint(ctx, r, "uncertain", "provider_confirmation_invalid", false, nil)
	}
	r.ConfirmationRepository = repo.FullName
	return true, e.finishCheckpoint(ctx, r, "published", "", false, &comment)
}
func (e *Engine) preparationFailed(ctx context.Context, r checkpointRecord, reason string) error {
	if ctx.Err() != nil {
		return nil
	}
	return e.finishCheckpoint(ctx, r, "held", reason, true, nil)
}
func validCheckpointComment(r checkpointRecord, c reviewprovider.IssueComment) bool {
	if r.Intent == nil || c.ID <= 0 || c.Author.ID != r.Intent.Actor.ID || c.Author.Kind != r.Intent.Actor.Kind || !validCheckpointActor(c.Author) || c.CreatedAt.IsZero() || c.CreatedAt.Year() < 1 || c.CreatedAt.Year() > 9999 || c.UpdatedAt.IsZero() || c.UpdatedAt.Year() < 1 || c.UpdatedAt.Year() > 9999 || c.UpdatedAt.Before(c.CreatedAt) || c.CreatedAt.Location() != time.UTC || c.UpdatedAt.Location() != time.UTC {
		return false
	}

	match, err := reviewprovider.MatchCheckpoint(c.Body, r.Payload)
	return err == nil && match == reviewprovider.CheckpointMatch
}
func (e *Engine) observeCheckpoint(action, owner context.Context, r checkpointRecord, p CheckpointProvider) error {
	repo, err := p.Repository(action, r.Payload.RepositoryID)
	if err != nil || repo.ID != r.Payload.RepositoryID || !repository.MatchString(repo.FullName) {
		if owner.Err() != nil {
			return nil
		}
		return e.finishCheckpoint(owner, r, "uncertain", "recovery_unavailable", false, nil)
	}
	comments, err := p.CheckpointComments(action, repo.FullName, r.Payload.PullNumber, r.Intent.Actor)
	if owner.Err() != nil {
		return nil
	}
	if err != nil {
		return e.finishCheckpoint(owner, r, "uncertain", "recovery_unavailable", false, nil)
	}
	comment, found, err := reviewprovider.FindCheckpoint(r.Payload, r.Intent.Actor, comments)
	if err != nil {
		var failure *reviewprovider.Error
		if errors.As(err, &failure) && (failure.Code == "checkpoint_conflict" || failure.Code == "checkpoint_ambiguous") {
			return e.finishCheckpoint(owner, r, "held", failure.Code, false, nil)
		}
		return e.finishCheckpoint(owner, r, "uncertain", "recovery_unavailable", false, nil)
	}
	if !found {
		return e.finishCheckpoint(owner, r, "uncertain", "checkpoint_not_found", false, nil)
	}
	if !validCheckpointComment(r, comment) {
		return e.finishCheckpoint(owner, r, "held", "provider_confirmation_invalid", false, nil)
	}
	r.ConfirmationRepository = repo.FullName
	return e.finishCheckpoint(owner, r, "published", "", false, &comment)
}
func (e *Engine) finishCheckpoint(ctx context.Context, r checkpointRecord, state, reason string, unposted bool, comment *reviewprovider.IssueComment) error {
	prior := r.View.Revision
	// Keep the provider result and original claimed revision outside retries.
	err := retrySourceRefreshLocal(ctx, func() error {
		return e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			current, err := readCheckpoint(ctx, tx, r.View.ProjectID, r.View.ID)
			if err != nil {
				return err
			}
			if current.View.Revision != prior || current.View.State != r.View.State {
				return conflict()
			}
			next := current
			next.View.State, next.View.Reason, next.Unposted = state, reason, unposted
			next.View.Revision++
			next.View.UpdatedAt = sourceStamp()
			if comment != nil {
				next.ConfirmationRepository = r.ConfirmationRepository
				next.View.CommentID = comment.ID
				next.View.CommentURL = checkpointURL(r.ConfirmationRepository, r.Payload.PullNumber, comment.ID)
				next.View.PublishedAt = comment.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")
				next.View.ProviderAuthorID = comment.Author.ID
				next.View.ProviderAuthorKind = comment.Author.Kind
			}
			return persistCheckpoint(ctx, tx, next, prior)
		})
	})
	if err == nil {
		e.notifyCheckpoint(r.View.ID)
	}
	return err
}
