package historycollect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func TestProjectManagerCollectionActorAndReplayRecheck(t *testing.T) {
	f := newCollectionStoreFixture(t)
	p, w := f.project(t, "selected/repository")
	token := "synthetic-history-project-manager"
	sum := sha256.Sum256([]byte(token))
	response := f.request(t, "POST", "/api/v1/projects/"+p+"/credentials", f.admin, w, `{"token_sha256":"`+hex.EncodeToString(sum[:])+`","permissions":["manage"]}`, 200)
	var receipt struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(response.Body.Bytes(), &receipt) != nil {
		t.Fatal("invalid grant")
	}
	e := f.engine(t)
	scope := f.scope(t, p, w, token)
	selection := Selection{PullNumbers: []int64{2}}
	body, err := e.Enqueue(context.Background(), scope, "owner-history", selection)
	if err != nil {
		t.Fatal("project manager collection refused", err)
	}
	var operation Receipt
	if json.Unmarshal(body, &operation) != nil {
		t.Fatal("bad operation receipt")
	}
	if _, err := e.Read(t.Context(), scope, operation.ID); err != nil {
		t.Fatal("manager read refused", err)
	}
	retained := core.Scope{ProjectID: p, PrincipalID: scope.PrincipalID, WorkerID: w}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkActor(t.Context(), tx, retained); err != nil {
		t.Fatal("retained manager actor refused", err)
	}
	_ = tx.Rollback()
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_manage=0 WHERE credential_id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.Enqueue(t.Context(), scope, "owner-history", selection)
	collectionStoreCode(t, err, "forbidden")
	_, err = e.Read(t.Context(), scope, operation.ID)
	collectionStoreCode(t, err, "forbidden")
	tx, err = f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = checkActor(t.Context(), tx, retained)
	_ = tx.Rollback()
	collectionStoreCode(t, err, "forbidden")
}
