package backup

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These are the largest canonical scalar values a newly created bundle can
// report. Artifact sums retain the existing per-capture and inventory bounds.
func managedSummaryBudgetProbe(destination string) Summary {
	const latest = "9999-12-31T23:59:59.999999999Z"
	id := "backup_" + strings.Repeat("f", 32)
	return Summary{ID: id, State: "complete", SourceInstanceID: "instance_" + strings.Repeat("f", 32), Path: filepath.Join(destination, id),
		SnapshotStartedAt: latest, SnapshotFinishedAt: latest, EventSequence: math.MaxInt64, DatabaseBytes: maxDatabaseBytes,
		ArtifactCount: maxArtifacts, ArtifactBytes: int64(maxArtifacts) * maxHostedCaptureBytes}
}

// Select exact encoded boundaries without creating the synthetic directory
// tree. Components stay below ordinary filesystem name limits, and the raw
// normalized destination stays below its separate 2048-byte configuration cap.
func managedSummaryBudgetDestination(t *testing.T, root string, size int) string {
	t.Helper()
	destination := filepath.Join(root, "absent-destination")
	encodedSize := func(path string) int {
		body, err := json.Marshal(managedSummaryBudgetProbe(path))
		if err != nil {
			t.Fatal(err)
		}
		return len(body) + 1
	}
	for {
		next := destination + "&"
		if len(filepath.Base(destination)) >= 180 {
			next = filepath.Join(destination, "&")
		}
		if encodedSize(next) > size {
			break
		}
		destination = next
	}
	padding := size - encodedSize(destination)
	if padding < 0 {
		t.Fatal("fixture base already exceeds summary bound")
	}
	destination += strings.Repeat("x", padding)
	if encodedSize(destination) != size || len(destination) > 2048 || filepath.Clean(destination) != destination || !filepath.IsAbs(destination) {
		t.Fatal("fixture must isolate the encoded summary bound", encodedSize(destination), len(destination))
	}
	return destination
}

func TestManagedSummaryBudgetEscapedDestinationBoundaryBeforeWrites(t *testing.T) {
	for _, size := range []int{4096, 4097} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			root := t.TempDir()
			destination := managedSummaryBudgetDestination(t, root, size)
			cfg := ScheduleConfig{Destination: destination, Interval: time.Minute, Timeout: 5 * time.Minute, Keep: 1}
			probe := managedSummaryBudgetProbe(destination)
			// Even two such summaries fit the larger operator status. Rejection
			// must therefore enforce the individual creation-summary boundary.
			status := Status{State: "scheduled", Destination: destination, Interval: cfg.Interval.String(), Timeout: cfg.Timeout.String(), Keep: cfg.Keep, LastSuccess: &probe, UnregisteredBundle: &probe}
			body, err := json.Marshal(status)
			if err != nil || len(body)+1 > maxManagedStatusBytes {
				t.Fatal("fixture must remain within the larger status budget", len(body), err)
			}
			got, err := ValidateScheduleConfig(filepath.Join(root, "absent-source"), cfg)
			if size == 4096 {
				if err != nil || got != cfg {
					t.Fatal("newline-inclusive boundary should remain configurable", err)
				}
			} else {
				if err == nil {
					t.Fatal("configuration admitted a 4097-byte future bundle summary")
				}
				backupCode(t, err, "configuration_invalid")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatal("configuration validation created filesystem state", len(entries), err)
			}
			t.Logf("maximal summary bytes=%d; normalized path bytes=%d; two-summary status bytes=%d", size, len(destination), len(body)+1)
		})
	}
}

func TestManagedSummaryBudgetRefusalPreservesActualSuccess(t *testing.T) {
	f, manager := newManagedFixture(t, 1)
	if reports := runManaged(t, manager, 1); len(reports) != 0 {
		t.Fatal(reports)
	}
	previous, err := ReadStatus(t.Context(), f.dir)
	if err != nil || previous.LastSuccess == nil {
		t.Fatal("genuine prior success required", err)
	}
	root := t.TempDir()
	cfg := ScheduleConfig{Destination: managedSummaryBudgetDestination(t, root, 4097), Interval: time.Minute, Keep: 1}
	before := backupRows(t, f.store.DB())
	_, err = NewManager(f.store, f.dir, cfg)
	backupCode(t, err, "configuration_invalid")
	current, err := ReadStatus(t.Context(), f.dir)
	if err != nil || !reflect.DeepEqual(current, previous) || !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("refused configuration changed prior status, bundle ledger or source", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("refused manager created destination state", len(entries), err)
	}
}
