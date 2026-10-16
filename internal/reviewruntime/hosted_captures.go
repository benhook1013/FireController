package reviewruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxHostedCaptureFile = 48 << 20

// HostedCaptureRecord retains a complete scoped observation, not findings or
// coverage. A complete collection may still contain ambiguous publication facts.
type HostedCaptureRecord struct {
	RepositoryID    int64                                    `json:"repository_id"`
	Repository      string                                   `json:"repository"`
	PullNumber      int64                                    `json:"pull_number"`
	Intent          reviewprovider.HostedTriggerIntent       `json:"intent"`
	PostingActor    reviewprovider.Author                    `json:"posting_actor"`
	ProviderActor   reviewprovider.Author                    `json:"provider_actor"`
	EarliestPosting time.Time                                `json:"earliest_posting"`
	StartedAt       time.Time                                `json:"started_at"`
	FinishedAt      time.Time                                `json:"finished_at"`
	Snapshot        reviewprovider.HostedPublicationSnapshot `json:"snapshot"`
}

// HostedCaptureStore publishes immutable private observations. Callers retain
// each returned reference; the store does not list observations or poll providers.
// As with CaptureStore, it does not defend against hostile same-user FS races.
type HostedCaptureStore struct{ root string }

func (*HostedCaptureStore) String() string     { return "HostedCaptureStore{private}" }
func (s *HostedCaptureStore) GoString() string { return s.String() }

type hostedCaptureEnvelope struct {
	Version   int                 `json:"version"`
	AttemptID string              `json:"attempt_id"`
	Record    HostedCaptureRecord `json:"record"`
}

func NewHostedCaptureStore(root string) (*HostedCaptureStore, error) {
	if root == "" {
		return nil, captureError("configuration_invalid")
	}
	absolute, err := filepath.Abs(root)
	if err != nil || captureDirectories(absolute, true) != nil {
		return nil, captureError("configuration_invalid")
	}
	return &HostedCaptureStore{root: absolute}, nil
}

func validHostedCapture(attemptID string, r HostedCaptureRecord) bool {
	parts := strings.Split(r.Repository, "/")
	if !validIdentifier(attemptID) || r.Intent.AttemptID != attemptID || r.RepositoryID <= 0 || len(parts) != 2 || !asciiName(parts[0], 100, "._-", true) || !asciiName(parts[1], 100, "._-", true) || r.PullNumber < 1 || r.PullNumber > 2147483647 || !r.Snapshot.Complete {
		return false
	}
	for _, instant := range []time.Time{r.EarliestPosting, r.StartedAt, r.FinishedAt, r.Snapshot.ObservedAt} {
		if instant.Location() != time.UTC || instant.IsZero() || instant.Year() < 1 || instant.Year() > 9999 {
			return false
		}
	}
	if r.Snapshot.ObservedAt.Before(r.StartedAt) || r.Snapshot.ObservedAt.After(r.FinishedAt) || r.EarliestPosting.After(r.Snapshot.ObservedAt) {
		return false
	}
	publication, err := reviewprovider.ResolveHostedPublication(r.Snapshot, r.Intent, r.PostingActor, r.ProviderActor, r.EarliestPosting)
	return err == nil && publication.Reason != "invalid_publication_evidence"
}

// hostedCaptureBuffer bounds encoded bytes as well as the provider body budget.
type hostedCaptureBuffer struct{ bytes.Buffer }

func (b *hostedCaptureBuffer) Write(p []byte) (int, error) {
	if len(p) > maxHostedCaptureFile-b.Len() {
		return 0, errors.New("capture size")
	}
	return b.Buffer.Write(p)
}

func encodeHostedCapture(attemptID string, r HostedCaptureRecord) ([]byte, error) {
	var b hostedCaptureBuffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(hostedCaptureEnvelope{Version: 1, AttemptID: attemptID, Record: r}); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func hostedCaptureFilename(attemptID string, ref CaptureReference) string {
	return attemptID + "-hosted-" + ref.SHA256 + ".json"
}

func validHostedReference(ref CaptureReference) bool {
	if len(ref.SHA256) != 64 || ref.Bytes <= 0 || ref.Bytes > maxHostedCaptureFile || ref.ID != "review_capture_"+ref.SHA256[:32] {
		return false
	}
	for _, c := range ref.SHA256 {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Save atomically publishes without replacing any observation. An uncertain
// directory sync is reconciled by retrying this exact record, never a new post.
func (s *HostedCaptureStore) Save(ctx context.Context, attemptID string, record HostedCaptureRecord) (CaptureReference, error) {
	var empty CaptureReference
	if ctx == nil || ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	if !validHostedCapture(attemptID, record) {
		return empty, captureError("validation_failed")
	}
	data, err := encodeHostedCapture(attemptID, record)
	if err != nil {
		return empty, captureError("validation_failed")
	}
	if s == nil || captureDirectories(s.root, false) != nil || ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	ref := captureReference(data)
	final := filepath.Join(s.root, hostedCaptureFilename(attemptID, ref))
	reconcile := func() (CaptureReference, error) {
		if _, err := s.Read(ctx, attemptID, ref); err != nil {
			return empty, err
		}
		if syncCaptureDirectory(s.root) != nil || ctx.Err() != nil {
			return empty, captureError("capture_unavailable")
		}
		return ref, nil
	}
	if _, err := os.Lstat(final); err == nil {
		return reconcile()
	} else if !errors.Is(err, os.ErrNotExist) {
		return empty, captureError("capture_unavailable")
	}
	temp, err := os.CreateTemp(s.root, ".hosted-capture-*")
	if err != nil {
		return empty, captureError("capture_unavailable")
	}
	defer os.Remove(temp.Name()) // Only this invocation's disposable file.
	writeErr := temp.Chmod(0600)
	if writeErr == nil {
		_, writeErr = temp.Write(data)
	}
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil || ctx.Err() != nil || captureDirectories(s.root, false) != nil {
		return empty, captureError("capture_unavailable")
	}
	if err := os.Link(temp.Name(), final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return reconcile()
		}
		return empty, captureError("capture_unavailable")
	}
	if syncCaptureDirectory(s.root) != nil || ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	return ref, nil
}

// Read checks the caller-owned reference and strict canonical version-1 bytes.
// Unsupported or corrupt files remain untouched and unavailable.
func (s *HostedCaptureStore) Read(ctx context.Context, attemptID string, expected CaptureReference) (HostedCaptureRecord, error) {
	var empty HostedCaptureRecord
	if ctx == nil || ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	if !validIdentifier(attemptID) || !validHostedReference(expected) {
		return empty, captureError("validation_failed")
	}
	if s == nil || captureDirectories(s.root, false) != nil {
		return empty, captureError("capture_unavailable")
	}
	path := filepath.Join(s.root, hostedCaptureFilename(attemptID, expected))
	before, err := os.Lstat(path)
	if err != nil || !privateRegular(before) || !captureMode(before, 0600) {
		return empty, captureError("capture_unavailable")
	}
	data, _, err := readPrivateFile(path, maxHostedCaptureFile)
	var envelope hostedCaptureEnvelope
	// Validate rejects duplicate/invalid Unicode JSON; canonical re-encoding below
	// additionally rejects unknown, missing, null or case-aliased fields without
	// treating time.Time's JSON strings as ordinary Go structs.
	if err != nil || captureReference(data) != expected || strictjson.Validate(data) != nil || json.Unmarshal(data, &envelope) != nil || envelope.Version != 1 || envelope.AttemptID != attemptID || !validHostedCapture(attemptID, envelope.Record) {
		return empty, captureError("capture_unavailable")
	}
	canonical, err := encodeHostedCapture(attemptID, envelope.Record)
	if err != nil || !bytes.Equal(data, canonical) {
		return empty, captureError("capture_unavailable")
	}
	after, err := os.Lstat(path)
	if err != nil || !privateRegular(after) || !captureMode(after, 0600) || !os.SameFile(before, after) || captureDirectories(s.root, false) != nil || ctx.Err() != nil {
		return empty, captureError("capture_unavailable")
	}
	return envelope.Record, nil
}
