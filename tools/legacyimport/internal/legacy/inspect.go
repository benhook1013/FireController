package legacy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

func limitsWithDefaults(l Limits) (Limits, error) {
	defaults := DefaultLimits()
	fields := []*int64{&l.MaxDatabaseBytes, &l.MaxRowsPerTable, &l.MaxTotalRows, &l.MaxCellBytes, &l.MaxTotalCellBytes}
	values := []int64{defaults.MaxDatabaseBytes, defaults.MaxRowsPerTable, defaults.MaxTotalRows, defaults.MaxCellBytes, defaults.MaxTotalCellBytes}
	for i, p := range fields {
		if *p < 0 || *p == math.MaxInt64 {
			return Limits{}, failure("limits_invalid")
		}
		if *p == 0 {
			*p = values[i]
		}
	}
	return l, nil
}
func privateRegular(info os.FileInfo) bool {
	return info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
}
func noSidecars(path string) bool {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}
func fileDigest(ctx context.Context, file *os.File, maximum int64) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", failure("snapshot_unavailable")
	}
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var size int64
	for {
		if ctx.Err() != nil {
			return "", failure("inspection_interrupted")
		}
		n, err := file.Read(buf)
		if n > 0 {
			if int64(n) > maximum-size {
				return "", failure("snapshot_too_large")
			}
			size += int64(n)
			h.Write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", failure("snapshot_unavailable")
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// InspectSnapshot reads an explicitly selected completed standalone export,
// never the live controller path. The caller supplies that sealed copy. File
// guards and before/after hashes detect observed changes; they cannot establish
// exporter provenance or protect against a hostile same-user replacement race.
// Only known source tables are read. Recognition/counts and selected blockers
// are not complete legacy content validation or approval to import anything.
func InspectSnapshot(ctx context.Context, path string, limits Limits) (result Inspection, err error) {
	return withInspection(ctx, path, limits, nil)
}

// The optional converter shares the same open file, SQL handle, admitted bounds
// and final conservation check as inspection. Never reopen a checked path later.
func withInspection(ctx context.Context, path string, limits Limits, selected func(*sql.DB, Inspection) error) (result Inspection, err error) {
	if ctx == nil {
		return Inspection{}, failure("snapshot_invalid")
	}
	if ctx.Err() != nil {
		return Inspection{}, failure("inspection_interrupted")
	}
	limits, err = limitsWithDefaults(limits)
	if err != nil {
		return Inspection{}, err
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return Inspection{}, failure("snapshot_invalid")
	}
	initial, err := os.Lstat(path)
	if err != nil || !privateRegular(initial) || initial.Size() <= 0 {
		return Inspection{}, failure("snapshot_invalid")
	}
	if initial.Size() > limits.MaxDatabaseBytes {
		return Inspection{}, failure("snapshot_too_large")
	}
	if !noSidecars(path) {
		return Inspection{}, failure("snapshot_unsealed")
	}
	file, err := os.Open(path)
	if err != nil {
		return Inspection{}, failure("snapshot_unavailable")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !privateRegular(opened) || !os.SameFile(initial, opened) || opened.Size() != initial.Size() {
		return Inspection{}, failure("source_changed")
	}
	original, err := fileDigest(ctx, file, limits.MaxDatabaseBytes)
	if err != nil {
		return Inspection{}, err
	}
	defer func() {
		if ctx.Err() != nil {
			result = Inspection{}
			err = failure("inspection_interrupted")
			return
		}
		current, e := os.Lstat(path)
		if e != nil || !privateRegular(current) || !os.SameFile(opened, current) || current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) || !noSidecars(path) {
			result = Inspection{}
			err = failure("source_changed")
			return
		}
		after, e := fileDigest(ctx, file, limits.MaxDatabaseBytes)
		if e != nil {
			result = Inspection{}
			err = e
			return
		}
		if after != original {
			result = Inspection{}
			err = failure("source_changed")
		}
	}()
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: url.Values{"mode": {"ro"}, "immutable": {"1"}}.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return Inspection{}, failure("snapshot_unavailable")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	domains, err := recognize(ctx, db)
	if err != nil {
		return Inspection{}, err
	}
	result = Inspection{Kind: "legacy_snapshot_inspection", SHA256: original, Bytes: opened.Size(), Counts: map[string]int64{}, Diagnostics: []Diagnostic{}, Briefs: BriefStats{NativeLimitBytes: 32 << 10}}
	var totalRows, totalBytes int64
	for _, table := range tables {
		if !domains[table.domain] {
			continue
		}
		count, e := boundedTable(ctx, db, table, limits, &totalRows, &totalBytes)
		if e != nil {
			return Inspection{}, e
		}
		result.Counts[table.name] = count
	}
	if err = readVersions(ctx, db, domains, &result); err != nil {
		return Inspection{}, err
	}
	if domains["jobs"] {
		if err = inspectJobs(ctx, db, &result); err != nil {
			return Inspection{}, err
		}
	}
	if domains["inbox"] {
		if err = inspectMessages(ctx, db, &result); err != nil {
			return Inspection{}, err
		}
	}
	if err = inspectControllerState(ctx, db, &result); err != nil {
		return Inspection{}, err
	}
	if selected != nil {
		if err = selected(db, result); err != nil {
			return Inspection{}, err
		}
	}
	return result, nil
}

func recognize(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	expected := map[string]tableSpec{}
	for _, table := range tables {
		expected[table.name] = table
	}
	rows, err := db.QueryContext(ctx, `SELECT CASE WHEN length(name)<=128 THEN name ELSE NULL END,type,substr(sql,1,256) FROM sqlite_schema WHERE type IN ('table','view') AND name NOT GLOB 'sqlite_*' LIMIT 257`)
	if err != nil {
		return nil, failure("snapshot_schema_unsupported")
	}
	found := map[string]bool{}
	for rows.Next() {
		var name, kind, prefix sql.NullString
		if err = rows.Scan(&name, &kind, &prefix); err != nil {
			break
		}
		if len(found) >= 256 {
			err = failure("snapshot_too_large")
			break
		}
		words := strings.Fields(strings.ToUpper(prefix.String))
		if !name.Valid || !kind.Valid || kind.String != "table" || len(words) < 2 || words[0] != "CREATE" || words[1] != "TABLE" {
			err = failure("snapshot_schema_unsupported")
			break
		}
		if _, ok := expected[name.String]; !ok {
			err = failure("snapshot_schema_unsupported")
			break
		}
		found[name.String] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, safeSQL(ctx, err)
	}
	if !found["controller_metadata"] || !found["review_state"] {
		return nil, failure("snapshot_schema_unsupported")
	}
	domains := map[string]bool{"controller": true}
	for name := range found {
		domains[expected[name].domain] = true
	}
	for _, table := range tables {
		if !domains[table.domain] {
			continue
		}
		if !found[table.name] {
			return nil, failure("snapshot_schema_unsupported")
		}
		columns, e := db.QueryContext(ctx, `SELECT CASE WHEN length(name)<=128 THEN name ELSE NULL END,hidden FROM pragma_table_xinfo(?) LIMIT 65`, table.name)
		if e != nil {
			return nil, failure("snapshot_schema_unsupported")
		}
		i := 0
		for columns.Next() {
			var name string
			var hidden int
			if e = columns.Scan(&name, &hidden); e != nil {
				break
			}
			if i >= len(table.columns) || name != table.columns[i] || hidden != 0 {
				e = failure("snapshot_schema_unsupported")
				break
			}
			i++
		}
		if e == nil {
			e = columns.Err()
		}
		columns.Close()
		if e != nil || i != len(table.columns) {
			return nil, failure("snapshot_schema_unsupported")
		}
	}
	return domains, nil
}
func safeSQL(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return failure("inspection_interrupted")
	}
	var own *Error
	if errors.As(err, &own) {
		return own
	}
	return failure("snapshot_schema_unsupported")
}

// Length-only reads bound every selected cell before any body is materialized.
// UTF-8 bytes are measured as BLOB length, not SQLite's character-count length.
func boundedTable(ctx context.Context, db *sql.DB, table tableSpec, l Limits, totalRows, totalBytes *int64) (int64, error) {
	expressions := make([]string, len(table.columns))
	for i, col := range table.columns {
		expressions[i] = `length(CAST("` + col + `" AS BLOB))`
	}
	rows, err := db.QueryContext(ctx, `SELECT `+strings.Join(expressions, ",")+` FROM "`+table.name+`" LIMIT ?`, l.MaxRowsPerTable+1)
	if err != nil {
		return 0, safeSQL(ctx, err)
	}
	defer rows.Close()
	lengths := make([]sql.NullInt64, len(expressions))
	targets := make([]any, len(lengths))
	for i := range lengths {
		targets[i] = &lengths[i]
	}
	var count int64
	for rows.Next() {
		count++
		if count > l.MaxRowsPerTable || *totalRows >= l.MaxTotalRows {
			return 0, failure("snapshot_too_large")
		}
		*totalRows++
		if err = rows.Scan(targets...); err != nil {
			return 0, safeSQL(ctx, err)
		}
		for _, n := range lengths {
			if n.Valid {
				if n.Int64 < 0 || n.Int64 > l.MaxCellBytes || n.Int64 > l.MaxTotalCellBytes-*totalBytes {
					return 0, failure("snapshot_too_large")
				}
				*totalBytes += n.Int64
			}
		}
	}
	return count, safeRows(ctx, rows.Err())
}
func safeRows(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	return safeSQL(ctx, err)
}

func readVersions(ctx context.Context, db *sql.DB, domains map[string]bool, result *Inspection) error {
	if result.Counts["controller_metadata"] != 1 || result.Counts["review_state"] != 1 {
		return failure("snapshot_schema_unsupported")
	}
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&result.Versions.SQLite); err != nil {
		return safeSQL(ctx, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT data_model_version,min_writer_build FROM controller_metadata WHERE singleton=1`).Scan(&result.Versions.Controller, &result.Versions.MinimumWriterBuild); err != nil {
		return safeSQL(ctx, err)
	}
	if result.Versions.SQLite != 1 || result.Versions.Controller < 1 || result.Versions.Controller > 3 || result.Versions.MinimumWriterBuild < 1 {
		return failure("snapshot_schema_unsupported")
	}
	for _, domain := range []struct {
		name, table, column string
		want                int64
		target              *int64
	}{{"jobs", "job_metadata", "jobs_schema_version", 2, &result.Versions.Jobs}, {"inbox", "inbox_metadata", "inbox_schema_version", 1, &result.Versions.Inbox}, {"map", "map_metadata", "map_schema_version", 1, &result.Versions.Map}, {"reviews", "review_records_metadata", "records_schema_version", 9, &result.Versions.Reviews}} {
		if !domains[domain.name] {
			continue
		}
		if result.Counts[domain.table] != 1 {
			return failure("snapshot_schema_unsupported")
		}
		if err := db.QueryRowContext(ctx, `SELECT `+domain.column+` FROM `+domain.table+` WHERE singleton=1`).Scan(domain.target); err != nil {
			return safeSQL(ctx, err)
		}
		if *domain.target != domain.want {
			return failure("snapshot_schema_unsupported")
		}
		if domain.name != "inbox" {
			var physical, model int64
			if err := db.QueryRowContext(ctx, `SELECT controller_schema_version,controller_data_model_version FROM `+domain.table+` WHERE singleton=1`).Scan(&physical, &model); err != nil {
				return safeSQL(ctx, err)
			}
			if physical != 1 || model < 1 || model > result.Versions.Controller {
				return failure("snapshot_schema_unsupported")
			}
		}
	}
	return nil
}
