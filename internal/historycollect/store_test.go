package historycollect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/server"
)

type collectionStoreFixture struct {
	store      *core.Store
	handler    http.Handler
	dir, admin string
	provider   *stepProvider
}

func newCollectionStoreFixture(t *testing.T) *collectionStoreFixture {
	t.Helper()
	f := &collectionStoreFixture{dir: filepath.Join(t.TempDir(), "private"), provider: newStepProvider(t)}
	f.open(t)
	t.Cleanup(func() { _ = f.store.Close() })
	return f
}
func (f *collectionStoreFixture) open(t *testing.T) {
	t.Helper()
	var err error
	f.store, err = core.Open(context.Background(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = EnsureSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f.admin = strings.TrimSpace(string(b))
	ids := map[string]int64{}
	f.handler = server.New(f.store, server.WithRepositoryResolver(func(_ context.Context, name string) (server.RepositoryIdentity, error) {
		id, ok := ids[name]
		if !ok {
			id = int64(len(ids) + 123)
			ids[name] = id
		}
		return server.RepositoryIdentity{ID: id, FullName: name}, nil
	}))
}
func (f *collectionStoreFixture) request(t *testing.T, method, path, token, worker, body string, status int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", core.NewID("key"))
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("setup %s status=%d want=%d", method, w.Code, status)
	}
	return w
}
func (f *collectionStoreFixture) project(t *testing.T, repository string) (string, string) {
	t.Helper()
	var p, w struct {
		ID string `json:"id"`
	}
	response := f.request(t, "POST", "/api/v1/projects", f.admin, "", `{"name":"Synthetic collection","repository":"`+repository+`"}`, 200)
	if err := json.Unmarshal(response.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	response = f.request(t, "POST", "/api/v1/projects/"+p.ID+"/workers", f.admin, "", `{"name":"collector"}`, 200)
	if err := json.Unmarshal(response.Body.Bytes(), &w); err != nil {
		t.Fatal(err)
	}
	return p.ID, w.ID
}
func (f *collectionStoreFixture) scopeRequest(project, worker, token string) *http.Request {
	r := httptest.NewRequest("GET", "http://localhost/api/v1/projects/"+project+"/collections", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	return r
}
func (f *collectionStoreFixture) scope(t *testing.T, project, worker, token string) core.Scope {
	t.Helper()
	// Obtain opaque authority from a real authenticated handler boundary, never
	// construct Scope's private credential/instance/session fields in a fixture.
	var scope core.Scope
	var err error
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { scope, err = f.store.Scope(r, "read") })
	h.ServeHTTP(httptest.NewRecorder(), f.scopeRequest(project, worker, token))
	if err != nil {
		t.Fatal(err)
	}
	return scope
}
func (f *collectionStoreFixture) engine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(f.store, f.provider, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func (f *collectionStoreFixture) enqueue(t *testing.T, scope core.Scope, key string, s Selection) Receipt {
	t.Helper()
	b, err := f.engine(t).Enqueue(context.Background(), scope, key, s)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err = json.Unmarshal(b, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}
func collectionStoreCode(t *testing.T, err error, want string) {
	t.Helper()
	var e *core.Error
	if !errors.As(err, &e) || e.Code != want {
		t.Fatalf("error class=%T code=%v want=%s", err, func() string {
			if e != nil {
				return e.Code
			}
			return "non-service error"
		}(), want)
	}
}
func collectionStoreRead(t *testing.T, e *Engine, s core.Scope, id string) Operation {
	t.Helper()
	r, err := e.Read(context.Background(), s, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func collectionStoreRows(t *testing.T, f *collectionStoreFixture) string {
	t.Helper()
	// Exact row bytes for all domain records, receipts and events; diagnostics
	// report equality only and never dump stored credentials or source bodies.
	var result []string
	for _, table := range []string{"history_collections", "history_collection_pages", "history_collection_seen", "receipts", "events"} {
		rows, err := f.store.DB().Query("SELECT * FROM " + table + " ORDER BY 1,2")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			v := make([]any, len(columns))
			dest := make([]any, len(v))
			for i := range v {
				dest[i] = &v[i]
			}
			if err = rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, table+":"+string(b))
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	return strings.Join(result, "\n")
}
func collectionStorePause(t *testing.T, f *collectionStoreFixture, id string) {
	t.Helper()
	// A retained unfinished-position fixture only: no invented provider pages,
	// review history, evidence or native credit. Worker capture proof is separate.
	_, err := f.store.DB().Exec(`UPDATE history_collections SET state='paused',revision=revision+1,reason='interrupted',position_json='{"pull_index":1,"resource":"review_comments","page":3}' WHERE id=?`, id)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCollectionStoreProviderFreeReplayAndRestart(t *testing.T) {
	f := newCollectionStoreFixture(t)
	p, w := f.project(t, "owner/one")
	s := f.scope(t, p, w, f.admin)
	e := f.engine(t)
	selection := Selection{PullNumbers: []int64{47, 12}}
	b, err := e.Enqueue(context.Background(), s, "original", selection)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &receipt) != nil || json.Unmarshal(b, &fields) != nil || len(fields) != 3 || len(b) > 256 || receipt.State != "queued" || receipt.Revision != 1 {
		t.Fatal("enqueue did not return compact operation receipt")
	}
	selection.PullNumbers[0] = 99
	stored := collectionStoreRead(t, e, s, receipt.ID)
	if !reflect.DeepEqual(stored.Selection.PullNumbers, []int64{47, 12}) || stored.Position != Start() || stored.RepositoryID != 123 {
		t.Fatal("selection, repository or initial position changed")
	}
	cancel, err := e.Change(context.Background(), s, receipt.ID, "cancel", "cancel", 1)
	if err != nil {
		t.Fatal(err)
	}
	before := collectionStoreRows(t, f)
	replay, err := e.Enqueue(context.Background(), s, "original", Selection{PullNumbers: []int64{47, 12}})
	if err != nil || string(replay) != string(b) || collectionStoreRows(t, f) != before {
		t.Fatal("later state changed original enqueue replay")
	}
	_, err = e.Enqueue(context.Background(), s, "original", Selection{PullNumbers: []int64{12}})
	collectionStoreCode(t, err, "idempotency_conflict")
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	s = f.scope(t, p, w, f.admin)
	disabled, err := New(f.store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	replay, err = disabled.Enqueue(context.Background(), s, "original", Selection{PullNumbers: []int64{47, 12}})
	if err != nil || string(replay) != string(b) {
		t.Fatal("disabled reader restart lost original receipt")
	}
	replay, err = disabled.Change(context.Background(), s, receipt.ID, "cancel", "cancel", 1)
	if err != nil || string(replay) != string(cancel) || collectionStoreRows(t, f) != before {
		t.Fatal("cancel receipt was not durable exact replay")
	}
	_, err = disabled.Enqueue(context.Background(), s, "new", Selection{PullNumbers: []int64{47}})
	collectionStoreCode(t, err, "collection_unavailable")
	if len(f.provider.calls) != 0 {
		t.Fatal("local storage operation invoked provider")
	}
}

func TestCollectionStoreAuthorityIsCurrentAndExplicit(t *testing.T) {
	f := newCollectionStoreFixture(t)
	p, w := f.project(t, "owner/one")
	other, ow := f.project(t, "owner/two")
	e := f.engine(t)
	admin := f.scope(t, p, w, f.admin)
	receipt := f.enqueue(t, admin, "original", Selection{PullNumbers: []int64{47}})
	for _, worker := range []string{"", ow} {
		r := f.scopeRequest(p, worker, f.admin)
		s, err := f.store.Scope(r, "read")
		if worker == ow {
			collectionStoreCode(t, err, "not_found")
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.Enqueue(context.Background(), s, "no-worker", Selection{PullNumbers: []int64{47}})
		collectionStoreCode(t, err, "worker_required")
	}
	noProject, err := f.store.AdminScope(f.scopeRequest("", "", f.admin))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.Enqueue(context.Background(), noProject, "no-project", Selection{PullNumbers: []int64{47}})
	collectionStoreCode(t, err, "worker_required")
	_, err = e.Read(context.Background(), f.scope(t, other, ow, f.admin), receipt.ID)
	collectionStoreCode(t, err, "not_found")
	for _, write := range []bool{false, true} {
		token := core.NewID("test_credential")
		sum := sha256.Sum256([]byte(token))
		permissions := `["read"]`
		if write {
			permissions = `["read","write"]`
		}
		response := f.request(t, "POST", "/api/v1/projects/"+p+"/credentials", f.admin, "", `{"token_sha256":"`+hex.EncodeToString(sum[:])+`","permissions":`+permissions+`}`, 200)
		var grant struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &grant); err != nil {
			t.Fatal(err)
		}
		ordinary := f.scope(t, p, w, token)
		_, err = e.Enqueue(context.Background(), ordinary, core.NewID("key"), Selection{PullNumbers: []int64{47}})
		collectionStoreCode(t, err, "forbidden")
		_, err = e.Read(context.Background(), ordinary, receipt.ID)
		collectionStoreCode(t, err, "forbidden")
		f.request(t, "DELETE", "/api/v1/projects/"+p+"/credentials/"+grant.ID, f.admin, "", `{}`, 200)
		_, err = e.Enqueue(context.Background(), ordinary, core.NewID("key"), Selection{PullNumbers: []int64{47}})
		collectionStoreCode(t, err, "unauthorized")
		_, err = e.Read(context.Background(), ordinary, receipt.ID)
		collectionStoreCode(t, err, "unauthorized")
	}
	session := strings.Repeat("b", 32)
	response := f.request(t, "POST", "/api/v1/projects/"+p+"/browser-session", f.admin, "", `{"session_id":"`+session+`"}`, 200)
	r := f.scopeRequest(p, w, "")
	r.Header.Del("Authorization")
	r.Header.Set("X-FC-Browser-Session", session)
	for _, cookie := range response.Result().Cookies() {
		r.AddCookie(cookie)
	}
	browser, err := f.store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	before := collectionStoreRows(t, f)
	_, err = e.Enqueue(context.Background(), browser, "browser", Selection{PullNumbers: []int64{47}})
	collectionStoreCode(t, err, "forbidden")
	if before != collectionStoreRows(t, f) || len(f.provider.calls) != 0 {
		t.Fatal("rejected authority changed collection state or called provider")
	}
}

func TestCollectionStoreGuardedCancelAndResume(t *testing.T) {
	f := newCollectionStoreFixture(t)
	p, w := f.project(t, "owner/one")
	s := f.scope(t, p, w, f.admin)
	e := f.engine(t)
	receipt := f.enqueue(t, s, "enqueue", Selection{PullNumbers: []int64{47, 12}})
	before := collectionStoreRows(t, f)
	_, err := e.Change(context.Background(), s, receipt.ID, "premature", "resume", 1)
	collectionStoreCode(t, err, "revision_conflict")
	_, err = e.Change(context.Background(), s, receipt.ID, "bad-action", "retry", 1)
	collectionStoreCode(t, err, "validation_failed")
	if collectionStoreRows(t, f) != before {
		t.Fatal("invalid lifecycle action changed state")
	}
	collectionStorePause(t, f, receipt.ID)
	paused := collectionStoreRead(t, e, s, receipt.ID)
	before = collectionStoreRows(t, f)
	_, err = e.Change(context.Background(), s, receipt.ID, "stale", "resume", 1)
	collectionStoreCode(t, err, "revision_conflict")
	disabled, err := New(f.store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = disabled.Change(context.Background(), s, receipt.ID, "disabled", "resume", 2)
	collectionStoreCode(t, err, "collection_unavailable")
	if collectionStoreRows(t, f) != before {
		t.Fatal("failed resume changed retained position")
	}
	resumed, err := e.Change(context.Background(), s, receipt.ID, "resume", "resume", 2)
	if err != nil {
		t.Fatal(err)
	}
	current := collectionStoreRead(t, e, s, receipt.ID)
	if current.State != "queued" || current.Revision != 3 || current.PassNumber != 2 || current.Position != (Position{PullIndex: 1, Resource: "review_comments", Page: 1}) || !reflect.DeepEqual(current.Selection, paused.Selection) || current.PageCount != paused.PageCount || current.RecordCount != paused.RecordCount {
		t.Fatal("resume did not preserve selection and restart unfinished resource")
	}
	canceled, err := e.Change(context.Background(), s, receipt.ID, "cancel", "cancel", 3)
	if err != nil {
		t.Fatal(err)
	}
	final := collectionStoreRead(t, e, s, receipt.ID)
	if final.State != "canceled" || final.Revision != 4 || final.Position != current.Position || final.PassNumber != 2 || !reflect.DeepEqual(final.Selection, current.Selection) {
		t.Fatal("cancel erased retained operation state")
	}
	before = collectionStoreRows(t, f)
	replay, err := e.Change(context.Background(), s, receipt.ID, "resume", "resume", 2)
	if err != nil || string(replay) != string(resumed) {
		t.Fatal("resume replay used later lifecycle state")
	}
	replay, err = e.Change(context.Background(), s, receipt.ID, "cancel", "cancel", 3)
	if err != nil || string(replay) != string(canceled) {
		t.Fatal("cancel replay failed")
	}
	_, err = e.Change(context.Background(), s, receipt.ID, "terminal", "resume", 4)
	collectionStoreCode(t, err, "revision_conflict")
	if collectionStoreRows(t, f) != before || len(f.provider.calls) != 0 {
		t.Fatal("lifecycle replay or refusal changed durable rows/provider")
	}
}

func TestCollectionStoreResumeRequiresInitiatingWorker(t *testing.T) {
	f := newCollectionStoreFixture(t)
	project, worker := f.project(t, "owner/resume-binding")
	original := f.scope(t, project, worker, f.admin)
	response := f.request(t, "POST", "/api/v1/projects/"+project+"/workers", f.admin, "", `{"name":"second authorized worker"}`, 200)
	var second struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	other := f.scope(t, project, second.ID, f.admin)
	e := f.engine(t)
	receipt := f.enqueue(t, original, "binding-enqueue", Selection{PullNumbers: []int64{47, 12}})
	collectionStorePause(t, f, receipt.ID)
	paused := collectionStoreRead(t, e, other, receipt.ID)
	before := collectionStoreRows(t, f)
	_, err := e.Change(t.Context(), other, receipt.ID, "binding-resume", "resume", paused.Revision)
	collectionStoreCode(t, err, "collection_binding_changed")
	var refusal *core.Error
	if !errors.As(err, &refusal) || refusal.Status != 409 {
		t.Fatal("cross-binding resume did not return conflict")
	}
	if collectionStoreRows(t, f) != before {
		t.Fatal("cross-binding resume changed operation, receipt or event rows")
	}
	if _, err = e.Change(t.Context(), original, receipt.ID, "binding-resume", "resume", paused.Revision); err != nil {
		t.Fatal("initiating binding could not resume after refused attempt", err)
	}
	resumed := collectionStoreRead(t, e, original, receipt.ID)
	if resumed.State != "queued" || resumed.Revision != paused.Revision+1 || resumed.PassNumber != paused.PassNumber+1 || resumed.Position != (Position{PullIndex: 1, Resource: "review_comments", Page: 1}) || !reflect.DeepEqual(resumed.Selection, paused.Selection) {
		t.Fatal("initiating binding did not resume its selected unfinished resource")
	}
	if len(f.provider.calls) != 0 {
		t.Fatal("resume binding checks invoked provider")
	}
}

func TestCollectionStorePendingCapacityIncludesResume(t *testing.T) {
	for _, global := range []bool{false, true} {
		name := "project"
		if global {
			name = "global"
		}
		t.Run(name, func(t *testing.T) {
			f := newCollectionStoreFixture(t)
			e := f.engine(t)
			p, w := f.project(t, "owner/paused")
			s := f.scope(t, p, w, f.admin)
			paused := f.enqueue(t, s, "paused", Selection{PullNumbers: []int64{47, 12}})
			collectionStorePause(t, f, paused.ID)
			var queued Receipt
			var queueScope core.Scope
			projects := 1
			if global {
				projects = 4
			}
			for project := 0; project < projects; project++ {
				qs := s
				if global {
					qp, qw := f.project(t, fmt.Sprintf("owner/capacity%d", project))
					qs = f.scope(t, qp, qw, f.admin)
				}
				for i := 0; i < 16; i++ {
					queued = f.enqueue(t, qs, fmt.Sprintf("queue-%d-%d", project, i), Selection{PullNumbers: []int64{47}})
				}
				queueScope = qs
			}
			before := collectionStoreRows(t, f)
			_, err := e.Enqueue(context.Background(), s, "overflow", Selection{PullNumbers: []int64{47}})
			collectionStoreCode(t, err, "overloaded")
			_, err = e.Change(context.Background(), s, paused.ID, "resume", "resume", 2)
			collectionStoreCode(t, err, "overloaded")
			if collectionStoreRows(t, f) != before {
				t.Fatal("capacity refusal changed durable rows")
			}
			if _, err = e.Change(context.Background(), queueScope, queued.ID, "free-slot", "cancel", 1); err != nil {
				t.Fatal(err)
			}
			if _, err = e.Change(context.Background(), s, paused.ID, "resume", "resume", 2); err != nil {
				t.Fatal(err)
			}
			if collectionStoreRead(t, e, s, paused.ID).PassNumber != 2 || len(f.provider.calls) != 0 {
				t.Fatal("released capacity did not permit local resume")
			}
		})
	}
}

func TestCollectionStoreEventFailureRollsBackMutationAndReceipt(t *testing.T) {
	f := newCollectionStoreFixture(t)
	p, w := f.project(t, "owner/one")
	s := f.scope(t, p, w, f.admin)
	e := f.engine(t)
	receipt := f.enqueue(t, s, "existing", Selection{PullNumbers: []int64{47, 12}})
	_, err := f.store.DB().Exec(`CREATE TRIGGER collection_test_event_failure BEFORE INSERT ON events WHEN NEW.event_type IN ('history.collection_created','history.collection_changed') BEGIN SELECT RAISE(ABORT,'controlled collection event failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	before := collectionStoreRows(t, f)
	_, err = e.Enqueue(context.Background(), s, "enqueue-event-fails", Selection{PullNumbers: []int64{12}})
	collectionStoreCode(t, err, "storage_unavailable")
	_, err = e.Change(context.Background(), s, receipt.ID, "cancel-event-fails", "cancel", 1)
	collectionStoreCode(t, err, "storage_unavailable")
	if collectionStoreRows(t, f) != before {
		t.Fatal("event failure leaked operation, event or receipt")
	}
	collectionStorePause(t, f, receipt.ID)
	before = collectionStoreRows(t, f)
	_, err = e.Change(context.Background(), s, receipt.ID, "resume-event-fails", "resume", 2)
	collectionStoreCode(t, err, "storage_unavailable")
	if collectionStoreRows(t, f) != before {
		t.Fatal("resume event failure advanced retained pass")
	}
	if _, err = f.store.DB().Exec(`DROP TRIGGER collection_test_event_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Change(context.Background(), s, receipt.ID, "resume-event-fails", "resume", 2); err != nil {
		t.Fatal("failed mutation incorrectly occupied idempotency key")
	}
	if len(f.provider.calls) != 0 {
		t.Fatal("storage rollback invoked provider")
	}
}
