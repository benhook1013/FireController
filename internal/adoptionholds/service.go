package adoptionholds

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
	"github.com/benhook1013/FireController/internal/core"
	"time"
)

type Service struct{ store *core.Store }

func New(store *core.Store) *Service { return &Service{store: store} }
func administrative(scope core.Scope) error {
	if !scope.Administrative() || scope.WorkerID == "" {
		return core.Fail("forbidden", "Administrative worker permission is required.", 403)
	}
	return nil
}
func preview(store *core.Store, scope core.Scope, b m.Batch, body []byte, newCount int) m.Preview {
	binding := encode(struct {
		Domain, Instance, Project, Principal, Worker string
		Batch                                        json.RawMessage
	}{"firecontroller.adoption-obligations.preview.v1", store.InstanceID(), scope.ProjectID, scope.PrincipalID, scope.WorkerID, body})
	return m.Preview{ProjectID: scope.ProjectID, SourceRevision: *b.ExpectedRevision, Selected: len(b.Obligations), New: newCount, Unchanged: len(b.Obligations) - newCount, PayloadSHA256: digest(body), PreviewSHA256: digest(binding)}
}
func admit(ctx context.Context, tx *sql.Tx, scope core.Scope, b m.Batch) ([]m.Obligation, error) {
	var repository int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repository); err != nil {
		return nil, err
	}
	if repository != b.Source.RepositoryID {
		return nil, conflict()
	}
	retained, err := ReadSource(ctx, tx, scope.ProjectID, b.Source.Namespace)
	if err != nil {
		var failure *core.Error
		if !errors.As(err, &failure) || failure.Code != "not_found" {
			return nil, err
		}
		if *b.ExpectedRevision != 0 {
			return nil, conflict()
		}
	} else if retained.Source != b.Source || retained.Revision != *b.ExpectedRevision {
		return nil, conflict()
	}
	added := []m.Obligation{}
	for _, obligation := range b.Obligations {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM review_adoption_obligations WHERE project_id=? AND namespace=? AND source_key=?`, scope.ProjectID, b.Source.Namespace, obligation.SourceKey).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			added = append(added, obligation)
			continue
		}
		if err != nil {
			return nil, err
		}
		existing, err := readDetail(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if string(encode(existing.Original)) != string(encode(obligation)) {
			return nil, conflict()
		}
	}
	return added, nil
}
func (s *Service) Preview(ctx context.Context, scope core.Scope, input m.Batch) (m.Preview, error) {
	if err := administrative(scope); err != nil {
		return m.Preview{}, err
	}
	b, body, err := NormalizeBatch(input)
	if err != nil {
		return m.Preview{}, err
	}
	var added []m.Obligation
	err = s.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		added, err = admit(ctx, tx, scope, b)
		return err
	})
	if err != nil {
		return m.Preview{}, err
	}
	return preview(s.store, scope, b, body, len(added)), nil
}
func (s *Service) Apply(ctx context.Context, scope core.Scope, key string, input m.ApplyRequest) (json.RawMessage, error) {
	if err := administrative(scope); err != nil {
		return nil, err
	}
	b, body, err := NormalizeBatch(input.Batch)
	if err != nil {
		return nil, err
	}
	p := preview(s.store, scope, b, body, 0)
	if input.PreviewSHA256 != p.PreviewSHA256 {
		return nil, core.Fail("import_preview_mismatch", "The batch or destination differs from its preview.", 409)
	}
	input.Batch = b
	return s.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.adoption_obligations.apply", Target: b.Source.Namespace, Key: key, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		added, err := admit(ctx, tx, scope, b)
		if err != nil {
			return nil, err
		}
		revision := *b.ExpectedRevision
		if len(added) == 0 {
			return m.Receipt{Outcome: "unchanged", Revision: revision, Unchanged: len(b.Obligations)}, nil
		}
		revision++
		at := time.Now().UTC().Format(time.RFC3339Nano)
		if revision == 1 {
			_, err = tx.ExecContext(ctx, `INSERT INTO review_adoption_sources(project_id,namespace,repository_id,snapshot_sha256,revision) VALUES(?,?,?,?,?)`, scope.ProjectID, b.Source.Namespace, b.Source.RepositoryID, b.Source.SnapshotSHA256, revision)
		} else {
			var result sql.Result
			result, err = tx.ExecContext(ctx, `UPDATE review_adoption_sources SET revision=? WHERE project_id=? AND namespace=? AND revision=?`, revision, scope.ProjectID, b.Source.Namespace, revision-1)
			if err == nil {
				count, e := result.RowsAffected()
				if e != nil {
					return nil, e
				}
				if count != 1 {
					return nil, conflict()
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_adoption_imports(project_id,namespace,revision,batch_json,batch_sha256,actor_id,worker_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, b.Source.Namespace, revision, string(body), digest(body), scope.PrincipalID, scope.WorkerID, at); err != nil {
			return nil, err
		}
		target, err := tx.PrepareContext(ctx, `INSERT INTO review_adoption_targets(obligation_id,project_id,pull_number) VALUES(?,?,?)`)
		if err != nil {
			return nil, err
		}
		defer target.Close()
		for _, item := range added {
			id := core.NewID("adoption_obligation")
			original := encode(item)
			if _, err = tx.ExecContext(ctx, `INSERT INTO review_adoption_obligations(id,project_id,namespace,source_key,source_revision,original_json,original_sha256,status,revision,actor_id,worker_id,created_at) VALUES(?,?,?,?,?,?,?,'open',1,?,?,?)`, id, scope.ProjectID, b.Source.Namespace, item.SourceKey, revision, string(original), digest(original), scope.PrincipalID, scope.WorkerID, at); err != nil {
				return nil, err
			}
			for _, number := range item.AffectedPullNumbers {
				if _, err = target.ExecContext(ctx, id, scope.ProjectID, number); err != nil {
					return nil, err
				}
			}
		}
		if err = core.AppendEvent(ctx, tx, scope, "reviews.adoption_obligations_imported", b.Source.Namespace, revision, map[string]any{"new": len(added), "unchanged": len(b.Obligations) - len(added)}); err != nil {
			return nil, err
		}
		return m.Receipt{Outcome: "imported", Revision: revision, New: len(added), Unchanged: len(b.Obligations) - len(added)}, nil
	})
}
func (s *Service) Resolve(ctx context.Context, scope core.Scope, id, key string, input m.ResolveRequest) (json.RawMessage, error) {
	if err := administrative(scope); err != nil {
		return nil, err
	}
	if !identifier.MatchString(id) {
		return nil, invalid()
	}
	r, err := normalizeResolution(input)
	if err != nil {
		return nil, err
	}
	return s.store.Mutate(ctx, scope, core.Mutation{Operation: "reviews.adoption_obligations.resolve", Target: id, Key: key, Request: r}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		current, err := readDetail(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if current.Revision != *r.ExpectedRevision || current.Status != "open" {
			return nil, conflict()
		}
		at := time.Now().UTC().Format(time.RFC3339Nano)
		result, err := tx.ExecContext(ctx, `UPDATE review_adoption_obligations SET status='resolved',revision=2 WHERE project_id=? AND id=? AND status='open' AND revision=?`, scope.ProjectID, id, *r.ExpectedRevision)
		if err != nil {
			return nil, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if count != 1 {
			return nil, conflict()
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO review_adoption_resolutions(obligation_id,revision,reason,evidence_json,actor_id,worker_id,resolved_at) VALUES(?,2,?,?,?,?,?)`, id, r.Reason, string(encode(r.Evidence)), scope.PrincipalID, scope.WorkerID, at); err != nil {
			return nil, err
		}
		if err = core.AppendEvent(ctx, tx, scope, "reviews.adoption_obligation_resolved", id, 2, struct{}{}); err != nil {
			return nil, err
		}
		return struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Revision int64  `json:"revision"`
		}{id, "resolved", 2}, nil
	})
}
