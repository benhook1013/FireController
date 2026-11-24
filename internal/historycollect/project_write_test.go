package historycollect

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func collectionGrant(t *testing.T, f *collectionStoreFixture, p, w, permissions string) (token, id string) {
	t.Helper()
	token = core.NewID("history_test_credential")
	sum := sha256.Sum256([]byte(token))
	response := f.request(t, "POST", "/api/v1/projects/"+p+"/credentials", f.admin, w, `{"token_sha256":"`+hex.EncodeToString(sum[:])+`","permissions":`+permissions+`}`, 200)
	var receipt struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(response.Body.Bytes(), &receipt) != nil || receipt.ID == "" {
		t.Fatal("invalid grant")
	}
	return token, receipt.ID
}

func TestProjectWriterCollectionTransactionsAndReplayRecheck(t *testing.T) {
	f := newCollectionStoreFixture(t)
	p, w := f.project(t, "selected/repository")
	token, grant := collectionGrant(t, f, p, w, `["read","write"]`)
	e := f.engine(t)
	scope := f.scope(t, p, w, token)
	selection := Selection{PullNumbers: []int64{47, 12}}
	body, err := e.Enqueue(t.Context(), scope, "writer-history", selection)
	if err != nil {
		t.Fatal("project writer collection refused", err)
	}
	var operation Receipt
	if json.Unmarshal(body, &operation) != nil {
		t.Fatal("bad operation receipt")
	}
	collectionAdvance(t, e)
	current := collectionStoreRead(t, e, scope, operation.ID)
	pages, err := e.Pages(t.Context(), scope, operation.ID, 5, "")
	if err != nil || len(pages.Items) != 1 {
		t.Fatal("writer page list refused or background capture missing", err)
	}
	page := pages.Items[0].ID
	if _, err := e.Capture(t.Context(), scope, operation.ID, page); err != nil {
		t.Fatal("writer capture refused", err)
	}
	if _, err := e.Change(t.Context(), scope, operation.ID, "writer-cancel", "cancel", current.Revision); err != nil {
		t.Fatal("writer cancel refused", err)
	}
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id=?`, grant); err != nil {
		t.Fatal(err)
	}
	before := collectionStoreRows(t, f)
	_, err = e.Enqueue(t.Context(), scope, "writer-history", selection)
	collectionStoreCode(t, err, "forbidden")
	_, err = e.Enqueue(t.Context(), scope, "new-writer-history", selection)
	collectionStoreCode(t, err, "forbidden")
	_, err = e.Change(t.Context(), scope, operation.ID, "writer-cancel", "cancel", current.Revision)
	collectionStoreCode(t, err, "forbidden")
	_, err = e.Change(t.Context(), scope, operation.ID, "new-writer-resume", "resume", current.Revision)
	collectionStoreCode(t, err, "forbidden")
	_, err = e.Read(t.Context(), scope, operation.ID)
	collectionStoreCode(t, err, "forbidden")
	_, err = e.Pages(t.Context(), scope, operation.ID, 5, "")
	collectionStoreCode(t, err, "forbidden")
	_, err = e.Capture(t.Context(), scope, operation.ID, page)
	collectionStoreCode(t, err, "forbidden")
	if before != collectionStoreRows(t, f) || len(f.provider.calls) != 2 {
		t.Fatal("denied writer requests changed retained rows or invoked provider work")
	}
}
