// Package coordimport applies prepared native coordination history. Source
// schemas and conversion belong to optional tools, never to this package.
package coordimport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
)

type Batch = coordimportmodel.Batch
type ApplyRequest = coordimportmodel.ApplyRequest
type Counts = coordimportmodel.Counts
type Preview = coordimportmodel.Preview
type Mapping = coordimportmodel.Mapping
type Receipt = coordimportmodel.Receipt

func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "coordination_import", []string{
		`CREATE TABLE coordination_import_state(project_id TEXT PRIMARY KEY REFERENCES projects(id),revision INTEGER NOT NULL CHECK(revision>=1));`,
		`CREATE TABLE coordination_import_records(project_id TEXT NOT NULL,source TEXT NOT NULL,source_key TEXT NOT NULL,job_id TEXT NOT NULL,import_revision INTEGER NOT NULL,item_sha256 TEXT NOT NULL,source_sha256 TEXT NOT NULL,item_json TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,source,source_key),UNIQUE(project_id,job_id),FOREIGN KEY(project_id,job_id) REFERENCES jobs(project_id,id),FOREIGN KEY(project_id,import_revision) REFERENCES coordination_import_batches(project_id,revision) DEFERRABLE INITIALLY DEFERRED);`,
		`CREATE TABLE coordination_import_batches(project_id TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>=1),source TEXT NOT NULL,source_sha256 TEXT NOT NULL,payload_sha256 TEXT NOT NULL,actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(project_id,revision),FOREIGN KEY(project_id) REFERENCES projects(id));`,
	})
}

func Register(mux *http.ServeMux, store *core.Store) {
	mux.HandleFunc("GET /api/v1/projects/{project_id}/coordination/imports", current(store))
	mux.HandleFunc("POST /api/v1/projects/{project_id}/coordination/imports/preview", preview(store))
	mux.HandleFunc("POST /api/v1/projects/{project_id}/coordination/imports/apply", apply(store))
}

func invalid(message string) error { return core.Fail("validation_failed", message, 400) }
func conflict() error {
	return core.Fail("coordination_import_conflict", "The destination revision or a retained source mapping conflicts; inspect the destination and preview again.", 409)
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func validHash(s string) bool   { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }

// Normalize checks the eventual apply wrapper as well as individual records,
// so a valid preview cannot produce an oversized follow-up request.
func Normalize(batch Batch) (Batch, []byte, error) {
	if batch.Version != 1 || batch.RepositoryID <= 0 || batch.ExpectedRevision == nil || *batch.ExpectedRevision < 0 || *batch.ExpectedRevision == int64(^uint64(0)>>1) || batch.Source == "" || strings.TrimSpace(batch.Source) != batch.Source || len(batch.Source) > 200 || !utf8.ValidString(batch.Source) || strings.IndexFunc(batch.Source, unicode.IsControl) >= 0 || !validHash(batch.SourceSHA256) || len(batch.Jobs) < 1 || len(batch.Jobs) > 25 {
		return Batch{}, nil, invalid("Provide a version-one prepared batch, repository identity, expected revision, source and digest, and one to twenty-five selected jobs.")
	}
	batch.Jobs = append([]jobs.PreparedJob(nil), batch.Jobs...)
	keys := map[string]bool{}
	checkpoints := 0
	for i, item := range batch.Jobs {
		normalized, err := jobs.NormalizePreparedJob(item)
		if err != nil {
			return Batch{}, nil, err
		}
		if keys[normalized.SourceKey] {
			return Batch{}, nil, invalid("Prepared job source keys must be unique.")
		}
		keys[normalized.SourceKey] = true
		checkpoints += len(normalized.Checkpoints)
		if checkpoints > 100 {
			return Batch{}, nil, invalid("A prepared batch contains at most one hundred checkpoints.")
		}
		batch.Jobs[i] = normalized
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return Batch{}, nil, invalid("The prepared batch could not be encoded.")
	}
	wrapped, err := json.Marshal(ApplyRequest{PreviewSHA256: strings.Repeat("0", 64), Batch: batch})
	if err != nil || len(wrapped) > 256<<10 {
		return Batch{}, nil, invalid("The normalized apply request exceeds 256 KiB.")
	}
	return batch, body, nil
}

func revision(ctx context.Context, tx *sql.Tx, project string) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx, `SELECT revision FROM coordination_import_state WHERE project_id=?`, project).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func resourceID(project, source, kind, parent, key string) string {
	body, _ := json.Marshal([]string{project, source, kind, parent, key})
	return kind + "_" + digest(body)[:32]
}

func bindings(ctx context.Context, tx *sql.Tx, project string, item jobs.PreparedJob) error {
	if item.WorkerID != "" {
		if err := core.CheckWorker(ctx, tx, project, item.WorkerID); err != nil {
			return err
		}
	}
	for _, cp := range item.Checkpoints {
		if cp.WorkerID != "" {
			if err := core.CheckWorker(ctx, tx, project, cp.WorkerID); err != nil {
				return err
			}
		}
	}
	return nil
}

func admit(ctx context.Context, tx *sql.Tx, scope core.Scope, batch Batch) (Counts, []bool, error) {
	var repository int64
	if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repository); err != nil {
		return Counts{}, nil, err
	}
	n, err := revision(ctx, tx, scope.ProjectID)
	if err != nil {
		return Counts{}, nil, err
	}
	if repository != batch.RepositoryID || n != *batch.ExpectedRevision {
		return Counts{}, nil, conflict()
	}
	counts := Counts{Jobs: len(batch.Jobs)}
	fresh := make([]bool, len(batch.Jobs))
	for i, item := range batch.Jobs {
		if err := bindings(ctx, tx, scope.ProjectID, item); err != nil {
			return Counts{}, nil, err
		}
		counts.Checkpoints += len(item.Checkpoints)
		body, _ := json.Marshal(item)
		id := resourceID(scope.ProjectID, batch.Source, "job", "", item.SourceKey)
		var oldID, oldHash string
		err := tx.QueryRowContext(ctx, `SELECT job_id,item_sha256 FROM coordination_import_records WHERE project_id=? AND source=? AND source_key=?`, scope.ProjectID, batch.Source, item.SourceKey).Scan(&oldID, &oldHash)
		if err == nil {
			if oldID != id || oldHash != digest(body) {
				return Counts{}, nil, conflict()
			}
			counts.UnchangedJobs++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Counts{}, nil, err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE project_id=? AND id=?)`, scope.ProjectID, id).Scan(&exists); err != nil {
			return Counts{}, nil, err
		}
		if exists {
			return Counts{}, nil, conflict()
		}
		fresh[i] = true
		counts.NewJobs++
	}
	return counts, fresh, nil
}

func fingerprint(store *core.Store, scope core.Scope, batch Batch, body []byte, counts Counts) Preview {
	binding, _ := json.Marshal(struct {
		Domain, Instance, Project, Principal, Worker string
		Body                                         json.RawMessage
	}{"firecontroller.coordination-import.preview.v1", store.InstanceID(), scope.ProjectID, scope.PrincipalID, scope.WorkerID, body})
	return Preview{ProjectID: scope.ProjectID, ExpectedRevision: *batch.ExpectedRevision, PayloadSHA256: digest(body), PreviewSHA256: digest(binding), Counts: counts}
}

func selectedScope(store *core.Store, r *http.Request) (core.Scope, error) {
	scope, err := store.AdminScope(r)
	if err != nil {
		return scope, err
	}
	if scope.WorkerID == "" {
		return scope, invalid("Select the importing administrator's worker with X-FC-Worker.")
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		return scope, err
	}
	return scope, nil
}

func current(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := selectedScope(store, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var n int64
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var e error
			n, e = revision(ctx, tx, scope.ProjectID)
			return e
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, struct {
			Revision int64 `json:"revision"`
		}{n})
	}
}

func preview(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := selectedScope(store, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var batch Batch
		if err = core.ReadJSON(w, r, &batch); err != nil {
			core.WriteError(w, err)
			return
		}
		batch, body, err := Normalize(batch)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var counts Counts
		err = store.Read(r.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			var e error
			counts, _, e = admit(ctx, tx, scope, batch)
			return e
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, fingerprint(store, scope, batch, body, counts))
	}
}

func apply(store *core.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, err := selectedScope(store, r)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		var request ApplyRequest
		if err = core.ReadJSON(w, r, &request); err != nil {
			core.WriteError(w, err)
			return
		}
		batch, body, err := Normalize(request.Batch)
		if err != nil {
			core.WriteError(w, err)
			return
		}
		if !validHash(request.PreviewSHA256) {
			core.WriteError(w, invalid("preview_sha256 must contain sixty-four lowercase hexadecimal characters."))
			return
		}
		p := fingerprint(store, scope, batch, body, Counts{})
		if p.PreviewSHA256 != request.PreviewSHA256 {
			core.WriteError(w, core.Fail("import_preview_mismatch", "The selected content or destination differs from the preview; preview again.", 409))
			return
		}
		key, err := core.IdempotencyKey(r, "validation_failed")
		if err != nil {
			core.WriteError(w, err)
			return
		}
		result, err := store.Mutate(r.Context(), scope, core.Mutation{Operation: "coordination.import", Target: "jobs", Key: key, Request: batch}, func(ctx context.Context, tx *sql.Tx) (any, error) {
			counts, fresh, err := admit(ctx, tx, scope, batch)
			if err != nil {
				return nil, err
			}
			receipt := Receipt{Revision: *batch.ExpectedRevision, Outcome: "unchanged", Counts: counts, Mappings: make([]Mapping, 0, len(batch.Jobs))}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			for i, item := range batch.Jobs {
				id := resourceID(scope.ProjectID, batch.Source, "job", "", item.SourceKey)
				receipt.Mappings = append(receipt.Mappings, Mapping{SourceKey: item.SourceKey, JobID: id})
				if !fresh[i] {
					continue
				}
				ids := make([]string, len(item.Checkpoints))
				for j, cp := range item.Checkpoints {
					ids[j] = resourceID(scope.ProjectID, batch.Source, "checkpoint", item.SourceKey, cp.SourceKey)
				}
				if err := jobs.InsertPreparedJob(ctx, tx, scope.ProjectID, id, item, ids); err != nil {
					return nil, err
				}
				itemBody, _ := json.Marshal(item)
				if _, err := tx.ExecContext(ctx, `INSERT INTO coordination_import_records(project_id,source,source_key,job_id,import_revision,item_sha256,source_sha256,item_json,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, scope.ProjectID, batch.Source, item.SourceKey, id, *batch.ExpectedRevision+1, digest(itemBody), batch.SourceSHA256, string(itemBody), now); err != nil {
					return nil, err
				}
			}
			if counts.NewJobs > 0 {
				receipt.Revision++
				receipt.Outcome = "imported"
				if _, err := tx.ExecContext(ctx, `INSERT INTO coordination_import_state(project_id,revision) VALUES(?,?) ON CONFLICT(project_id) DO UPDATE SET revision=excluded.revision`, scope.ProjectID, receipt.Revision); err != nil {
					return nil, err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO coordination_import_batches(project_id,revision,source,source_sha256,payload_sha256,actor_id,worker_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, scope.ProjectID, receipt.Revision, batch.Source, batch.SourceSHA256, digest(body), scope.PrincipalID, scope.WorkerID, now); err != nil {
					return nil, err
				}
				if err := core.AppendEvent(ctx, tx, scope, "coordination.imported", scope.ProjectID, receipt.Revision, map[string]int{"jobs": counts.NewJobs}); err != nil {
					return nil, err
				}
			}
			return receipt, nil
		})
		if err != nil {
			core.WriteError(w, err)
			return
		}
		core.WriteJSON(w, 200, json.RawMessage(result))
	}
}
