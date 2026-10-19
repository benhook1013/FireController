package historycollect

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// EnsureSchema owns acquisition records independently of native execution.
// Installing it does not start provider work or grant an import capability.
func EnsureSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "history_collection", SchemaSQL())
}

// SchemaSQL returns an owned copy of this domain's canonical physical schema.
func SchemaSQL() []string {
	return []string{
		`CREATE TABLE history_collections(id TEXT NOT NULL PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),repository_id INTEGER NOT NULL,actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,reader_fingerprint TEXT NOT NULL,selection_json TEXT NOT NULL,state TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>0),reason TEXT NOT NULL,position_json TEXT NOT NULL,pass_number INTEGER NOT NULL CHECK(pass_number>0),page_count INTEGER NOT NULL CHECK(page_count>=0),record_count INTEGER NOT NULL CHECK(record_count>=0),created_at TEXT NOT NULL,updated_at TEXT NOT NULL,UNIQUE(project_id,id))`,
		`CREATE INDEX history_collection_pending ON history_collections(state,updated_at,id)`,
		`CREATE INDEX history_collection_project_pending ON history_collections(project_id,state)`,
		`CREATE TABLE history_collection_pages(id TEXT NOT NULL PRIMARY KEY,project_id TEXT NOT NULL,collection_id TEXT NOT NULL,sequence INTEGER NOT NULL CHECK(sequence>0),pass_number INTEGER NOT NULL CHECK(pass_number>0),reader_fingerprint TEXT NOT NULL,pull_number INTEGER NOT NULL,resource TEXT NOT NULL,page_number INTEGER NOT NULL CHECK(page_number>0),record_count INTEGER NOT NULL CHECK(record_count>=0),body_sha256 TEXT NOT NULL,body_json TEXT NOT NULL,created_at TEXT NOT NULL,FOREIGN KEY(project_id,collection_id) REFERENCES history_collections(project_id,id),UNIQUE(project_id,collection_id,id),UNIQUE(collection_id,sequence),UNIQUE(collection_id,pass_number,page_number))`,
		`CREATE TABLE history_collection_seen(project_id TEXT NOT NULL,collection_id TEXT NOT NULL,pass_number INTEGER NOT NULL CHECK(pass_number>0),record_id TEXT NOT NULL,page_id TEXT NOT NULL,PRIMARY KEY(collection_id,pass_number,record_id),FOREIGN KEY(project_id,collection_id,page_id) REFERENCES history_collection_pages(project_id,collection_id,id))`,
	}
}

type Operation struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	RepositoryID int64     `json:"repository_id"`
	State        string    `json:"state"`
	Revision     int64     `json:"revision"`
	Reason       string    `json:"reason"`
	Selection    Selection `json:"selection"`
	Position     Position  `json:"position"`
	PassNumber   int64     `json:"pass_number"`
	PageCount    int64     `json:"page_count"`
	RecordCount  int64     `json:"record_count"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
}

type retainedOperation struct {
	Operation
	actor, worker, fingerprint string
}

type Receipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	State    string `json:"state"`
}

type Engine struct {
	store        *core.Store
	provider     Provider
	fingerprint  string
	start        sync.Mutex
	running      bool
	wake         chan struct{}
	waitMu       sync.Mutex
	waiters      map[string]map[*waiter]struct{}
	waitCount    int
	principals   map[string]int
	waitLifetime context.Context
}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var operationPattern = regexp.MustCompile(`^history_collection_[0-9a-f]{32}$`)

// EngineOption composes trusted application integrations before use.
type EngineOption func(*Engine)

// WithWaitLifetime detaches result observers when the service stops. It never
// cancels accepted collection work or provider acquisition.
func WithWaitLifetime(ctx context.Context) EngineOption {
	return func(e *Engine) { e.waitLifetime = ctx }
}

func New(store *core.Store, provider Provider, fingerprint string, options ...EngineOption) (*Engine, error) {
	if store == nil || provider != nil && !digestPattern.MatchString(fingerprint) || provider == nil && fingerprint != "" {
		return nil, core.Fail("configuration_invalid", "The history reader configuration is invalid.", 500)
	}
	e := &Engine{store: store, provider: provider, fingerprint: fingerprint, wake: make(chan struct{}, 1), waiters: make(map[string]map[*waiter]struct{}), principals: make(map[string]int)}
	for _, option := range options {
		if option == nil {
			return nil, core.Fail("configuration_invalid", "The history reader configuration is invalid.", 500)
		}
		option(e)
	}
	return e, nil
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func storageError() error {
	return core.Fail("storage_unavailable", "Stored history collection is unavailable.", 500)
}
func conflict() error {
	return core.Fail("revision_conflict", "The collection changed; read its current state.", 409)
}
func missing() error {
	return core.Fail("not_found", "The selected collection is unavailable.", 404)
}

// An administrator starts acquisition with an explicit project and worker.
// Background reads recheck retained principal authority before provider work
// and again before retaining its response.
func checkActor(ctx context.Context, tx *sql.Tx, scope core.Scope) error {
	if scope.ProjectID == "" || scope.WorkerID == "" {
		return core.Fail("worker_required", "Select the collection project and worker.", 400)
	}
	var allowed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM credentials WHERE principal_id=? AND is_admin=1 AND revoked=0)`, scope.PrincipalID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return core.Fail("forbidden", "Administrative permission is required.", 403)
	}
	return core.CheckWorker(ctx, tx, scope.ProjectID, scope.WorkerID)
}

func checkCapacity(ctx context.Context, tx *sql.Tx, projectID string) error {
	var total, project int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(CASE WHEN project_id=? THEN 1 ELSE 0 END),0) FROM history_collections WHERE state IN ('queued','running')`, projectID).Scan(&total, &project); err != nil {
		return err
	}
	if total >= 64 || project >= 16 {
		return core.Fail("overloaded", "History collection capacity is occupied; try later.", 429)
	}
	return nil
}

// Enqueue performs only local storage work. Result waiting and provider work are
// separate; exact original-key replay remains useful with a disabled reader.
func (e *Engine) Enqueue(ctx context.Context, scope core.Scope, key string, selection Selection) (json.RawMessage, error) {
	s, err := Normalize(selection)
	if err != nil {
		return nil, err
	}
	mutation := core.Mutation{Operation: "history.collect", Target: "collections", Key: key, Request: s}
	result, err := e.store.Mutate(ctx, scope, mutation, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if err := checkActor(ctx, tx, scope); err != nil {
			return nil, err
		}
		if e.provider == nil {
			return nil, core.Fail("collection_unavailable", "No history reader is configured.", 409)
		}
		if err := checkCapacity(ctx, tx, scope.ProjectID); err != nil {
			return nil, err
		}
		var repositoryID int64
		if err := tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&repositoryID); err != nil {
			return nil, err
		}
		if repositoryID <= 0 {
			return nil, storageError()
		}
		id, at := core.NewID("history_collection"), stamp()
		body, _ := json.Marshal(s)
		position, _ := json.Marshal(Start())
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_collections(id,project_id,repository_id,actor_id,worker_id,reader_fingerprint,selection_json,state,revision,reason,position_json,pass_number,page_count,record_count,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'queued',1,'',?,1,0,0,?,?)`, id, scope.ProjectID, repositoryID, scope.PrincipalID, scope.WorkerID, e.fingerprint, string(body), string(position), at, at); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "history.collection_created", id, 1, struct{}{}); err != nil {
			return nil, err
		}
		return Receipt{ID: id, Revision: 1, State: "queued"}, nil
	})
	if err == nil {
		e.signal()
	}
	return result, err
}

func readOperation(ctx context.Context, tx *sql.Tx, project, id string) (retainedOperation, error) {
	var r retainedOperation
	var selection, position string
	err := tx.QueryRowContext(ctx, `SELECT id,project_id,repository_id,actor_id,worker_id,reader_fingerprint,selection_json,state,revision,reason,position_json,pass_number,page_count,record_count,created_at,updated_at FROM history_collections WHERE project_id=? AND id=?`, project, id).Scan(&r.ID, &r.ProjectID, &r.RepositoryID, &r.actor, &r.worker, &r.fingerprint, &selection, &r.State, &r.Revision, &r.Reason, &position, &r.PassNumber, &r.PageCount, &r.RecordCount, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, missing()
	}
	if err != nil {
		return r, err
	}
	if len(selection) > 4096 || len(position) > 256 || strictjson.Decode([]byte(selection), &r.Selection) != nil || strictjson.Decode([]byte(position), &r.Position) != nil {
		return r, storageError()
	}
	if _, err := Normalize(r.Selection); err != nil || !validPosition(r.Selection, r.Position) || r.Revision < 1 || r.PassNumber < 1 || r.PageCount < 0 || r.RecordCount < 0 || !digestPattern.MatchString(r.fingerprint) {
		return r, storageError()
	}
	switch r.State {
	case "queued", "running", "paused", "completed", "canceled":
	default:
		return r, storageError()
	}
	return r, nil
}

func (e *Engine) Read(ctx context.Context, scope core.Scope, id string) (Operation, error) {
	var result Operation
	if !operationPattern.MatchString(id) {
		return result, invalid()
	}
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		if err := checkActor(ctx, tx, scope); err != nil {
			return err
		}
		r, err := readOperation(ctx, tx, scope.ProjectID, id)
		result = r.Operation
		return err
	})
	return result, err
}

// Change is a guarded explicit local action. Resume starts a new retained pass
// at page one of the unfinished resource; cancellation never erases captures.
func (e *Engine) Change(ctx context.Context, scope core.Scope, id, key, action string, expected int64) (json.RawMessage, error) {
	if !operationPattern.MatchString(id) || expected < 1 || action != "resume" && action != "cancel" {
		return nil, invalid()
	}
	request := struct {
		ExpectedRevision int64  `json:"expected_revision"`
		Action           string `json:"action"`
	}{expected, action}
	result, err := e.store.Mutate(ctx, scope, core.Mutation{Operation: "history." + action, Target: id, Key: key, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		if err := checkActor(ctx, tx, scope); err != nil {
			return nil, err
		}
		r, err := readOperation(ctx, tx, scope.ProjectID, id)
		if err != nil {
			return nil, err
		}
		if r.Revision != expected || r.State == "completed" || r.State == "canceled" || action == "resume" && r.State != "paused" {
			return nil, conflict()
		}
		state, pass, fingerprint := "canceled", r.PassNumber, r.fingerprint
		if action == "resume" {
			if scope.PrincipalID != r.actor || scope.WorkerID != r.worker {
				return nil, core.Fail("collection_binding_changed", "Resume using the initiating principal and worker, or start a new selected collection.", 409)
			}
			if e.provider == nil {
				return nil, core.Fail("collection_unavailable", "No history reader is configured.", 409)
			}
			if err := checkCapacity(ctx, tx, scope.ProjectID); err != nil {
				return nil, err
			}
			state, pass, fingerprint = "queued", pass+1, e.fingerprint
			r.Position.Page = 1
		}
		position, _ := json.Marshal(r.Position)
		if _, err := tx.ExecContext(ctx, `UPDATE history_collections SET state=?,revision=revision+1,reason='',position_json=?,pass_number=?,reader_fingerprint=?,updated_at=? WHERE project_id=? AND id=? AND revision=?`, state, string(position), pass, fingerprint, stamp(), scope.ProjectID, id, expected); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "history.collection_changed", id, expected+1, struct{}{}); err != nil {
			return nil, err
		}
		return Receipt{ID: id, Revision: expected + 1, State: state}, nil
	})
	if err == nil {
		e.notify(id)
		e.signal()
	}
	return result, err
}
