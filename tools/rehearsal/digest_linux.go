//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// snapshotTable hashes a deterministic multiset of typed rows, retaining only a
// single row in Go. A private disk-backed index orders fixed-size row digests;
// multiplicities preserve duplicate rows. Like the final proof, row identity
// relies on SHA-256 collision resistance. No source values enter the scratch DB.
func snapshotTable(ctx context.Context, db *sql.DB, name string) (tableProof, error) {
	var proof tableProof
	dir, err := os.MkdirTemp("", "fc-rehearsal-digests-")
	if err != nil {
		return proof, err
	}
	defer os.RemoveAll(dir)
	scratch, err := sql.Open("sqlite", filepath.Join(dir, "digests.sqlite"))
	if err != nil {
		return proof, err
	}
	defer scratch.Close()
	scratch.SetMaxOpenConns(1)
	// The private index is disposable, not recovery data. Bound its page cache and
	// keep sorting/index pages on disk, independent of database payload size.
	if _, err = scratch.ExecContext(ctx, `PRAGMA page_size=4096; PRAGMA max_page_count=65536; PRAGMA journal_mode=OFF; PRAGMA synchronous=OFF; PRAGMA cache_size=-1024; PRAGMA temp_store=FILE; PRAGMA mmap_size=0; CREATE TABLE digests (digest BLOB PRIMARY KEY, multiplicity INTEGER NOT NULL) WITHOUT ROWID`); err != nil {
		return proof, err
	}
	tx, err := scratch.BeginTx(ctx, nil)
	if err != nil {
		return proof, err
	}
	defer tx.Rollback()
	insert, err := tx.PrepareContext(ctx, `INSERT INTO digests VALUES(?,1) ON CONFLICT(digest) DO UPDATE SET multiplicity=multiplicity+1`)
	if err != nil {
		return proof, err
	}
	defer insert.Close()
	rows, err := db.QueryContext(ctx, `SELECT * FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`)
	if err != nil {
		return proof, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return proof, err
	}
	values, pointers := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err = ctx.Err(); err != nil {
			return proof, err
		}
		if err = rows.Scan(pointers...); err != nil {
			return proof, err
		}
		cells, err := taggedCells(values)
		if err != nil {
			return proof, err
		}
		encoded, err := json.Marshal(cells)
		if err != nil {
			return proof, err
		}
		digest := sha256.Sum256(encoded)
		if _, err = insert.ExecContext(ctx, digest[:]); err != nil {
			return proof, err
		}
		proof.Rows++
	}
	if err = rows.Err(); err != nil {
		return proof, err
	}
	if err = rows.Close(); err != nil {
		return proof, err
	}
	if err = insert.Close(); err != nil {
		return proof, err
	}
	if err = tx.Commit(); err != nil {
		return proof, err
	}
	ordered, err := scratch.QueryContext(ctx, `SELECT digest,multiplicity FROM digests ORDER BY digest`)
	if err != nil {
		return proof, err
	}
	defer ordered.Close()
	h := sha256.New()
	h.Write([]byte("fc-typed-row-multiset-v2\x00"))
	schema, err := json.Marshal(columns)
	if err != nil {
		return proof, err
	}
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(schema)))
	h.Write(count[:])
	h.Write(schema)
	for ordered.Next() {
		if err = ctx.Err(); err != nil {
			return proof, err
		}
		var digest []byte
		var multiplicity int64
		if err = ordered.Scan(&digest, &multiplicity); err != nil {
			return proof, err
		}
		h.Write(digest)
		binary.BigEndian.PutUint64(count[:], uint64(multiplicity))
		h.Write(count[:])
	}
	if err = ordered.Err(); err != nil {
		return proof, err
	}
	if err = ctx.Err(); err != nil {
		return proof, err
	}
	proof.SHA256 = hex.EncodeToString(h.Sum(nil))
	return proof, nil
}
