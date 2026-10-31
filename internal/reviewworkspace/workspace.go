// Package reviewworkspace materializes pinned local Git objects for one attempt.
// Shared cache provisioning belongs to the caller; Prune retires terminal checkouts.
package reviewworkspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const materializationBudget = 30 * time.Second
const patchLimit = 32 << 20

var (
	errConfiguration = errors.New("review workspace configuration is invalid")
	errInput         = errors.New("review workspace identity is invalid")
	errConflict      = errors.New("review workspace already exists")
	errMaterialize   = errors.New("review workspace materialization failed")
	errVerification  = errors.New("review workspace verification failed")
)

// Manager owns its private workspace layout and a resolved operator Git executable.
type Manager struct {
	pruning   atomic.Bool
	root      string
	git       string
	env       []string
	fetchGate chan struct{}
}

// Workspace identifies the exact source and comparison prepared for an attempt.
type Workspace struct {
	Directory           string
	ComparisonRef       string
	CandidateSHA        string
	BaseSHA             string
	MergeBaseSHA        string
	PatchSHA256         string
	RepositoryCanonical string
}

// New creates or validates the private owned layout. It never provisions a cache.
func New(root, gitExecutable string) (*Manager, error) {
	if root == "" || gitExecutable == "" {
		return nil, errConfiguration
	}
	resolved, err := exec.LookPath(gitExecutable)
	if err != nil {
		return nil, errConfiguration
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, errConfiguration
	}
	absolute, err := filepath.Abs(root)
	if err != nil || privateDirectory(absolute, true) != nil {
		return nil, errConfiguration
	}
	for _, child := range []string{"repositories", "workspaces"} {
		if privateDirectory(filepath.Join(absolute, child), true) != nil {
			return nil, errConfiguration
		}
	}
	env := []string{}
	for _, key := range []string{"PATH", "SystemRoot", "TMP", "TEMP", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0", "LANG=C.UTF-8")
	return &Manager{root: absolute, git: resolved, env: env, fetchGate: make(chan struct{}, 1)}, nil
}

// CachePath computes the server-owned cache name without touching the filesystem.
func (m *Manager) CachePath(repositoryID int64) (string, error) {
	if repositoryID <= 0 {
		return "", errInput
	}
	return filepath.Join(m.root, "repositories", strconv.FormatInt(repositoryID, 10)+".git"), nil
}

// Prepare creates one new attempt. Terminal attempts are reclaimed by Prune;
// an existing path is never reused, overwritten or removed.
func (m *Manager) Prepare(ctx context.Context, repositoryID int64, attemptID, candidateSHA, baseSHA string) (Workspace, error) {
	if !validID(attemptID) || !validSHA(candidateSHA) || !validSHA(baseSHA) {
		return Workspace{}, errInput
	}
	cache, err := m.CachePath(repositoryID)
	if err != nil {
		return Workspace{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, materializationBudget)
	defer cancel()
	if ctx.Err() != nil {
		return Workspace{}, errMaterialize
	}
	if m.layout() != nil || privateDirectory(cache, false) != nil {
		return Workspace{}, errMaterialize
	}
	gitDir := "--git-dir=" + cache
	bare, err := m.output(ctx, cache, gitDir, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return Workspace{}, errMaterialize
	}
	for _, sha := range []string{candidateSHA, baseSHA} {
		commit, err := m.output(ctx, cache, gitDir, "rev-parse", "--verify", sha+"^{commit}")
		if err != nil || commit != sha {
			return Workspace{}, errMaterialize
		}
	}
	directory := filepath.Join(m.root, "workspaces", attemptID)
	if err := os.Mkdir(directory, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Workspace{}, errConflict
		}
		return Workspace{}, errMaterialize
	}
	if _, err := m.output(ctx, m.root, "clone", "--shared", "--no-checkout", "--template=", "--", cache, directory); err != nil {
		return Workspace{}, errMaterialize
	}
	w := Workspace{Directory: directory, ComparisonRef: "refs/firecontroller/review-base/" + attemptID, CandidateSHA: candidateSHA, BaseSHA: baseSHA}
	if _, err := m.output(ctx, directory, "switch", "--detach", candidateSHA); err != nil {
		return Workspace{}, errMaterialize
	}
	if _, err := m.output(ctx, directory, "update-ref", w.ComparisonRef, baseSHA); err != nil {
		return Workspace{}, errMaterialize
	}
	w.MergeBaseSHA, err = m.output(ctx, directory, "merge-base", baseSHA, candidateSHA)
	if err != nil || !validSHA(w.MergeBaseSHA) {
		return Workspace{}, errMaterialize
	}
	w.PatchSHA256, err = m.patch(ctx, w)
	if err != nil || m.verify(ctx, w) != nil {
		return Workspace{}, errMaterialize
	}
	return w, nil
}

// Verify rejects changed source, comparison, patch, tracked or untracked content.
func (m *Manager) Verify(ctx context.Context, w Workspace) error {
	ctx, cancel := context.WithTimeout(ctx, materializationBudget)
	defer cancel()
	if m.verify(ctx, w) != nil {
		return errVerification
	}
	return nil
}

func (m *Manager) verify(ctx context.Context, w Workspace) error {
	id := filepath.Base(w.Directory)
	if !validID(id) || w.Directory != filepath.Join(m.root, "workspaces", id) ||
		w.ComparisonRef != "refs/firecontroller/review-base/"+id || !validSHA(w.CandidateSHA) || !validSHA(w.BaseSHA) || !validSHA(w.MergeBaseSHA) || !lowerHex(w.PatchSHA256, 64) ||
		ctx.Err() != nil || m.layout() != nil || privateDirectory(w.Directory, false) != nil {
		return errVerification
	}
	// A fresh local clone must keep its own metadata directory; reject redirected Git metadata.
	metadata, err := os.Lstat(filepath.Join(w.Directory, ".git"))
	if err != nil || !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 {
		return errVerification
	}
	if w.RepositoryCanonical != "" {
		if !validCanonicalRepository(w.RepositoryCanonical) {
			return errVerification
		}
		origin, err := m.output(ctx, w.Directory, "remote", "get-url", "origin")
		if err != nil || origin != "https://github.com/"+w.RepositoryCanonical+".git" {
			return errVerification
		}
	}
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "--verify", "HEAD"}, w.CandidateSHA},
		{[]string{"rev-parse", "--verify", w.ComparisonRef}, w.BaseSHA},
		{[]string{"merge-base", w.BaseSHA, w.CandidateSHA}, w.MergeBaseSHA},
		{[]string{"status", "--porcelain=v1", "--untracked-files=all", "--ignored", "-z"}, ""},
	} {
		value, err := m.output(ctx, w.Directory, check.args...)
		if err != nil || value != check.want {
			return errVerification
		}
	}
	// Detached HEAD is part of the source contract, even if an attached branch matches.
	if _, err := m.output(ctx, w.Directory, "symbolic-ref", "-q", "HEAD"); err == nil {
		return errVerification
	}
	patch, err := m.patch(ctx, w)
	if err != nil || patch != w.PatchSHA256 {
		return errVerification
	}
	return nil
}

func (m *Manager) patch(ctx context.Context, w Workspace) (string, error) {
	hash := sha256.New()
	err := m.run(ctx, w.Directory, hash, patchLimit, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--binary", "--full-index", "--no-renames", "--diff-algorithm=myers", "--src-prefix=a/", "--dst-prefix=b/", w.MergeBaseSHA, w.CandidateSHA, "--")
	if err != nil {
		return "", errMaterialize
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (m *Manager) output(ctx context.Context, directory string, args ...string) (string, error) {
	var output bytes.Buffer
	if err := m.run(ctx, directory, &output, 64<<10, args...); err != nil {
		return "", errMaterialize
	}
	return strings.TrimSuffix(output.String(), "\n"), nil
}

func (m *Manager) run(ctx context.Context, directory string, stdout io.Writer, limit int64, args ...string) error {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fixed := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.autocrlf=false", "-c", "core.fsmonitor=false"}
	command := exec.CommandContext(commandCtx, m.git, append(fixed, args...)...)
	command.Dir, command.Env, command.WaitDelay = directory, m.env, time.Second
	command.Stdout = &boundedWriter{writer: stdout, remaining: limit, cancel: cancel}
	command.Stderr = &boundedWriter{writer: io.Discard, remaining: 64 << 10, cancel: cancel}
	if command.Run() != nil || commandCtx.Err() != nil {
		return errMaterialize
	}
	return nil
}

type boundedWriter struct {
	writer    io.Writer
	remaining int64
	cancel    context.CancelFunc
}

func (w *boundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		w.cancel()
		return 0, errMaterialize
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}

func (m *Manager) layout() error {
	for _, path := range []string{m.root, filepath.Join(m.root, "repositories"), filepath.Join(m.root, "workspaces")} {
		if privateDirectory(path, false) != nil {
			return errConfiguration
		}
	}
	return nil
}

func privateDirectory(path string, create bool) error {
	// Operator-selected ancestors can be shared, but none may redirect the owned
	// layout through a symlink. Permissions are checked on the owned directory.
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) || err != nil && !errors.Is(err, os.ErrNotExist) {
			return errConfiguration
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && create {
		parent := filepath.Dir(path)
		if parent != path {
			if _, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
				if privateDirectory(parent, true) != nil {
					return errConfiguration
				}
			}
		}
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return errConfiguration
		}
		info, err = os.Lstat(path)
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return errConfiguration
	}
	return nil
}

func validID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, c := range []byte(value) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validSHA(value string) bool { return lowerHex(value, 40) }

func lowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, c := range []byte(value) {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
