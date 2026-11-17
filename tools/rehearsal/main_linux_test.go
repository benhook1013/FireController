//go:build linux

package main

import (
	"context"
	"github.com/benhook1013/FireController/internal/core"
	"os"
	"path/filepath"
	"testing"
)

func TestPopulatedRehearsal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	s, e := core.Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = schemas(ctx, s); e != nil {
		t.Fatal(e)
	}
	_, e = s.DB().ExecContext(ctx, `CREATE TABLE rehearsal_fixture (id INTEGER, value BLOB); INSERT INTO rehearsal_fixture VALUES(1,x'010203'),(2,NULL),(3,x'')`)
	if e != nil {
		t.Fatal(e)
	}
	s.Close()
	p, e := rehearse(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	if p["rehearsal_fixture"].Rows != 3 {
		t.Fatal(p)
	}
}
func TestMissingSourceRefused(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if _, e := rehearse(context.Background(), dir); e == nil {
		t.Fatal("accepted missing")
	}
	if _, e := os.Stat(filepath.Join(dir, "firecontroller.sqlite")); !os.IsNotExist(e) {
		t.Fatal("created source")
	}
}
func TestChangedProofRefused(t *testing.T) {
	a := map[string]tableProof{"x": {1, "a"}}
	for _, b := range []map[string]tableProof{{"x": {1, "b"}}, {"x": {2, "a"}}, {"y": {1, "a"}}, {}} {
		if same(a, b) {
			t.Fatal("accepted mismatch")
		}
	}
}

func TestSnapshotPreservesStorageClasses(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := core.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	db := s.DB()
	if _, err = db.ExecContext(ctx, "CREATE TABLE typed(value)"); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"X'010203'", "'AQID'"}, {"1", "1.0"}, {"NULL", "X''"}, {"X''", "''"}} {
		var proofs [2]map[string]tableProof
		for i, value := range pair {
			if _, err = db.ExecContext(ctx, "DELETE FROM typed; INSERT INTO typed VALUES("+value+")"); err != nil {
				t.Fatal(err)
			}
			proofs[i], err = snapshot(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
		}
		if same(proofs[0], proofs[1]) {
			t.Fatalf("storage class change hidden: %v", pair)
		}
	}
}
func TestProofRejectsUnknownDriverType(t *testing.T) {
	if _, err := taggedCells([]any{true}); err == nil {
		t.Fatal("accepted unknown driver type")
	}
}
