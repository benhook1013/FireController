package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func validateSchedulingSnapshotForTest(t *testing.T, f *attemptFixture) error {
	t.Helper()
	return f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		return ValidateSchedulingSnapshot(ctx, tx)
	})
}

func TestValidateSchedulingSnapshotAcceptsSupportedVersions(t *testing.T) {
	for _, tc := range []struct {
		name           string
		version        string
		legacyPhysical bool
	}{
		{name: "legacy-v1", version: "1", legacyPhysical: true},
		{name: "current-v2", version: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := schedulingFixture(t)
			schedulingEnable(t, f, 0, true)
			if tc.legacyPhysical {
				admissionSQL(t, f, `ALTER TABLE review_scheduling DROP COLUMN automatic_cli`)
			}
			admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_scheduling'`, tc.version)
			if err := validateSchedulingSnapshotForTest(t, f); err != nil {
				t.Fatalf("valid schema version %s rejected: %v", tc.version, err)
			}
		})
	}
}

func TestValidateSchedulingSnapshotRejectsMalformedAndFutureVersions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
	}{
		{name: "malformed", version: "not-a-version"},
		{name: "future", version: "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := schedulingFixture(t)
			admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_scheduling'`, tc.version)
			attemptCode(t, validateSchedulingSnapshotForTest(t, f), "storage_unavailable")
		})
	}
}

func TestValidateSchedulingSnapshotRequiresPresentDomainMetadata(t *testing.T) {
	f := schedulingFixture(t)
	admissionSQL(t, f, `DELETE FROM metadata WHERE key='schema:review_scheduling'`)
	if err := validateSchedulingSnapshotForTest(t, f); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing optional domain metadata should not be mistaken for a schema version: %v", err)
	}
}
