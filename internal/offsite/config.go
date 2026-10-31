// Package offsite transfers verified backup bundles through an operator-owned
// rclone connection. It never opens the serving database or starts providers.
package offsite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
)

var bundleID = regexp.MustCompile(`^backup_[0-9a-f]{32}$`)
var instanceID = regexp.MustCompile(`^instance_[0-9a-f]{32}$`)
var hashID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var remoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*:[A-Za-z0-9][A-Za-z0-9/_-]*$`)

const maxStateBytes = 1 << 20
const maxOwned = 128

type Config struct {
	Version        int    `json:"version"`
	SourceRoot     string `json:"source_root"`
	InstanceID     string `json:"instance_id"`
	StateDir       string `json:"state_dir"`
	RcloneBinary   string `json:"rclone_binary"`
	RcloneConfig   string `json:"rclone_config"`
	Remote         string `json:"remote"`
	Keep           int    `json:"keep"`
	Timeout        string `json:"timeout"`
	MaxBundleBytes int64  `json:"max_bundle_bytes"`
}

func failure(code string) error {
	return core.Fail(code, "The offsite backup operation could not complete. Inspect its local status before retrying; existing verified backups remain separate.", 503)
}

func absolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsAny(path, "\x00\r\n")
}

func privateDir(path string, create bool) error {
	if !absolute(path) {
		return failure("offsite_configuration_invalid")
	}
	if create {
		if err := os.MkdirAll(path, 0700); err != nil {
			return failure("offsite_storage_unavailable")
		}
	}
	s, err := os.Lstat(path)
	if err != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && s.Mode().Perm()&0077 != 0 {
		return failure("offsite_configuration_invalid")
	}
	return nil
}

func privateRead(path string, limit int64) ([]byte, error) {
	s, err := os.Lstat(path)
	if err != nil || !s.Mode().IsRegular() || s.Size() > limit || runtime.GOOS != "windows" && s.Mode().Perm()&0077 != 0 {
		return nil, failure("offsite_configuration_invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, failure("offsite_storage_unavailable")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(s, opened) {
		return nil, failure("offsite_configuration_invalid")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, failure("offsite_storage_unavailable")
	}
	return b, nil
}

func loadConfig(path string) (Config, error) {
	var c Config
	b, err := privateRead(path, 16<<10)
	if err != nil {
		return c, err
	}
	if strictjson.Decode(b, &c) != nil || c.Version != 1 || !instanceID.MatchString(c.InstanceID) || !absolute(c.SourceRoot) || !absolute(c.StateDir) || !absolute(c.RcloneBinary) || !absolute(c.RcloneConfig) || !remoteName.MatchString(c.Remote) || strings.Contains(c.Remote, "//") || c.Keep < 1 || c.Keep > 100 || c.MaxBundleBytes < 1024 || c.MaxBundleBytes > 1<<40 {
		return c, failure("offsite_configuration_invalid")
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d <= 0 || d > 24*time.Hour {
		return c, failure("offsite_configuration_invalid")
	}
	// Resolve existing parent aliases, including macOS's /var -> /private/var.
	a, z := resolvePrefix(c.SourceRoot), resolvePrefix(c.StateDir)
	if a == z || strings.HasPrefix(a, z+string(filepath.Separator)) || strings.HasPrefix(z, a+string(filepath.Separator)) {
		return c, failure("offsite_configuration_invalid")
	}
	return c, nil
}

func resolvePrefix(p string) string {
	var tail []string
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				r = filepath.Join(r, tail[i])
			}
			return r
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		tail = append(tail, filepath.Base(p))
		p = parent
	}
}

func binding(c Config) string {
	b, _ := json.Marshal([]string{c.InstanceID, c.SourceRoot, c.Remote, c.RcloneConfig, c.RcloneBinary})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func writeJSON(path string, value any) error {
	b, err := json.Marshal(value)
	if err != nil || len(b) > maxStateBytes {
		return failure("offsite_state_invalid")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return failure("offsite_storage_unavailable")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		f.Close()
		return failure("offsite_storage_unavailable")
	}
	if f.Sync() != nil || f.Close() != nil {
		return failure("offsite_storage_unavailable")
	}
	if os.Rename(f.Name(), path) != nil {
		return failure("offsite_storage_unavailable")
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return failure("offsite_storage_unavailable")
	}
	defer f.Close()
	if err = f.Sync(); err != nil && runtime.GOOS != "windows" {
		return failure("offsite_storage_unavailable")
	}
	return nil
}

func fileHash(ctx context.Context, path string, limit int64) (string, int64, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || runtime.GOOS != "windows" && before.Mode().Perm()&0077 != 0 {
		return "", 0, failure("offsite_archive_invalid")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, failure("offsite_storage_unavailable")
	}
	defer f.Close()
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() || !os.SameFile(before, s) || s.Size() > limit {
		return "", 0, failure("offsite_archive_invalid")
	}
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx: ctx, r: io.LimitReader(f, limit+1)})
	if err != nil || n > limit || n != s.Size() {
		return "", 0, failure("offsite_archive_invalid")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type Receipt struct {
	Version            int    `json:"version"`
	ID                 string `json:"id"`
	InstanceID         string `json:"instance_id"`
	SnapshotFinishedAt string `json:"snapshot_finished_at"`
	EventSequence      int64  `json:"event_sequence"`
	ArchiveSHA256      string `json:"archive_sha256"`
	ArchiveBytes       int64  `json:"archive_bytes"`
}

func validReceipt(r Receipt, c Config) bool {
	t, err := time.Parse(time.RFC3339Nano, r.SnapshotFinishedAt)
	return r.Version == 1 && bundleID.MatchString(r.ID) && r.InstanceID == c.InstanceID && err == nil && !t.IsZero() && r.SnapshotFinishedAt == t.UTC().Format(time.RFC3339Nano) && r.EventSequence >= 0 && hashID.MatchString(r.ArchiveSHA256) && r.ArchiveBytes > 0 && r.ArchiveBytes <= c.MaxBundleBytes
}

type Attempt struct {
	State      string `json:"state"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Reason     string `json:"reason"`
}
type Status struct {
	Version         int       `json:"version"`
	Binding         string    `json:"binding"`
	LastAttempt     *Attempt  `json:"last_attempt,omitempty"`
	LastSuccess     *Receipt  `json:"last_success,omitempty"`
	Pending         *Receipt  `json:"pending,omitempty"`
	Owned           []Receipt `json:"owned"`
	RetentionReason string    `json:"retention_reason"`
}

func readState(c Config) (Status, error) {
	s := Status{Version: 1, Binding: binding(c), Owned: []Receipt{}}
	p := filepath.Join(c.StateDir, "offsite.json")
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	b, err := privateRead(p, maxStateBytes)
	if err != nil {
		return s, err
	}
	// Defaults are for an absent file only. A present file must explicitly
	// carry its version, binding and ownership inventory.
	s = Status{}
	if strictjson.Decode(b, &s) != nil || s.Version != 1 || s.Binding != binding(c) || s.Owned == nil || len(s.Owned) > maxOwned {
		return s, failure("offsite_state_invalid")
	}
	seen := map[string]bool{}
	for _, r := range s.Owned {
		if !validReceipt(r, c) || seen[r.ID] {
			return s, failure("offsite_state_invalid")
		}
		seen[r.ID] = true
	}
	if s.Pending != nil && (!validReceipt(*s.Pending, c) || seen[s.Pending.ID]) {
		return s, failure("offsite_state_invalid")
	}
	if s.LastSuccess != nil {
		found := false
		for _, r := range s.Owned {
			found = found || r == *s.LastSuccess
		}
		if !found {
			return s, failure("offsite_state_invalid")
		}
	}
	return s, nil
}

type Report struct {
	Version         int      `json:"version"`
	LastAttempt     *Attempt `json:"last_attempt"`
	LastSuccess     *Receipt `json:"last_success"`
	Pending         *Receipt `json:"pending"`
	Retained        int      `json:"retained"`
	RetentionReason string   `json:"retention_reason"`
}

func ReadStatus(ctx context.Context, configPath string) (Report, error) {
	if ctx == nil || ctx.Err() != nil {
		return Report{}, failure("offsite_cancelled")
	}
	c, err := loadConfig(configPath)
	if err != nil {
		return Report{}, err
	}
	s, err := readState(c)
	if err != nil {
		return Report{}, err
	}
	return Report{Version: 1, LastAttempt: s.LastAttempt, LastSuccess: s.LastSuccess, Pending: s.Pending, Retained: len(s.Owned), RetentionReason: s.RetentionReason}, nil
}
