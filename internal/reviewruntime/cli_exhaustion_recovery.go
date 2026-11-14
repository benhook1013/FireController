package reviewruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const MaxCLIExhaustionArtifactBytes = 32 << 10

// CLIExhaustionStore is independent of executables, credentials and worktrees.
// The root is opened lazily: absence is ordinary missing recovery source.
type CLIExhaustionStore struct{ root, boundary string }

func NewCLIExhaustionStore(root, managedRoot string) (*CLIExhaustionStore, error) {
	if root == "" || managedRoot == "" {
		return nil, configurationError()
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Resolve the longest existing parent, retaining lazy creation of the rest.
	// Resolving only the immediate parent misses symlinks above missing dirs.
	parent := filepath.Dir(absolute)
	missing := []string{filepath.Base(absolute)}
	for {
		resolved, resolveErr := filepath.EvalSymlinks(parent)
		if resolveErr == nil {
			absolute = resolved
			for i := len(missing) - 1; i >= 0; i-- {
				absolute = filepath.Join(absolute, missing[i])
			}
			break
		}
		if !errors.Is(resolveErr, os.ErrNotExist) || filepath.Dir(parent) == parent {
			return nil, resolveErr
		}
		missing = append(missing, filepath.Base(parent))
		parent = filepath.Dir(parent)
	}
	// The configured data directory is the stable custody boundary. Its own
	// provisioning/durability belongs to the caller, not this artifact store.
	boundary, err := filepath.EvalSymlinks(managedRoot)
	if err != nil {
		return nil, err
	}
	boundary, err = filepath.Abs(boundary)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(boundary)
	relative, relErr := filepath.Rel(boundary, absolute)
	if err != nil || !info.IsDir() || relErr != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, configurationError()
	}
	return &CLIExhaustionStore{root: absolute, boundary: boundary}, nil
}

type cliExhaustionEnvelope struct {
	Version int                              `json:"version"`
	Bytes   int                              `json:"bytes"`
	SHA256  string                           `json:"sha256"`
	Record  reviewexec.CLIExhaustionRecovery `json:"record"`
}

func exhaustionEnvelope(r reviewexec.CLIExhaustionRecovery) ([]byte, error) {
	if err := reviewexec.ValidateCLIExhaustionRecovery(r); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	data, err := json.Marshal(cliExhaustionEnvelope{1, len(payload), hex.EncodeToString(sum[:]), r})
	if err != nil {
		return nil, err
	}
	if len(data) > MaxCLIExhaustionArtifactBytes {
		return nil, captureError("validation_failed")
	}
	return data, nil
}

// DecodeCLIExhaustionArtifact is read-only and can validate backup inventory
// before any journal row exists. Same-user tampering is outside the store model.
func DecodeCLIExhaustionArtifact(attemptID string, data []byte) (reviewexec.CLIExhaustionRecovery, error) {
	var e cliExhaustionEnvelope
	if len(data) > MaxCLIExhaustionArtifactBytes || strictjson.Validate(data) != nil || json.Unmarshal(data, &e) != nil || e.Version != 1 || e.Record.Attempt.ID != attemptID {
		return e.Record, captureError("validation_failed")
	}
	canonical, err := exhaustionEnvelope(e.Record)
	if err != nil || !bytes.Equal(data, canonical) {
		return e.Record, captureError("validation_failed")
	}
	return e.Record, nil
}
func (s *CLIExhaustionStore) LoadCLIExhaustion(ctx context.Context, id string) (reviewexec.CLIExhaustionRecovery, bool, error) {
	var empty reviewexec.CLIExhaustionRecovery
	if ctx.Err() != nil {
		return empty, false, ctx.Err()
	}
	if !validIdentifier(id) {
		return empty, false, captureError("validation_failed")
	}
	info, err := os.Lstat(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !captureMode(info, 0700) || captureDirectories(s.root, false) != nil {
		return empty, false, captureError("capture_unavailable")
	}
	path := filepath.Join(s.root, id+".json")
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, false, nil
	}
	if err != nil {
		return empty, false, err
	}
	if !privateRegular(info) || !captureMode(info, 0600) || info.Size() > MaxCLIExhaustionArtifactBytes {
		return empty, false, captureError("validation_failed")
	}
	data, _, err := readPrivateFile(path, MaxCLIExhaustionArtifactBytes)
	if err != nil {
		return empty, false, err
	}
	if ctx.Err() != nil {
		return empty, false, ctx.Err()
	}
	after, err := os.Lstat(path)
	if err != nil {
		return empty, false, err
	}
	if !os.SameFile(info, after) {
		return empty, false, captureError("capture_unavailable")
	}
	r, err := DecodeCLIExhaustionArtifact(id, data)
	return r, err == nil, err
}
func (s *CLIExhaustionStore) Save(ctx context.Context, r reviewexec.CLIExhaustionRecovery) error {
	data, err := exhaustionEnvelope(r)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if prepareCLIExhaustionDirectories(s.root, s.boundary, syncCaptureDirectory) != nil {
		return captureError("capture_unavailable")
	}
	final := filepath.Join(s.root, r.Attempt.ID+".json")
	reconcile := func() error {
		old, found, err := s.LoadCLIExhaustion(ctx, r.Attempt.ID)
		if err != nil {
			return err
		}
		if !found {
			return captureError("capture_unavailable")
		}
		prior, err := exhaustionEnvelope(old)
		if err != nil || !bytes.Equal(prior, data) {
			return captureError("capture_conflict")
		}
		return syncCaptureDirectory(s.root)
	}
	if _, err := os.Lstat(final); err == nil {
		return reconcile()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(s.root, ".exhaustion-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	writeErr := temp.Chmod(0600)
	if writeErr == nil {
		_, writeErr = temp.Write(data)
	}
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	closeErr := temp.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if captureDirectories(s.root, false) != nil {
		return captureError("capture_unavailable")
	}
	if err = os.Link(temp.Name(), final); errors.Is(err, os.ErrExist) {
		return reconcile()
	} else if err != nil {
		return err
	}
	return syncCaptureDirectory(s.root)
}

// prepareCLIExhaustionDirectories durably reconciles the recovery directory chain.
func prepareCLIExhaustionDirectories(root, boundary string, syncParent func(string) error) error {
	if root == "" || !filepath.IsAbs(root) || boundary == "" || !filepath.IsAbs(boundary) {
		return errors.New("directory")
	}
	relative, err := filepath.Rel(boundary, root)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("directory")
	}
	info, err := os.Lstat(boundary)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("directory")
	}
	var chain []string
	for path := root; path != boundary; path = filepath.Dir(path) {
		chain = append(chain, path)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		path := chain[i]
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(path)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || i == 0 && !captureMode(info, 0700) {
			return errors.New("directory")
		}
		// Reconcile every directory name on write, including existing ones:
		// an earlier or concurrent creator may have failed its parent sync.
		if err = syncParent(filepath.Dir(path)); err != nil {
			return err
		}
	}
	return nil
}
