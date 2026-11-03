package historycollect

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run owns one background acquisition lane. Every completed page rejoins the
// oldest-work queue, so a large collection cannot retain the lane forever.
// The caller cancels ctx and joins the returned channel before closing storage.
func (e *Engine) Run(ctx context.Context) (<-chan error, error) {
	if ctx == nil || e.provider == nil {
		return nil, core.Fail("collection_unavailable", "No history reader is configured.", 409)
	}
	e.start.Lock()
	defer e.start.Unlock()
	if e.running {
		return nil, core.Fail("collection_unavailable", "The history worker is already running.", 409)
	}
	// Running rows may have observed an uncommitted page before interruption;
	// queued continuations may also outlive the provider pagination they follow.
	// Restart either unfinished pass at page one, preserving committed captures.
	if err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return recoverInterrupted(ctx, tx)
	}); err != nil {
		return nil, err
	}
	e.running = true
	done := make(chan error, 1)
	go func() {
		defer close(done)
		defer func() { e.start.Lock(); e.running = false; e.start.Unlock() }()
		retryDelay := 25 * time.Millisecond
		for ctx.Err() == nil {
			worked, err := e.advance(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				var failure *core.Error
				if errors.As(err, &failure) && failure.Code == "overloaded" {
					timer := time.NewTimer(retryDelay)
					select {
					case <-ctx.Done():
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						return
					case <-timer.C:
					}
					if retryDelay < 250*time.Millisecond {
						retryDelay *= 2
						if retryDelay > 250*time.Millisecond {
							retryDelay = 250 * time.Millisecond
						}
					}
					continue
				}
				done <- err
				return
			}
			retryDelay = 25 * time.Millisecond
			if worked {
				continue
			}
			select {
			case <-ctx.Done():
			case <-e.wake:
			}
		}
	}()
	return done, nil
}

type recoveryCandidate struct{ project, id string }

func recoverInterrupted(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT project_id,id FROM history_collections WHERE state='running' OR (state='queued' AND json_extract(position_json,'$.page')>1) ORDER BY project_id,id LIMIT 65`)
	if err != nil {
		return err
	}
	candidates := make([]recoveryCandidate, 0, 64)
	for rows.Next() {
		var candidate recoveryCandidate
		if err := rows.Scan(&candidate.project, &candidate.id); err != nil {
			rows.Close()
			return err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// The ordinary admission cap makes this recovery set finite. Refuse an
	// inconsistent larger set instead of growing startup memory without bound.
	if len(candidates) > 64 {
		return storageError()
	}
	for _, candidate := range candidates {
		r, err := readOperation(ctx, tx, candidate.project, candidate.id)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE history_collections SET state='queued',revision=revision+1,position_json=json_set(position_json,'$.page',1),pass_number=pass_number+1,reason='',updated_at=? WHERE project_id=? AND id=? AND revision=? AND (state='running' OR (state='queued' AND json_extract(position_json,'$.page')>1))`, stamp(), r.ProjectID, r.ID, r.Revision)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return conflict()
		}
		if err := core.AppendEvent(ctx, tx, core.Scope{ProjectID: r.ProjectID, PrincipalID: r.actor, WorkerID: r.worker}, "history.collection_changed", r.ID, r.Revision+1, struct{}{}); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) binding(ctx context.Context, tx *sql.Tx, r retainedOperation) (string, error) {
	var repositoryID int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, r.ProjectID).Scan(&repositoryID); err != nil {
		return "", err
	}
	if repositoryID != r.RepositoryID || r.fingerprint != e.fingerprint {
		return "configuration_changed", nil
	}
	err := checkActor(ctx, tx, core.Scope{ProjectID: r.ProjectID, PrincipalID: r.actor, WorkerID: r.worker})
	if err != nil {
		var failure *core.Error
		if errors.As(err, &failure) && (failure.Status == 400 || failure.Status == 403 || failure.Status == 404) {
			return "authorization_changed", nil
		}
	}
	return "", err
}

func transition(ctx context.Context, tx *sql.Tx, r retainedOperation, state, reason string) error {
	result, err := tx.ExecContext(ctx, `UPDATE history_collections SET state=?,reason=?,revision=revision+1,updated_at=? WHERE project_id=? AND id=? AND state=? AND revision=?`, state, reason, stamp(), r.ProjectID, r.ID, r.State, r.Revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return conflict()
	}
	return core.AppendEvent(ctx, tx, core.Scope{ProjectID: r.ProjectID, PrincipalID: r.actor, WorkerID: r.worker}, "history.collection_changed", r.ID, r.Revision+1, struct{}{})
}

func (e *Engine) advance(ctx context.Context) (bool, error) {
	var selected *retainedOperation
	var changedID string
	worked := false
	err := e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var project, id string
		err := tx.QueryRowContext(ctx, `SELECT project_id,id FROM history_collections WHERE state='queued' ORDER BY updated_at,id LIMIT 1`).Scan(&project, &id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r, err := readOperation(ctx, tx, project, id)
		if err != nil {
			return err
		}
		reason, err := e.binding(ctx, tx, r)
		if err != nil {
			return err
		}
		worked, changedID = true, r.ID
		if reason != "" {
			return transition(ctx, tx, r, "paused", reason)
		}
		if err := transition(ctx, tx, r, "running", ""); err != nil {
			return err
		}
		r.State, r.Revision = "running", r.Revision+1
		selected = &r
		return nil
	})
	if err == nil && changedID != "" {
		e.notify(changedID)
	}
	if err != nil || selected == nil {
		return worked, err
	}
	r := *selected
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	capture, readErr := Step(readCtx, e.provider, r.RepositoryID, r.Selection, r.Position)
	cancel()
	if ctx.Err() != nil {
		return true, nil
	}
	body, _ := json.Marshal(capture)
	ids, identityErr := captureIdentities(capture)
	if readErr == nil {
		readErr = identityErr
	}
	// Retry local writer contention with the already captured response. This
	// never repeats a provider GET merely because SQLite is temporarily busy.
	commitRetryDelay := 25 * time.Millisecond
	for {
		err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			current, err := readOperation(ctx, tx, r.ProjectID, r.ID)
			if err != nil {
				return err
			}
			if current.State != r.State || current.Revision != r.Revision {
				// Explicit cancellation/revision owns the result. Discard this
				// uncommitted response, preserving earlier immutable pages.
				return nil
			}
			reason, err := e.binding(ctx, tx, r)
			if err != nil {
				return err
			}
			if reason == "" && readErr != nil {
				reason = collectionReason(readErr)
			}
			if reason != "" {
				return transition(ctx, tx, current, "paused", reason)
			}
			return commitCapture(ctx, tx, current, capture, body, ids)
		})
		if ctx.Err() != nil {
			return true, nil
		}
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "overloaded" {
			if err == nil {
				e.notify(r.ID)
			}
			return true, err
		}
		timer := time.NewTimer(commitRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return true, nil
		case <-timer.C:
		}
		if commitRetryDelay < 250*time.Millisecond {
			commitRetryDelay *= 2
			if commitRetryDelay > 250*time.Millisecond {
				commitRetryDelay = 250 * time.Millisecond
			}
		}
	}
}

func collectionReason(err error) string {
	var provider *reviewprovider.Error
	if errors.As(err, &provider) {
		switch provider.Code {
		case "provider_rate_limited":
			return "rate_limited"
		case "provider_invalid_response":
			return "source_invalid"
		}
	}
	var failure *core.Error
	if errors.As(err, &failure) && failure.Code == "collection_source_changed" {
		return "source_changed"
	}
	if errors.As(err, &failure) && failure.Code == "collection_source_invalid" {
		return "source_invalid"
	}
	return "source_unavailable"
}

func captureIdentities(c Capture) ([]string, error) {
	ids, err := reviewprovider.ValidateHistoryFacts(c.Resource, c.Facts)
	if err != nil || len(ids) != c.Count {
		return nil, core.Fail("collection_source_invalid", "The collection page has invalid source facts.", 502)
	}
	return ids, nil
}

func commitCapture(ctx context.Context, tx *sql.Tx, r retainedOperation, c Capture, body []byte, ids []string) error {
	for _, id := range ids {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM history_collection_seen WHERE collection_id=? AND pass_number=? AND record_id=?)`, r.ID, r.PassNumber, id).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return transition(ctx, tx, r, "paused", "source_changed")
		}
	}
	pageID, at := core.NewID("history_page"), stamp()
	hash := sha256.Sum256(body)
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_collection_pages(id,project_id,collection_id,sequence,pass_number,reader_fingerprint,pull_number,resource,page_number,record_count,body_sha256,body_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, pageID, r.ProjectID, r.ID, r.PageCount+1, r.PassNumber, r.fingerprint, c.PullNumber, c.Resource, c.Page, c.Count, hex.EncodeToString(hash[:]), string(body), at); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_collection_seen(project_id,collection_id,pass_number,record_id,page_id) VALUES(?,?,?,?,?)`, r.ProjectID, r.ID, r.PassNumber, id, pageID); err != nil {
			return err
		}
	}
	state, position, pass := "completed", r.Position, r.PassNumber
	if c.Next != nil {
		state, position = "queued", *c.Next
		if position.PullIndex != r.Position.PullIndex || position.Resource != r.Position.Resource {
			pass++
		}
	}
	encodedPosition, _ := json.Marshal(position)
	if _, err := tx.ExecContext(ctx, `UPDATE history_collections SET position_json=?,pass_number=?,page_count=page_count+1,record_count=record_count+?,state=?,revision=revision+1,reason='',updated_at=? WHERE project_id=? AND id=? AND state='running' AND revision=?`, string(encodedPosition), pass, c.Count, state, at, r.ProjectID, r.ID, r.Revision); err != nil {
		return err
	}
	return core.AppendEvent(ctx, tx, core.Scope{ProjectID: r.ProjectID, PrincipalID: r.actor, WorkerID: r.worker}, "history.collection_changed", r.ID, r.Revision+1, struct{}{})
}
