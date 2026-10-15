package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// privateDirectory rejects symlink components; only the selected directory must
// be owner-private (ancestors such as /tmp need not be). No hostile same-user
// filesystem-race protection is claimed.
func privateDirectory(path string, create bool) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return invalid()
	}
	var chain []string
	for p := absolute; ; p = filepath.Dir(p) {
		chain = append(chain, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		p := chain[i]
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) && create {
			err = os.Mkdir(p, 0700)
			if err != nil {
				return unavailable()
			}
			info, err = os.Lstat(p)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return invalid()
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return invalid()
	}
	return nil
}
func privateFile(path string) (os.FileInfo, error) {
	if err := privateDirectory(filepath.Dir(path), false); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, invalid()
	}
	return info, nil
}
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return unavailable()
	}
	defer f.Close()
	if err = f.Sync(); err != nil && runtime.GOOS != "windows" {
		return unavailable()
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := checkContext(r.ctx); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func hashPrivate(ctx context.Context, path string, limit int64) (Artifact, error) {
	if checkContext(ctx) != nil || limit < 1 {
		return Artifact{}, unavailable()
	}
	before, err := privateFile(path)
	if err != nil || before.Size() < 0 || before.Size() > limit {
		return Artifact{}, invalid()
	}
	f, err := os.Open(path)
	if err != nil {
		return Artifact{}, unavailable()
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return Artifact{}, invalid()
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, contextReader{ctx, io.LimitReader(f, before.Size())}, make([]byte, 64<<10))
	if err != nil {
		return Artifact{}, unavailable()
	}
	after, err := privateFile(path)
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() || n != before.Size() || checkContext(ctx) != nil {
		return Artifact{}, invalid()
	}
	return Artifact{Path: filepath.Base(path), SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: n}, nil
}

// copyPrivate creates a new 0600 file, never replacing a destination. Path in
// the result is its basename; inventory callers set the manifest-relative path.
func copyPrivate(ctx context.Context, source, destination string, limit int64) (Artifact, error) {
	if checkContext(ctx) != nil {
		return Artifact{}, unavailable()
	}
	if limit < 1 {
		return Artifact{}, invalid()
	}
	before, err := privateFile(source)
	if err != nil || before.Size() < 0 || before.Size() > limit {
		return Artifact{}, invalid()
	}
	if privateDirectory(filepath.Dir(destination), false) != nil {
		return Artifact{}, invalid()
	}
	in, err := os.Open(source)
	if err != nil {
		return Artifact{}, unavailable()
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return Artifact{}, invalid()
	}
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return Artifact{}, exists()
	}
	if err != nil {
		return Artifact{}, unavailable()
	}
	complete := false
	defer func() {
		out.Close()
		if !complete {
			os.Remove(destination)
		}
	}()
	h := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(out, h), contextReader{ctx, io.LimitReader(in, before.Size())}, make([]byte, 64<<10))
	if err != nil || n != before.Size() {
		return Artifact{}, unavailable()
	}
	after, err := privateFile(source)
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() || checkContext(ctx) != nil {
		return Artifact{}, invalid()
	}
	if out.Sync() != nil || out.Close() != nil {
		return Artifact{}, unavailable()
	}
	complete = true
	return Artifact{Path: filepath.Base(destination), SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: n}, nil
}
func writePrivate(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return exists()
	}
	if err != nil {
		return unavailable()
	}
	_, werr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	if werr != nil || syncErr != nil || closeErr != nil {
		return unavailable()
	}
	return nil
}
func openDatabase(ctx context.Context, path string, immutable bool) (*sql.DB, error) {
	if checkContext(ctx) != nil {
		return nil, unavailable()
	}
	info, err := privateFile(path)
	if err != nil || info.Size() <= 0 {
		return nil, invalid()
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, invalid()
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	q := url.Values{"mode": {"ro"}, "_pragma": {"foreign_keys(1)", "busy_timeout(0)"}}
	if immutable {
		q.Set("immutable", "1")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, unavailable()
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, invalid()
	}
	return db, nil
}
func openSnapshot(ctx context.Context, path string) (*sql.DB, error) {
	return openDatabase(ctx, path, true)
}
func canonicalManifest(m Manifest) ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil || len(b)+1 > maxManifestBytes {
		return nil, invalid()
	}
	return append(b, '\n'), nil
}
func validManifest(m Manifest) bool {
	start, ok := canonicalTime(m.SnapshotStartedAt)
	end, eok := canonicalTime(m.SnapshotFinishedAt)
	if m.Version != 1 || !backupID.MatchString(m.ID) || !instanceID.MatchString(m.SourceInstanceID) || !ok || !eok || end.Before(start) || m.EventSequence < 0 || m.Database.Path != "firecontroller.sqlite" || !digest.MatchString(m.Database.SHA256) || m.Database.Bytes <= 0 || m.Artifacts == nil || len(m.Artifacts) > maxArtifacts {
		return false
	}
	previous := ""
	var total int64
	for _, a := range m.Artifacts {
		_, limit, ok := artifactPath(a.Path)
		if !ok || a.Path <= previous || !digest.MatchString(a.SHA256) || a.Bytes < 0 || a.Bytes > limit || total > math.MaxInt64-a.Bytes {
			return false
		}
		if strings.HasPrefix(a.Path, "hosted-review-captures/") && !strings.HasSuffix(a.Path, "-"+a.SHA256+".json") {
			return false
		}
		previous = a.Path
		total += a.Bytes
	}
	return true
}
func readBounded(ctx context.Context, path string, limit int64) ([]byte, error) {
	info, err := privateFile(path)
	if err != nil || info.Size() < 0 || info.Size() > limit {
		return nil, invalid()
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, unavailable()
	}
	defer f.Close()
	data, err := io.ReadAll(contextReader{ctx, io.LimitReader(f, limit+1)})
	if err != nil {
		return nil, unavailable()
	}
	after, err := privateFile(path)
	if err != nil || !os.SameFile(info, after) || int64(len(data)) != info.Size() || int64(len(data)) > limit {
		return nil, invalid()
	}
	return data, nil
}

// inspectManifest verifies only a completed immutable bundle. The checksum file
// is the commit marker; a reserved but unfinished directory is never usable.
func inspectManifest(ctx context.Context, bundleDir string) (Manifest, error) {
	var m Manifest
	if checkContext(ctx) != nil {
		return m, unavailable()
	}
	absolute, err := filepath.Abs(bundleDir)
	if strings.TrimSpace(bundleDir) == "" || err != nil || !backupID.MatchString(filepath.Base(absolute)) || privateDirectory(absolute, false) != nil {
		return m, invalid()
	}
	sidecar, err := readBounded(ctx, filepath.Join(absolute, "manifest.sha256"), 65)
	if err != nil {
		return m, err
	}
	if len(sidecar) != 65 || sidecar[64] != '\n' || !digest.Match(sidecar[:64]) {
		return m, invalid()
	}
	body, err := readBounded(ctx, filepath.Join(absolute, "manifest.json"), maxManifestBytes)
	if err != nil {
		return m, err
	}
	hash := sha256.Sum256(body)
	if hex.EncodeToString(hash[:]) != string(sidecar[:64]) || strictjson.Decode(body, &m) != nil || !validManifest(m) || m.ID != filepath.Base(absolute) {
		return m, invalid()
	}
	canonical, err := canonicalManifest(m)
	if err != nil || !bytes.Equal(canonical, body) {
		return m, invalid()
	}
	expected := map[string]bool{"manifest.json": true, "manifest.sha256": true, "firecontroller.sqlite": true}
	for _, a := range m.Artifacts {
		expected[a.Path] = true
	}
	if err = checkBundleFiles(ctx, absolute, expected); err != nil {
		return m, err
	}
	db, err := hashPrivate(ctx, filepath.Join(absolute, m.Database.Path), maxDatabaseBytes)
	if err != nil || db.SHA256 != m.Database.SHA256 || db.Bytes != m.Database.Bytes {
		return m, invalid()
	}
	for _, a := range m.Artifacts {
		_, limit, _ := artifactPath(a.Path)
		actual, err := hashPrivate(ctx, filepath.Join(absolute, filepath.FromSlash(a.Path)), limit)
		if err != nil || actual.SHA256 != a.SHA256 || actual.Bytes != a.Bytes {
			return m, invalid()
		}
	}
	if err = validateSnapshot(ctx, absolute, m); err != nil {
		return m, err
	}
	return m, nil
}
func Inspect(ctx context.Context, bundleDir string) (Summary, error) {
	m, err := inspectManifest(ctx, bundleDir)
	if err != nil {
		return Summary{}, err
	}
	absolute, _ := filepath.Abs(bundleDir)
	return summary(m, absolute, "verified")
}
func within(path, parent string) bool {
	relative, err := filepath.Rel(parent, path)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// Create snapshots the supplied Store and inventories recognized immutable raw
// capture files. The final directory is exclusively reserved; its durable
// checksum marker is written last, making incomplete reservations unusable.
func Create(ctx context.Context, store *core.Store, dataDir, destinationRoot string) (Summary, error) {
	if checkContext(ctx) != nil || store == nil {
		return Summary{}, unavailable()
	}
	return create(ctx, store.DB(), store.InstanceID(), dataDir, destinationRoot)
}

// CreateFromDirectory opens an existing installation read-only, without Store
// initialization, migration or writer admission. SQLite reads the committed WAL
// prefix while a concurrent writer may retain an uncommitted transaction.
func CreateFromDirectory(ctx context.Context, dataDir, destinationRoot string) (Summary, error) {
	if err := ValidateSource(ctx, dataDir); err != nil {
		return Summary{}, err
	}
	db, err := openDatabase(ctx, filepath.Join(dataDir, "firecontroller.sqlite"), false)
	if err != nil {
		return Summary{}, err
	}
	defer db.Close()
	var identity string
	if db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='instance_id'`).Scan(&identity) != nil || !instanceID.MatchString(identity) {
		return Summary{}, invalid()
	}
	return create(ctx, db, identity, dataDir, destinationRoot)
}

func create(ctx context.Context, sourceDB *sql.DB, identity, dataDir, destinationRoot string) (out Summary, err error) {
	if checkContext(ctx) != nil || sourceDB == nil {
		return out, unavailable()
	}
	if strings.TrimSpace(dataDir) == "" || strings.TrimSpace(destinationRoot) == "" {
		return out, invalid()
	}
	source, err := filepath.Abs(dataDir)
	if err != nil || privateDirectory(source, false) != nil {
		return out, invalid()
	}
	destination, err := filepath.Abs(destinationRoot)
	if err != nil || within(destination, source) || within(source, destination) {
		return out, invalid()
	}
	if err = privateDirectory(destination, true); err != nil {
		return out, err
	}
	var livePath string
	rows, err := sourceDB.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return out, unavailable()
	}
	for rows.Next() {
		var sequence int
		var name, path string
		if err = rows.Scan(&sequence, &name, &path); err != nil {
			rows.Close()
			return out, unavailable()
		}
		if name == "main" {
			livePath = path
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if rowErr != nil {
		return out, unavailable()
	}
	selected, err := privateFile(filepath.Join(source, "firecontroller.sqlite"))
	if err != nil {
		return out, err
	}
	actual, err := privateFile(livePath)
	if err != nil || !os.SameFile(selected, actual) {
		return out, invalid()
	}
	if err = ValidateSource(ctx, source); err != nil {
		return out, err
	}
	id := core.NewID("backup")
	final := filepath.Join(destination, id)
	if err = os.Mkdir(final, 0700); errors.Is(err, os.ErrExist) {
		return out, exists()
	} else if err != nil {
		return out, unavailable()
	}
	owned, err := os.Lstat(final)
	if err != nil {
		return out, unavailable()
	}
	committed := false
	defer func() {
		if !committed {
			current, e := os.Lstat(final)
			if e == nil && os.SameFile(owned, current) {
				os.RemoveAll(final)
			}
		}
	}()
	m := Manifest{Version: 1, ID: id, SourceInstanceID: identity, SnapshotStartedAt: time.Now().UTC().Format(time.RFC3339Nano), Artifacts: []Artifact{}}
	if err = core.SnapshotDatabase(ctx, sourceDB, filepath.Join(final, "firecontroller.sqlite")); err != nil {
		return out, unavailable()
	}
	m.SnapshotFinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	m.Database, err = hashPrivate(ctx, filepath.Join(final, "firecontroller.sqlite"), maxDatabaseBytes)
	if err != nil {
		return out, err
	}
	m.Artifacts, err = copyInventory(ctx, source, final)
	if err != nil {
		return out, err
	}
	sort.Slice(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].Path < m.Artifacts[j].Path })
	db, err := openSnapshot(ctx, filepath.Join(final, "firecontroller.sqlite"))
	if err != nil {
		return out, err
	}
	err = db.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM events`).Scan(&m.EventSequence)
	db.Close()
	if err != nil {
		return out, invalid()
	}
	if !validManifest(m) {
		return out, invalid()
	}
	if err = validateSnapshot(ctx, final, m); err != nil {
		return out, err
	}
	body, err := canonicalManifest(m)
	if err != nil {
		return out, err
	}
	if err = writePrivate(filepath.Join(final, "manifest.json"), body); err != nil {
		return out, err
	}
	if err = syncDirectory(final); err != nil {
		return out, err
	}
	checksum := sha256.Sum256(body)
	pendingChecksum := filepath.Join(final, ".manifest.sha256.pending")
	if err = writePrivate(pendingChecksum, []byte(hex.EncodeToString(checksum[:])+"\n")); err != nil {
		return out, err
	}
	if err = os.Link(pendingChecksum, filepath.Join(final, "manifest.sha256")); err != nil {
		if errors.Is(err, os.ErrExist) {
			return out, exists()
		}
		return out, unavailable()
	}
	if err = os.Remove(pendingChecksum); err != nil {
		return out, unavailable()
	}
	if err = syncDirectory(final); err != nil {
		return out, err
	}
	if err = syncDirectory(destination); err != nil {
		return out, err
	}
	if checkContext(ctx) != nil {
		return out, unavailable()
	}
	out, err = summary(m, final, "complete")
	if err != nil {
		return out, err
	}
	committed = true
	return out, nil
}
