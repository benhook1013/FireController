package adoptionholds_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	m "github.com/benhook1013/FireController/internal/adoptionholdsmodel"
)

func TestOrdinaryWriterResolvesButCannotImportAndRevokedReplayRefuses(t *testing.T) {
	f := fresh(t)
	p := f.preview(batch())
	f.apply(batch(), "seed")
	id := f.page("?status=all").Items[0].ID
	token := "ordinary-adoption-writer"
	sum := sha256.Sum256([]byte(token))
	f.exec(`INSERT INTO credentials VALUES('ordinary_writer','ordinary_principal',?,0,0)`, hex.EncodeToString(sum[:]))
	f.exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('ordinary_writer','project_one',1,1)`)
	request := httptest.NewRequest("POST", base, nil)
	request.SetPathValue("project_id", "project_one")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-FC-Worker", "worker_one")
	scope, err := f.store.Scope(request, "write")
	if err != nil {
		t.Fatal(err)
	}
	decode[map[string]any](t, f.call("POST", base+"/preview", token, "worker_one", "", batch()), 403)
	decode[map[string]any](t, f.call("POST", base+"/apply", token, "worker_one", "cannot-import", m.ApplyRequest{Batch: batch(), PreviewSHA256: p.PreviewSHA256}), 403)
	one := int64(1)
	resolution := m.ResolveRequest{ExpectedRevision: &one, Reason: "Verified correction and counting effects", Evidence: []string{"commit:verified"}}
	path := base + "/" + id + "/resolve"
	// Invalid attribution and foreign project writers cannot resolve a hold.
	decode[map[string]any](t, f.call("POST", path, token, "", "missing-worker", resolution), 403)
	decode[map[string]any](t, f.call("POST", path, token, "worker_two", "foreign-worker", resolution), 404)
	foreignToken := "foreign-adoption-writer"
	foreignSum := sha256.Sum256([]byte(foreignToken))
	f.exec(`INSERT INTO credentials VALUES('foreign_writer','foreign_principal',?,0,0)`, hex.EncodeToString(foreignSum[:]))
	f.exec(`INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES('foreign_writer','project_two',1,1)`)
	decode[map[string]any](t, f.call("POST", path, foreignToken, "worker_one", "foreign-project", resolution), 403)
	first := f.call("POST", path, token, "worker_one", "resolve", resolution)
	decode[map[string]any](t, first, 200)
	detail := decode[m.Detail](t, f.call("GET", base+"/"+id, token, "", "", nil), 200)
	if detail.Status != "resolved" || detail.Resolution == nil || detail.Resolution.ActorID != "ordinary_principal" || detail.Resolution.WorkerID != "worker_one" {
		t.Fatal("writer resolution attribution missing", detail)
	}
	if got := f.call("POST", path, token, "worker_one", "resolve", resolution); got.Code != 200 || got.Body.String() != first.Body.String() {
		t.Fatal("writer replay lost original result", got.Body.String())
	}
	// A cookie created while the credential was read-only cannot acquire write
	// capability when its project grant later permits writes.
	f.exec(`UPDATE grants SET can_write=0 WHERE credential_id='ordinary_writer'`)
	login := httptest.NewRequest("POST", "http://localhost"+base, nil)
	login.SetPathValue("project_id", "project_one")
	login.Header.Set("Authorization", "Bearer "+token)
	login.Header.Set("X-FC-Instance", f.store.InstanceID())
	browserID := strings.Repeat("c", 32)
	_, cookie, err := f.store.CreateBrowserSession(login, browserID)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE grants SET can_write=1 WHERE credential_id='ordinary_writer'`)
	r := httptest.NewRequest("POST", "http://localhost"+path, strings.NewReader(`{"expected_revision":1,"reason":"Verified correction","evidence":["commit:proof"]}`))
	r.Header.Set("X-FC-Browser-Session", browserID)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("X-FC-Worker", "worker_one")
	r.Header.Set("Idempotency-Key", "cookie-denied")
	r.AddCookie(cookie)
	response := httptest.NewRecorder()
	f.mux.ServeHTTP(response, r)
	decode[map[string]any](t, response, 403)
	before := f.count("receipts")
	f.exec(`UPDATE grants SET can_write=0 WHERE credential_id='ordinary_writer'`)
	decode[map[string]any](t, f.call("POST", path, token, "worker_one", "resolve", resolution), 403)
	if _, err = f.service.Resolve(t.Context(), scope, id, "resolve", resolution); err == nil {
		t.Fatal("cached writer scope replay bypassed revoked write grant")
	}
	if _, err = f.service.Resolve(t.Context(), scope, id, "new-after-revocation", resolution); err == nil {
		t.Fatal("cached writer scope mutated after write grant revocation")
	}
	f.exec(`UPDATE credentials SET revoked=1 WHERE id='ordinary_writer'`)
	decode[map[string]any](t, f.call("POST", path, token, "worker_one", "resolve", resolution), 401)
	if _, err = f.service.Resolve(t.Context(), scope, id, "resolve", resolution); err == nil {
		t.Fatal("cached scope replay bypassed credential revocation")
	}
	if f.count("receipts") != before || f.count("review_adoption_resolutions") != 1 {
		t.Fatal("refused mutations wrote state")
	}
	f.validate()
}
