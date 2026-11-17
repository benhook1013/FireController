//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMultisetDigestOrderMultiplicityColumnsAndCancellation(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "fixture.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(q string) {
		t.Helper()
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE x(a,b); INSERT INTO x VALUES(1,'a'),(2,x'00'),(1,'a'),(NULL,1.0)`)
	proof := func() tableProof {
		t.Helper()
		p, err := snapshotTable(context.Background(), db, "x")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	before := proof()
	exec(`DELETE FROM x; INSERT INTO x VALUES(NULL,1.0),(1,'a'),(1,'a'),(2,x'00')`)
	if got := proof(); got != before {
		t.Fatal("row order changed multiset proof")
	}
	exec(`DELETE FROM x WHERE rowid=(SELECT min(rowid) FROM x WHERE a=1)`)
	if got := proof(); got == before || got.Rows != 3 {
		t.Fatal("lost duplicate hidden")
	}
	exec(`INSERT INTO x VALUES(1,'a'); ALTER TABLE x RENAME COLUMN b TO renamed`)
	if got := proof(); got == before {
		t.Fatal("column identity hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := snapshotTable(ctx, db, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

// Opt-in realistic payload fixture: run the compiled test under the same 512MiB
// cgroup as deployment. A successful unit run alone is not a memory-bound proof.
func TestLargePayloadSnapshot(t *testing.T) {
	if os.Getenv("FC_REHEARSAL_LARGE_TEST") != "1" {
		t.Skip("explicit resource qualification")
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "large.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE payload(id INTEGER, body BLOB)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO payload VALUES(?,zeroblob(1048576))`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 450; i++ {
		if _, err = stmt.Exec(i); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	before, err := snapshotTable(context.Background(), db, "payload")
	if err != nil {
		t.Fatal(err)
	}
	if before.Rows != 450 {
		t.Fatal(before.Rows)
	}
	after, err := snapshotTable(context.Background(), db, "payload")
	if err != nil || before != after {
		t.Fatalf("repeated proof differs: %v", err)
	}
	if _, err = db.Exec(`UPDATE payload SET body=x'00' WHERE id=449`); err != nil {
		t.Fatal(err)
	}
	changed, err := snapshotTable(context.Background(), db, "payload")
	if err != nil || changed == before {
		t.Fatalf("mutation hidden: %v", err)
	}
}

func TestManySmallRowsSnapshot(t *testing.T) {
	if os.Getenv("FC_REHEARSAL_LARGE_TEST") != "1" {
		t.Skip("explicit resource qualification")
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "many.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE small(id INTEGER,body BLOB); WITH RECURSIVE n(v) AS (VALUES(1) UNION ALL SELECT v+1 FROM n WHERE v<250000) INSERT INTO small SELECT v,zeroblob(16) FROM n`); err != nil {
		t.Fatal(err)
	}
	p, err := snapshotTable(context.Background(), db, "small")
	if err != nil || p.Rows != 250000 {
		t.Fatalf("proof %+v: %v", p, err)
	}
}
