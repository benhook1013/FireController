package reviewexec

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviews"
)

// Build the current empty schema once per test process. Running every
// historical migration for every fixture dominates the race suite's runtime.
var httpFixtureSchema = sync.OnceValues(func() ([]byte, error) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "firecontroller-test-schema-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	store, err := core.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	err = reviews.EnsureSchema(ctx, store)
	if err == nil {
		err = EnsureSchema(ctx, store)
	}
	if err = errors.Join(err, store.Close()); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "firecontroller.sqlite"))
})

func openHTTPFixtureStore(t *testing.T, dir string) *core.Store {
	t.Helper()
	schema, err := httpFixtureSchema()
	if err != nil {
		t.Fatal("build fixture schema", err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "firecontroller.sqlite")
	if err := os.WriteFile(path, schema, 0600); err != nil {
		t.Fatal(err)
	}

	// Every copied fixture gets independent instance, signing and bootstrap
	// identities before application code can open it.
	token := core.NewID("fc_admin")
	tokenHash := sha256.Sum256([]byte(token))
	signingKey := sha256.Sum256([]byte(core.NewID("fixture_signing")))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`UPDATE metadata SET value=CASE key WHEN 'instance_id' THEN ? ELSE ? END WHERE key IN ('instance_id','signing_key')`,
		core.NewID("instance"), hex.EncodeToString(signingKey[:]))
	if err == nil {
		_, err = db.Exec(`UPDATE credentials SET id=?,principal_id=?,token_sha256=? WHERE is_admin=1`,
			core.NewID("credential"), core.NewID("principal"), hex.EncodeToString(tokenHash[:]))
	}
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal("initialize fixture identity", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bootstrap-admin.token"), []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal("open fixture store", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
