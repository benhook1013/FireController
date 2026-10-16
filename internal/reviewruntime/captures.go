package reviewruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxCaptureStdout = 16 << 20
const maxCaptureStderr = 64 << 10
const maxCaptureFile = 24 << 20

// CaptureReference identifies exact retained bytes, without exposing their location.
// Retaining a capture does not establish review credit.
type CaptureReference struct {
	ID     string
	SHA256 string
	Bytes  int64
}

// CaptureRecord retains the measured invocation window with exact raw output.
// StartedAt and FinishedAt describe capture observation, even if Started is false.
type CaptureRecord struct {
	Capture               reviewprocess.Capture
	StartedAt, FinishedAt time.Time
}

// CaptureStore retains immutable private CLI artifacts independently of the journal.
type CaptureStore struct{ root string }

func (*CaptureStore) String() string     { return "CaptureStore{private}" }
func (s *CaptureStore) GoString() string { return s.String() }

type captureEnvelope struct {
	Version    int         `json:"version"`
	AttemptID  string      `json:"attempt_id"`
	StartedAt  string      `json:"started_at"`
	FinishedAt string      `json:"finished_at"`
	Capture    captureWire `json:"capture"`
}

type captureWire struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Started  bool   `json:"started"`
}

// NewCaptureStore pins existing ancestor aliases to one canonical private root.
// These checks do not defend against a hostile same-user filesystem race.
func NewCaptureStore(root string) (*CaptureStore, error) {
	if root == "" {
		return nil, captureError("configuration_invalid")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, captureError("configuration_invalid")
	}
	canonical, err := canonicalCaptureRoot(absolute)
	if err != nil {
		return nil, captureError("configuration_invalid")
	}
	return &CaptureStore{root: canonical}, nil
}

func canonicalCaptureRoot(root string) (string, error) {
	// The requested root itself cannot be an alias, a file or a public directory.
	info, err := os.Lstat(root)
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !captureMode(info, 0700)) {
		return "", errors.New("directory")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Resolve only the existing parent (or deepest existing ancestor) once.
	// Newly created components are checked independently and never followed.
	parent := filepath.Dir(root)
	missing := []string{filepath.Base(root)}
	for {
		if _, err = os.Lstat(parent); err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(parent) == parent {
			return "", err
		}
		missing = append(missing, filepath.Base(parent))
		parent = filepath.Dir(parent)
	}
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	info, err = os.Lstat(canonical)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("directory")
	}
	for i := len(missing) - 1; i >= 0; i-- {
		canonical = filepath.Join(canonical, missing[i])
		if err = os.Mkdir(canonical, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err = os.Lstat(canonical)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !captureMode(info, 0700) {
			return "", errors.New("directory")
		}
	}
	return canonical, captureDirectories(canonical, false)
}

// Save publishes with an atomic no-replace hard link. An unavailable result can
// follow publication; retrying the exact same capture reconciles that artifact.
func (s *CaptureStore) Save(ctx context.Context, attemptID string, record CaptureRecord) (CaptureReference, error) {
	var empty CaptureReference
	if ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	if !validIdentifier(attemptID) || !validCapture(record.Capture) || !validCaptureTiming(record.StartedAt, record.FinishedAt) {
		return empty, captureError("validation_failed")
	}
	if s == nil || captureDirectories(s.root, false) != nil {
		return empty, captureError("capture_unavailable")
	}
	capture := record.Capture
	wire := captureWire{base64.StdEncoding.EncodeToString(capture.Stdout), base64.StdEncoding.EncodeToString(capture.Stderr), capture.ExitCode, capture.Started}
	data, err := json.Marshal(captureEnvelope{Version: 2, AttemptID: attemptID, StartedAt: record.StartedAt.UTC().Format(time.RFC3339Nano), FinishedAt: record.FinishedAt.UTC().Format(time.RFC3339Nano), Capture: wire})
	if err != nil || len(data) > maxCaptureFile {
		return empty, captureError("validation_failed")
	}
	ref := captureReference(data)
	if ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	final := filepath.Join(s.root, attemptID+".json")
	if _, err := os.Lstat(final); err == nil {
		return s.existingCapture(ctx, attemptID, data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return empty, captureError("capture_unavailable")
	}
	if ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	temp, err := os.CreateTemp(s.root, ".capture-*")
	if err != nil {
		return empty, captureError("capture_unavailable")
	}
	// Only this call's randomly created disposable name is ever removed.
	defer os.Remove(temp.Name())
	writeErr := temp.Chmod(0600)
	if writeErr == nil {
		_, writeErr = temp.Write(data)
	}
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil || captureDirectories(s.root, false) != nil || ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	if err := os.Link(temp.Name(), final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return s.existingCapture(ctx, attemptID, data)
		}
		return empty, captureError("capture_unavailable")
	}
	if syncCaptureDirectory(s.root) != nil || ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	return ref, nil
}

func (s *CaptureStore) existingCapture(ctx context.Context, attemptID string, expected []byte) (CaptureReference, error) {
	data, _, err := s.readCapture(ctx, attemptID)
	if err != nil {
		return CaptureReference{}, err
	}
	if !bytes.Equal(data, expected) {
		return CaptureReference{}, captureError("capture_conflict")
	}
	ref := captureReference(data)
	if ctx.Err() != nil {
		return CaptureReference{}, captureError("capture_unavailable")
	}
	// Reconcile a previous successful link whose directory sync was uncertain.
	if syncCaptureDirectory(s.root) != nil || ctx.Err() != nil {
		return CaptureReference{}, captureError("capture_unavailable")
	}
	return ref, nil
}

// Read validates canonical version 2 timing and returns exact captured bytes.
// Older envelopes remain untouched and unavailable; their times are not inferred.
func (s *CaptureStore) Read(ctx context.Context, attemptID string) (CaptureRecord, CaptureReference, error) {
	data, capture, err := s.readCapture(ctx, attemptID)
	if err != nil {
		return CaptureRecord{}, CaptureReference{}, err
	}
	ref := captureReference(data)
	if ctx.Err() != nil {
		return CaptureRecord{}, CaptureReference{}, captureError("capture_unavailable")
	}
	return capture, ref, nil
}

func (s *CaptureStore) readCapture(ctx context.Context, attemptID string) ([]byte, CaptureRecord, error) {
	var empty CaptureRecord
	if ctx.Err() != nil {
		return nil, empty, captureError("capture_unavailable")
	}
	if !validIdentifier(attemptID) {
		return nil, empty, captureError("validation_failed")
	}
	if s == nil || captureDirectories(s.root, false) != nil {
		return nil, empty, captureError("capture_unavailable")
	}
	path := filepath.Join(s.root, attemptID+".json")
	info, err := os.Lstat(path)
	if err != nil || !privateRegular(info) || !captureMode(info, 0600) {
		return nil, empty, captureError("capture_unavailable")
	}
	data, _, err := readPrivateFile(path, maxCaptureFile)
	var envelope captureEnvelope
	if err != nil || strictjson.Decode(data, &envelope) != nil || envelope.Version != 2 || envelope.AttemptID != attemptID || !requiredNonNullObject(data, "version", "attempt_id", "started_at", "finished_at", "capture") {
		return nil, empty, captureError("capture_unavailable")
	}
	var root map[string]json.RawMessage
	_ = json.Unmarshal(data, &root)
	if !requiredNonNullObject(root["capture"], "stdout", "stderr", "exit_code", "started") {
		return nil, empty, captureError("capture_unavailable")
	}
	stdout, outErr := base64.StdEncoding.Strict().DecodeString(envelope.Capture.Stdout)
	stderr, errErr := base64.StdEncoding.Strict().DecodeString(envelope.Capture.Stderr)
	capture := reviewprocess.Capture{Stdout: stdout, Stderr: stderr, ExitCode: envelope.Capture.ExitCode, Started: envelope.Capture.Started}
	start, startErr := time.Parse(time.RFC3339Nano, envelope.StartedAt)
	finish, finishErr := time.Parse(time.RFC3339Nano, envelope.FinishedAt)
	if outErr != nil || errErr != nil || !validCapture(capture) || startErr != nil || finishErr != nil || !validCaptureTiming(start, finish) || envelope.StartedAt != start.UTC().Format(time.RFC3339Nano) || envelope.FinishedAt != finish.UTC().Format(time.RFC3339Nano) || ctx.Err() != nil || captureDirectories(s.root, false) != nil {
		return nil, empty, captureError("capture_unavailable")
	}
	after, err := os.Lstat(path)
	if err != nil || !privateRegular(after) || !captureMode(after, 0600) || !os.SameFile(info, after) || ctx.Err() != nil {
		return nil, empty, captureError("capture_unavailable")
	}
	return data, CaptureRecord{Capture: capture, StartedAt: start.UTC(), FinishedAt: finish.UTC()}, nil
}

func validCaptureTiming(start, finish time.Time) bool {
	start, finish = start.UTC(), finish.UTC()
	return !start.IsZero() && !finish.IsZero() && start.Year() >= 1 && start.Year() <= 9999 && finish.Year() >= 1 && finish.Year() <= 9999 && !finish.Before(start)
}

func validCapture(c reviewprocess.Capture) bool {
	return len(c.Stdout) <= maxCaptureStdout && len(c.Stderr) <= maxCaptureStderr && c.ExitCode >= -1 && int64(c.ExitCode) <= 0xffffffff
}

func captureReference(data []byte) CaptureReference {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	return CaptureReference{ID: "review_capture_" + digest[:32], SHA256: digest, Bytes: int64(len(data))}
}

func captureMode(info os.FileInfo, want os.FileMode) bool {
	return runtime.GOOS == "windows" || info.Mode().Perm() == want && info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

func captureDirectories(root string, create bool) error {
	if root == "" || !filepath.IsAbs(root) {
		return errors.New("directory")
	}
	var chain []string
	for path := root; ; path = filepath.Dir(path) {
		chain = append(chain, path)
		if filepath.Dir(path) == path {
			break
		}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		path := chain[i]
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(path)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || i == 0 && !captureMode(info, 0700) {
			return errors.New("directory")
		}
	}
	return nil
}

func syncCaptureDirectory(root string) error {
	// Windows does not support directory File.Sync through os.Open.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func captureError(code string) error {
	switch code {
	case "configuration_invalid":
		return core.Fail(code, "The private capture store configuration is invalid or unavailable.", 500)
	case "validation_failed":
		return core.Fail(code, "Use a valid attempt identifier and bounded capture values.", 400)
	case "capture_conflict":
		return core.Fail(code, "A different capture is already retained for this attempt.", 409)
	default:
		return core.Fail("capture_unavailable", "The capture is unavailable or its durable retention is uncertain; retry with the same attempt and capture.", 503)
	}
}
