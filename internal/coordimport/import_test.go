package coordimport_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/server"
)

type importFixture struct {
	t                           *testing.T
	store                       *core.Store
	handler                     http.Handler
	dir, admin, project, worker string
	lookups                     int
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()
	f := &importFixture{t: t, dir: filepath.Join(t.TempDir(), "private")}
	f.open()
	token, err := os.ReadFile(filepath.Join(f.dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f.admin = strings.TrimSpace(string(token))
	f.project = f.projectFor("owner/one")
	f.worker = f.workerFor(f.project, "Importer")
	t.Cleanup(func() {
		if f.store != nil {
			if err := f.store.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	return f
}
func (f *importFixture) open() {
	f.t.Helper()
	var err error
	f.store, err = core.Open(context.Background(), f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, ensure := range []func(context.Context, *core.Store) error{jobs.EnsureSchema, inbox.EnsureSchema, coordimport.EnsureSchema} {
		if err := ensure(context.Background(), f.store); err != nil {
			f.t.Fatal(err)
		}
	}
	mux := server.New(f.store, server.WithRepositoryResolver(func(_ context.Context, repo string) (server.RepositoryIdentity, error) {
		f.lookups++
		id := int64(991)
		if repo == "owner/two" {
			id = 992
		}
		return server.RepositoryIdentity{ID: id, FullName: repo}, nil
	}))
	jobs.Register(mux, f.store)
	inbox.Register(mux, f.store)
	coordimport.Register(mux, f.store)
	f.handler = mux
}
func (f *importFixture) restart() {
	f.t.Helper()
	if err := f.store.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.store = nil
	f.open()
}
func (f *importFixture) call(method, path, token, worker, key string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func checked[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func errorIs(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	out := checked[struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}](t, w, status)
	if out.Error.Code != code {
		t.Fatalf("error code %q, want %q", out.Error.Code, code)
	}
}
func (f *importFixture) projectFor(repo string) string {
	f.t.Helper()
	out := checked[struct {
		ID string `json:"id"`
	}](f.t, f.call("POST", "/api/v1/projects", f.admin, "", core.NewID("key"), map[string]string{"name": "Selected project", "repository": repo}), 200)
	return out.ID
}
func (f *importFixture) workerFor(project, name string) string {
	f.t.Helper()
	out := checked[struct {
		ID string `json:"id"`
	}](f.t, f.call("POST", "/api/v1/projects/"+project+"/workers", f.admin, "", core.NewID("key"), map[string]string{"name": name}), 200)
	return out.ID
}
func (f *importFixture) grant(project, token string, write bool) string {
	f.t.Helper()
	sum := sha256.Sum256([]byte(token))
	permissions := []string{"read"}
	if write {
		permissions = append(permissions, "write")
	}
	out := checked[struct {
		ID string `json:"id"`
	}](f.t, f.call("POST", "/api/v1/projects/"+project+"/credentials", f.admin, "", core.NewID("key"), map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": permissions}), 200)
	return out.ID
}
func (f *importFixture) base() string {
	return "/api/v1/projects/" + f.project + "/coordination/imports"
}
func preparedBatch(worker string, revision int64) coordimport.Batch {
	return coordimport.Batch{Version: 1, RepositoryID: 991, ExpectedRevision: &revision, Source: "selected-legacy-export", SourceSHA256: strings.Repeat("a", 64), Jobs: []coordimportmodel.PreparedJob{{
		SourceKey: "original-job-17", Title: "Retained <literal> work", Brief: "Original brief\nExact historical text <script>inert</script>", WorkerID: worker, Status: "blocked", SourceRevision: 9,
		CreatedAt: "2024-03-01T01:02:03.123456789Z", UpdatedAt: "2024-03-07T04:05:06+01:00",
		Checkpoints: []coordimportmodel.PreparedCheckpoint{
			{SourceKey: "checkpoint-41", ActorID: "original-author", WorkerID: worker, Done: "Original done\nline two", Next: "Original next", Blocker: "Original blocker", CreatedAt: "2024-03-05T05:06:07Z", Evidence: []string{"source:old-proof-41"}},
			{SourceKey: "checkpoint-55", ActorID: "second-author", WorkerID: "", Done: "Later source checkpoint", Next: "Continue selected work", CreatedAt: "2024-03-06T05:06:07.8Z", Evidence: []string{}},
		}}}}
}
func (f *importFixture) preview(batch coordimport.Batch) coordimport.Preview {
	f.t.Helper()
	return checked[coordimport.Preview](f.t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", batch), 200)
}
func (f *importFixture) apply(batch coordimport.Batch, preview coordimport.Preview, key string) *httptest.ResponseRecorder {
	return f.call("POST", f.base()+"/apply", f.admin, f.worker, key, coordimport.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Batch: batch})
}

// Capture exact selected fixture rows, not just counts: refusals and previews
// must conserve previous private records, receipts, events and sequence state.
func (f *importFixture) snapshot(skipReceipts bool) map[string]string {
	f.t.Helper()
	out := map[string]string{}
	for _, table := range []string{"metadata", "projects", "workers", "credentials", "grants", "receipts", "events", "jobs", "job_history", "job_checkpoints", "job_updates", "job_checklists", "job_checklist_history", "worker_primary_jobs", "worker_primary_job_history", "worker_pause_state", "worker_pause_history", "notes", "note_revisions", "inbox_conversations", "inbox_messages", "inbox_recipient_state", "coordination_import_state", "coordination_import_records", "coordination_import_batches", "sqlite_sequence"} {
		if skipReceipts && table == "receipts" {
			continue
		}
		rows, err := f.store.DB().Query("SELECT * FROM " + table + " ORDER BY rowid")
		if err != nil {
			f.t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			f.t.Fatal(err)
		}
		data := [][]any{}
		for rows.Next() {
			values := make([]any, len(cols))
			dest := make([]any, len(cols))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				rows.Close()
				f.t.Fatal(err)
			}
			data = append(data, values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			f.t.Fatal(err)
		}
		encoded, err := json.Marshal(data)
		if err != nil {
			f.t.Fatal(err)
		}
		out[table] = string(encoded)
	}
	return out
}
func conserved(t *testing.T, before, after map[string]string) {
	t.Helper()
	for table, data := range before {
		if after[table] != data {
			t.Errorf("retained %s rows changed", table)
		}
	}
}

func TestPreparedCoordinationImportPreservesHistoryAndReplayAfterEditRestart(t *testing.T) {
	f := newImportFixture(t)
	// Populate unrelated native coordination through its real public APIs, so
	// conservation assertions cover retained rows rather than only empty tables.
	other := f.workerFor(f.project, "Unrelated worker")
	jobBase := "/api/v1/projects/" + f.project
	ordinary := checked[struct {
		ID string `json:"id"`
	}](t, f.call("POST", jobBase+"/jobs", f.admin, f.worker, "unrelated-job", map[string]string{"title": "Existing ordinary job", "brief": "Preserve this brief", "worker_id": other}), 201)
	checked[map[string]any](t, f.call("POST", jobBase+"/notes", f.admin, f.worker, "unrelated-note", map[string]string{"worker_id": other, "job_id": ordinary.ID, "phase": "", "kind": "source", "body": "Existing note"}), 201)
	checked[map[string]any](t, f.call("POST", jobBase+"/messages", f.admin, f.worker, "unrelated-message", map[string]string{"recipient_worker_id": other, "body": "Existing message"}), 201)
	ordinaryBefore := f.call("GET", jobBase+"/jobs/"+ordinary.ID, f.admin, f.worker, "", nil)
	checked[map[string]any](t, ordinaryBefore, 200)
	batch := preparedBatch(f.worker, 0)
	before := f.snapshot(false)
	lookups := f.lookups
	current := checked[struct {
		Revision int64 `json:"revision"`
	}](t, f.call("GET", f.base(), f.admin, f.worker, "", nil), 200)
	if current.Revision != 0 {
		t.Fatal("fresh import revision is not zero")
	}
	p := f.preview(batch)
	if p.ProjectID != f.project || p.ExpectedRevision != 0 || len(p.PreviewSHA256) != 64 || len(p.PayloadSHA256) != 64 || p.Counts != (coordimport.Counts{Jobs: 1, Checkpoints: 2, NewJobs: 1}) {
		t.Fatalf("wrong preview: %+v", p)
	}
	conserved(t, before, f.snapshot(false))
	w := f.apply(batch, p, "selected-apply")
	receipt := checked[coordimport.Receipt](t, w, 200)
	if receipt.Revision != 1 || receipt.Outcome != "imported" || receipt.Counts != p.Counts || len(receipt.Mappings) != 1 || receipt.Mappings[0].SourceKey != batch.Jobs[0].SourceKey {
		t.Fatalf("wrong receipt: %+v", receipt)
	}
	if strings.Contains(w.Body.String(), "Original brief") || strings.Contains(w.Body.String(), "Original done") {
		t.Fatal("compact receipt leaked historical bodies")
	}
	id := receipt.Mappings[0].JobID
	jobPath := "/api/v1/projects/" + f.project + "/jobs/" + id
	var original struct {
		ID, ProjectID, Title, Brief, WorkerID, Status, CreatedAt, UpdatedAt string
		Revision                                                            int64
	}
	// Decode native JSON explicitly so spelling changes cannot silently zero fields.
	full := checked[map[string]json.RawMessage](t, f.call("GET", jobPath, f.admin, f.worker, "", nil), 200)
	read := func(key string, out any) {
		t.Helper()
		if err := json.Unmarshal(full[key], out); err != nil {
			t.Fatal(err)
		}
	}
	read("id", &original.ID)
	read("project_id", &original.ProjectID)
	read("title", &original.Title)
	read("brief", &original.Brief)
	read("worker_id", &original.WorkerID)
	read("status", &original.Status)
	read("created_at", &original.CreatedAt)
	read("updated_at", &original.UpdatedAt)
	read("revision", &original.Revision)
	item := batch.Jobs[0]
	if original.ID != id || original.ProjectID != f.project || original.Title != item.Title || original.Brief != item.Brief || original.WorkerID != item.WorkerID || original.Status != item.Status || original.CreatedAt != item.CreatedAt || original.UpdatedAt != item.UpdatedAt || original.Revision != 1 {
		t.Fatal("native job changed selected historical snapshot")
	}
	contextResult := checked[struct {
		Complete   bool `json:"complete"`
		Checkpoint struct {
			ActorID     string `json:"actor_id"`
			WorkerID    string `json:"worker_id"`
			Done        string `json:"done"`
			CreatedAt   string `json:"created_at"`
			JobRevision int64  `json:"job_revision"`
		} `json:"checkpoint"`
	}](t, f.call("GET", jobPath+"/context", f.admin, f.worker, "", nil), 200)
	cp := item.Checkpoints[1]
	if !contextResult.Complete || contextResult.Checkpoint.ActorID != cp.ActorID || contextResult.Checkpoint.WorkerID != cp.WorkerID || contextResult.Checkpoint.Done != cp.Done || contextResult.Checkpoint.CreatedAt != cp.CreatedAt || contextResult.Checkpoint.JobRevision != 1 {
		t.Fatal("native context did not expose preserved checkpoint")
	}
	page := checked[struct {
		Items []struct {
			ID        string   `json:"id"`
			ActorID   string   `json:"actor_id"`
			Done      string   `json:"done"`
			Next      string   `json:"next"`
			Blocker   string   `json:"blocker"`
			Evidence  []string `json:"evidence"`
			CreatedAt string   `json:"created_at"`
		} `json:"items"`
		HasMore bool `json:"has_more"`
	}](t, f.call("GET", jobPath+"/checkpoints", f.admin, f.worker, "", nil), 200)
	if len(page.Items) != 2 || page.HasMore {
		t.Fatal("checkpoint archive incomplete")
	}
	for i, actual := range page.Items {
		want := item.Checkpoints[1-i]
		if actual.ID == "" || actual.ActorID != want.ActorID || actual.Done != want.Done || actual.Next != want.Next || actual.Blocker != want.Blocker || !reflect.DeepEqual(actual.Evidence, want.Evidence) || actual.CreatedAt != want.CreatedAt {
			t.Fatal("checkpoint original facts changed")
		}
	}
	var mappedJob, sourceHash, raw string
	if err := f.store.DB().QueryRow(`SELECT job_id,source_sha256,item_json FROM coordination_import_records WHERE project_id=? AND source=? AND source_key=?`, f.project, batch.Source, item.SourceKey).Scan(&mappedJob, &sourceHash, &raw); err != nil {
		t.Fatal(err)
	}
	var retained coordimportmodel.PreparedJob
	if err := json.Unmarshal([]byte(raw), &retained); err != nil {
		t.Fatal(err)
	}
	if mappedJob != id || sourceHash != batch.SourceSHA256 || !reflect.DeepEqual(retained, item) {
		t.Fatal("source mapping/provenance changed")
	}
	history := checked[struct {
		Items []struct {
			ActorID string                     `json:"actor_id"`
			State   map[string]json.RawMessage `json:"state"`
		} `json:"items"`
	}](t, f.call("GET", jobPath+"/history", f.admin, f.worker, "", nil), 200)
	if len(history.Items) != 1 || history.Items[0].ActorID != "imported-history" {
		t.Fatal("source revision was fabricated as native history")
	}
	checked[map[string]any](t, f.call("PATCH", jobPath, f.admin, f.worker, "ordinary-edit", map[string]any{"expected_revision": 1, "brief": "New ordinary brief"}), 200)
	afterEdit := f.snapshot(false)
	replay := f.apply(batch, p, "selected-apply")
	checked[coordimport.Receipt](t, replay, 200)
	if !bytes.Equal(w.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("retry changed original receipt")
	}
	conserved(t, afterEdit, f.snapshot(false))
	instance := f.store.InstanceID()
	f.restart()
	if f.store.InstanceID() != instance {
		t.Fatal("restart changed instance")
	}
	afterRestart := f.snapshot(false)
	replay = f.apply(batch, p, "selected-apply")
	checked[coordimport.Receipt](t, replay, 200)
	if !bytes.Equal(w.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("restart retry changed receipt")
	}
	conserved(t, afterRestart, f.snapshot(false))
	next := batch
	one := int64(1)
	next.ExpectedRevision = &one
	unchanged := f.preview(next)
	if unchanged.Counts.NewJobs != 0 || unchanged.Counts.UnchangedJobs != 1 {
		t.Fatal("same source no longer unchanged")
	}
	noEffect := f.snapshot(true)
	unchangedReceipt := checked[coordimport.Receipt](t, f.apply(next, unchanged, "unchanged-new-key"), 200)
	if unchangedReceipt.Outcome != "unchanged" || unchangedReceipt.Revision != 1 {
		t.Fatal("new-key retry advanced import state")
	}
	conserved(t, noEffect, f.snapshot(true))
	edited := checked[struct {
		Brief    string `json:"brief"`
		Revision int64  `json:"revision"`
	}](t, f.call("GET", jobPath, f.admin, f.worker, "", nil), 200)
	if edited.Brief != "New ordinary brief" || edited.Revision != 2 {
		t.Fatal("import overwrote ordinary edit")
	}
	if f.lookups != lookups {
		t.Fatal("coordination import performed provider resolution")
	}
	final := f.snapshot(false)
	for _, table := range []string{"notes", "note_revisions", "inbox_conversations", "inbox_messages", "inbox_recipient_state", "worker_primary_jobs", "worker_primary_job_history", "worker_pause_state", "worker_pause_history"} {
		if final[table] != before[table] {
			t.Errorf("unrelated %s changed during import/replay", table)
		}
	}
	ordinaryAfter := f.call("GET", jobBase+"/jobs/"+ordinary.ID, f.admin, f.worker, "", nil)
	checked[map[string]any](t, ordinaryAfter, 200)
	if !bytes.Equal(ordinaryBefore.Body.Bytes(), ordinaryAfter.Body.Bytes()) {
		t.Fatal("unrelated ordinary job changed")
	}
}

func TestPreparedCoordinationImportConflictsRemainAtomic(t *testing.T) {
	f := newImportFixture(t)
	batch := preparedBatch(f.worker, 0)
	p := f.preview(batch)
	checked[coordimport.Receipt](t, f.apply(batch, p, "first"), 200)
	before := f.snapshot(false)
	errorIs(t, f.apply(batch, p, "stale-key"), 409, "coordination_import_conflict")
	changed := preparedBatch(f.worker, 1)
	changed.Jobs[0].Brief = "Different old source content"
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", changed), 409, "coordination_import_conflict")
	// A valid changed preview cannot reuse a prior action key for another source item.
	fresh := preparedBatch(f.worker, 1)
	fresh.Jobs[0].SourceKey = "second-job"
	freshPreview := f.preview(fresh)
	errorIs(t, f.apply(fresh, freshPreview, "first"), 409, "idempotency_conflict")
	tampered := fresh
	tampered.Jobs = append([]coordimportmodel.PreparedJob(nil), fresh.Jobs...)
	tampered.Jobs[0].Title = "Changed since preview"
	errorIs(t, f.apply(tampered, freshPreview, "changed-preview"), 409, "import_preview_mismatch")
	wrongRepo := preparedBatch(f.worker, 1)
	wrongRepo.RepositoryID = 992
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", wrongRepo), 409, "coordination_import_conflict")
	conserved(t, before, f.snapshot(false))
}

func TestPreparedCoordinationImportAuthorityAndPreviewBindings(t *testing.T) {
	f := newImportFixture(t)
	otherProject := f.projectFor("owner/two")
	foreignWorker := f.workerFor(otherProject, "Foreign")
	secondWorker := f.workerFor(f.project, "Other importer")
	readToken, writeToken := "only-read-token", "ordinary-write-token"
	f.grant(f.project, readToken, false)
	writeID := f.grant(f.project, writeToken, true)
	batch := preparedBatch(f.worker, 0)
	p := f.preview(batch)
	before := f.snapshot(false)
	for _, token := range []string{readToken, writeToken} {
		for _, route := range []string{f.base(), f.base() + "/preview", f.base() + "/apply"} {
			method := "POST"
			var body any = batch
			if route == f.base() {
				method = "GET"
				body = nil
			}
			if strings.HasSuffix(route, "/apply") {
				body = coordimport.ApplyRequest{PreviewSHA256: p.PreviewSHA256, Batch: batch}
			}
			errorIs(t, f.call(method, route, token, f.worker, "denied", body), 403, "forbidden")
		}
	}
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, "", "", batch), 400, "validation_failed")
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, foreignWorker, "", batch), 404, "not_found")
	foreignTarget := preparedBatch(foreignWorker, 0)
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", foreignTarget), 404, "not_found")
	foreignCheckpoint := preparedBatch(f.worker, 0)
	foreignCheckpoint.Jobs[0].Checkpoints[0].WorkerID = foreignWorker
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", foreignCheckpoint), 404, "not_found")
	request := coordimport.ApplyRequest{PreviewSHA256: p.PreviewSHA256, Batch: batch}
	errorIs(t, f.call("POST", f.base()+"/apply", f.admin, secondWorker, "changed-worker", request), 409, "import_preview_mismatch")
	errorIs(t, f.call("POST", "/api/v1/projects/"+otherProject+"/coordination/imports/apply", f.admin, foreignWorker, "changed-project", request), 409, "import_preview_mismatch")
	errorIs(t, f.call("POST", f.base()+"/apply", "", f.worker, "missing-auth", request), 401, "unauthorized")
	errorIs(t, f.call("POST", f.base()+"/apply", f.admin, f.worker, "", request), 400, "validation_failed")
	// Even an administrator's real browser cookie remains attenuated: the
	// session's explicit write allowance never includes historical imports.
	sessionID := strings.Repeat("b", 32)
	session := f.call("POST", "/api/v1/projects/"+f.project+"/browser-session", f.admin, f.worker, "", map[string]string{"session_id": sessionID})
	checked[core.BrowserSession](t, session, 200)
	cookies := session.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("browser session did not issue a cookie")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "https://example.com"+f.base()+"/apply", bytes.NewReader(encoded))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://example.com")
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("X-FC-Worker", f.worker)
	r.Header.Set("X-FC-Browser-Session", sessionID)
	r.Header.Set("Idempotency-Key", "browser-denied")
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	browserDenied := httptest.NewRecorder()
	f.handler.ServeHTTP(browserDenied, r)
	errorIs(t, browserDenied, 403, "forbidden")
	conserved(t, before, f.snapshot(false))
	// Revoke an actual granted credential through the supported access API.
	checked[map[string]any](t, f.call("DELETE", "/api/v1/projects/"+f.project+"/credentials/"+writeID, f.admin, f.worker, "revoke", nil), 200)
	afterRevocation := f.snapshot(false)
	errorIs(t, f.call("POST", f.base()+"/apply", writeToken, f.worker, "revoked", request), 401, "unauthorized")
	conserved(t, afterRevocation, f.snapshot(false))
}

func TestPreparedCoordinationImportRevokedAdminCannotReplay(t *testing.T) {
	f := newImportFixture(t)
	batch := preparedBatch(f.worker, 0)
	p := f.preview(batch)
	checked[coordimport.Receipt](t, f.apply(batch, p, "completed"), 200)
	// The public revocation endpoint intentionally cannot revoke administrators.
	// Change only this isolated credential's current access, not import evidence.
	hash := sha256.Sum256([]byte(f.admin))
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE token_sha256=?`, hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	before := f.snapshot(false)
	errorIs(t, f.apply(batch, p, "completed"), 401, "unauthorized")
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", batch), 401, "unauthorized")
	conserved(t, before, f.snapshot(false))
}

func TestPreparedCoordinationImportStorageFailureRollsBackAndKeyRetries(t *testing.T) {
	for _, stage := range []string{"second-record", "final-event"} {
		t.Run(stage, func(t *testing.T) {
			f := newImportFixture(t)
			batch := preparedBatch(f.worker, 0)
			second := batch.Jobs[0]
			second.SourceKey = "second-job"
			batch.Jobs = append(batch.Jobs, second)
			p := f.preview(batch)
			definition := `CREATE TRIGGER import_failure BEFORE INSERT ON coordination_import_records WHEN NEW.source_key='second-job' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`
			if stage == "final-event" {
				definition = `CREATE TRIGGER import_failure BEFORE INSERT ON events WHEN NEW.event_type='coordination.imported' BEGIN SELECT RAISE(ABORT,'fixture failure'); END`
			}
			if _, err := f.store.DB().Exec(definition); err != nil {
				t.Fatal(err)
			}
			before := f.snapshot(false)
			response := f.apply(batch, p, "retryable-key")
			if response.Code != 500 {
				t.Fatalf("failure status %d, want500", response.Code)
			}
			conserved(t, before, f.snapshot(false))
			if _, err := f.store.DB().Exec(`DROP TRIGGER import_failure`); err != nil {
				t.Fatal(err)
			}
			receipt := checked[coordimport.Receipt](t, f.apply(batch, p, "retryable-key"), 200)
			if receipt.Revision != 1 || receipt.Counts.NewJobs != 2 || receipt.Counts.Checkpoints != 4 {
				t.Fatal("rollback consumed retry key or retained partial data")
			}
		})
	}
}

func TestPreparedCoordinationImportBatchBounds(t *testing.T) {
	f := newImportFixture(t)
	before := f.snapshot(false)
	tooMany := preparedBatch(f.worker, 0)
	template := tooMany.Jobs[0]
	tooMany.Jobs = nil
	for i := 0; i < 26; i++ {
		item := template
		item.SourceKey = fmt.Sprint("job-", i)
		tooMany.Jobs = append(tooMany.Jobs, item)
	}
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", tooMany), 400, "validation_failed")
	tooManyCheckpoints := preparedBatch(f.worker, 0)
	second := template
	second.SourceKey = "second-job"
	tooManyCheckpoints.Jobs = append(tooManyCheckpoints.Jobs, second)
	for j, n := range []int{60, 41} {
		tooManyCheckpoints.Jobs[j].Checkpoints = nil
		for i := 0; i < n; i++ {
			cp := template.Checkpoints[0]
			cp.SourceKey = fmt.Sprint("checkpoint-", i)
			tooManyCheckpoints.Jobs[j].Checkpoints = append(tooManyCheckpoints.Jobs[j].Checkpoints, cp)
		}
	}
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", tooManyCheckpoints), 400, "validation_failed")
	conserved(t, before, f.snapshot(false))
	// Admit the combined job/checkpoint ceiling through the real coordinator.
	maximum := preparedBatch(f.worker, 0)
	maximum.Jobs = nil
	for i := 0; i < 25; i++ {
		item := template
		item.SourceKey = fmt.Sprint("job-", i)
		item.Checkpoints = nil
		for j := 0; j < 4; j++ {
			cp := template.Checkpoints[0]
			cp.SourceKey = fmt.Sprint("checkpoint-", j)
			item.Checkpoints = append(item.Checkpoints, cp)
		}
		maximum.Jobs = append(maximum.Jobs, item)
	}
	p := f.preview(maximum)
	if p.Counts.Jobs != 25 || p.Counts.Checkpoints != 100 {
		t.Fatal("maximum preview counts incomplete")
	}
	receipt := checked[coordimport.Receipt](t, f.apply(maximum, p, "maximum"), 200)
	if receipt.Counts.NewJobs != 25 || receipt.Counts.Checkpoints != 100 || len(receipt.Mappings) != 25 {
		t.Fatal("maximum batch not fully admitted")
	}
}

func TestPreparedCoordinationImportPreviewFitsApplyByteBudget(t *testing.T) {
	f := newImportFixture(t)
	batch := preparedBatch(f.worker, 0)
	template := batch.Jobs[0]
	batch.Jobs = nil
	for i := 0; i < 8; i++ {
		item := template
		item.SourceKey = fmt.Sprint("byte-budget-", i)
		item.Brief = strings.Repeat("x", 32<<10)
		if i == 7 {
			item.Brief = ""
		}
		batch.Jobs = append(batch.Jobs, item)
	}
	base, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	remaining := (256 << 10) - 1 - len(base)
	if remaining < 512 || remaining > 32<<10 {
		t.Fatal("fixture no longer reaches valid individual brief bounds")
	}
	batch.Jobs[7].Brief = strings.Repeat("x", remaining)
	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := json.Marshal(coordimport.ApplyRequest{PreviewSHA256: strings.Repeat("0", 64), Batch: batch})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 256<<10 || len(wrapper) <= 256<<10 {
		t.Fatal("fixture no longer distinguishes preview from eventual apply wrapper")
	}
	before := f.snapshot(false)
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", batch), 400, "validation_failed")
	conserved(t, before, f.snapshot(false))
	batch.Jobs[7].Brief = strings.Repeat("x", remaining-512)
	p := f.preview(batch)
	conserved(t, before, f.snapshot(false))
	receipt := checked[coordimport.Receipt](t, f.apply(batch, p, "near-byte-budget"), 200)
	if receipt.Counts.NewJobs != 8 || len(receipt.Mappings) != 8 {
		t.Fatal("near-limit approved batch was not fully applied")
	}
}
