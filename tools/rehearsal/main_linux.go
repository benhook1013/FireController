//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/benhook1013/FireController/internal/backup"
	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/historycollect"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/presentation"
	"github.com/benhook1013/FireController/internal/projectmap"
	"github.com/benhook1013/FireController/internal/publicsite"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Set only from the exact qualified source at build time.
var revision string

type tableProof struct {
	Rows   int    `json:"rows"`
	SHA256 string `json:"sha256"`
}

// proofCell preserves SQLite storage classes; untagged JSON conflates BLOB/TEXT
// and INTEGER/REAL values. Reject unknown driver types rather than weaken proof.
type proofCell struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

func taggedCells(values []any) ([]proofCell, error) {
	out := make([]proofCell, len(values))
	for i, value := range values {
		var kind string
		switch value.(type) {
		case nil:
			kind = "null"
		case int64:
			kind = "integer"
		case float64:
			kind = "real"
		case string:
			kind = "text"
		case []byte:
			kind = "blob"
		default:
			return nil, fmt.Errorf("unsupported SQLite proof value type %T", value)
		}
		out[i] = proofCell{kind, value}
	}
	return out, nil
}

func snapshot(ctx context.Context, db *sql.DB) (map[string]tableProof, error) {
	rows, e := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if e != nil {
		return nil, e
	}
	var names []string
	for rows.Next() {
		var n string
		if e = rows.Scan(&n); e != nil {
			rows.Close()
			return nil, e
		}
		names = append(names, n)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := map[string]tableProof{}
	for _, n := range names {
		proof, e := snapshotTable(ctx, db, n)
		if e != nil {
			return nil, e
		}
		out[n] = proof
	}
	return out, nil
}
func same(a, b map[string]tableProof) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || v != w {
			return false
		}
	}
	return true
}
func schemas(ctx context.Context, s *core.Store) error {
	for _, f := range []func(context.Context, *core.Store) error{backup.StartupGuard, jobs.EnsureSchema, projectmap.EnsureSchema, presentation.EnsureSchema, inbox.EnsureSchema, coordimport.EnsureSchema, reviews.EnsureSchema, publicsite.EnsureSchema, reviewexec.EnsureSchema, reviewexec.EnsureEvidenceSchema, reviewexec.EnsureAdjudicationSchema, reviewexec.EnsurePolicySchema, reviewexec.SeedNotificationFollowers, backup.EnsureManagedSchema, historycollect.EnsureSchema} {
		if e := f(ctx, s); e != nil {
			return e
		}
	}
	return nil
}
func rehearse(ctx context.Context, dir string) (map[string]tableProof, error) {
	abs, e := filepath.Abs(dir)
	if e != nil {
		return nil, e
	}
	resolved, e := filepath.EvalSymlinks(abs)
	if e != nil || resolved != abs {
		return nil, errors.New("source path unavailable or aliased")
	}
	p := filepath.Join(abs, "firecontroller.sqlite")
	st, e := os.Lstat(p)
	if e != nil || !st.Mode().IsRegular() {
		return nil, errors.New("existing regular source database required")
	}
	u := url.URL{Scheme: "file", Path: p}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	var integrity string
	e = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity)
	if e != nil || integrity != "ok" {
		db.Close()
		return nil, errors.New("source integrity failed")
	}
	before, e := snapshot(ctx, db)
	db.Close()
	if e != nil || len(before) == 0 {
		return nil, errors.New("source snapshot failed")
	}
	s, e := core.Open(ctx, abs)
	if e != nil {
		return nil, e
	}
	defer s.Close()
	if e = schemas(ctx, s); e != nil {
		return nil, e
	}
	after, e := snapshot(ctx, s.DB())
	if e != nil {
		return nil, e
	}
	if !same(before, after) {
		return nil, errors.New("table set, counts or logical content changed")
	}
	return after, nil
}
func main() {
	dir := flag.String("data-dir", "/work", "isolated copied source directory")
	report := flag.String("report", "/reports/rehearsal.json", "new report path")
	leasePath := flag.String("lease-file", "", "existing host-bound artifact lease")
	leaseDevice := flag.Uint64("lease-device", 0, "expected host lease device")
	leaseInode := flag.Uint64("lease-inode", 0, "expected host lease inode")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected arguments")
		os.Exit(2)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(revision) {
		fmt.Fprintln(os.Stderr, "qualified source revision required")
		os.Exit(2)
	}
	lease, err := acquireLease(*leasePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rehearsal lease unavailable")
		os.Exit(1)
	}
	defer lease.Close()
	if err := requireLeaseIdentity(lease, *leaseDevice, *leaseInode); err != nil {
		fmt.Fprintln(os.Stderr, "rehearsal lease identity differs")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	proof, e := rehearse(ctx, *dir)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	body, e := json.Marshal(map[string]any{"state": "passed", "source_revision": revision, "harness": "candidate-specific-schema-init-v1", "existing_table_counts_unchanged": true, "existing_table_logical_digests_unchanged": true, "provider_workers_constructed": false, "tables": proof})
	if e != nil {
		panic(e)
	}
	f, e := os.OpenFile(*report, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	_, e = f.Write(append(body, '\n'))
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
