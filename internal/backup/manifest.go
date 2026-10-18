// Package backup owns bounded private snapshot bundles. It never executes
// providers, loads operator credentials, or activates restored work.
package backup

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}
type Manifest struct {
	Version            int        `json:"version"`
	ID                 string     `json:"id"`
	SourceInstanceID   string     `json:"source_instance_id"`
	SnapshotStartedAt  string     `json:"snapshot_started_at"`
	SnapshotFinishedAt string     `json:"snapshot_finished_at"`
	EventSequence      int64      `json:"event_sequence"`
	Database           Artifact   `json:"database"`
	Artifacts          []Artifact `json:"artifacts"`
}
type Summary struct {
	ID                 string `json:"id"`
	State              string `json:"state"`
	SourceInstanceID   string `json:"source_instance_id"`
	Path               string `json:"path"`
	SnapshotStartedAt  string `json:"snapshot_started_at"`
	SnapshotFinishedAt string `json:"snapshot_finished_at"`
	EventSequence      int64  `json:"event_sequence"`
	DatabaseBytes      int64  `json:"database_bytes"`
	ArtifactCount      int64  `json:"artifact_count"`
	ArtifactBytes      int64  `json:"artifact_bytes"`
}

// These are finite first-slice safety bounds, not deployment capacity claims.
const (
	maxArtifacts           = 100000
	maxManifestBytes       = 64 << 20
	maxDatabaseBytes int64 = math.MaxInt64

	maxCLICaptureBytes    int64 = 24 << 20
	maxHostedCaptureBytes int64 = 48 << 20
)

var backupID = regexp.MustCompile(`^backup_[0-9a-f]{32}$`)
var instanceID = regexp.MustCompile(`^instance_[0-9a-f]{32}$`)
var attemptID = regexp.MustCompile(`^review_attempt_[0-9a-f]{32}$`)
var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func invalid() error {
	return core.Fail("backup_invalid", "The backup bundle or its selected inputs are invalid.", 400)
}
func unavailable() error {
	return core.Fail("backup_unavailable", "The private backup operation could not complete; no unverified bundle is usable.", 503)
}
func exists() error {
	return core.Fail("backup_exists", "The backup destination already exists; it was not replaced.", 409)
}
func checkContext(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return unavailable()
	}
	return nil
}
func canonicalTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil && !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 && s == t.UTC().Format(time.RFC3339Nano)
}
func summary(m Manifest, path, state string) (Summary, error) {
	s := Summary{ID: m.ID, State: state, SourceInstanceID: m.SourceInstanceID, Path: path, SnapshotStartedAt: m.SnapshotStartedAt, SnapshotFinishedAt: m.SnapshotFinishedAt, EventSequence: m.EventSequence, DatabaseBytes: m.Database.Bytes, ArtifactCount: int64(len(m.Artifacts))}
	for _, a := range m.Artifacts {
		s.ArtifactBytes += a.Bytes
	}
	b, err := json.Marshal(s)
	if err != nil || len(b)+1 > 4096 {
		return Summary{}, invalid()
	}
	return s, nil
}
