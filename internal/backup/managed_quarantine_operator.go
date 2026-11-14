package backup

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/core"
)

// ReconcileEmptyQuarantineFromDirectory is an explicit offline operator action.
// The operator must hold exclusive service/deployment custody, stop the daemon,
// and exclude all backup/retention and out-of-band filesystem writers for the
// whole verification and settlement interval. The attestation is not a lock.
// This command never stops a service, changes scheduling, or deletes a file.
func ReconcileEmptyQuarantineFromDirectory(ctx context.Context, dataDir, inactiveAttestation string, request EmptyQuarantineRequest, readback bool) (EmptyQuarantineReceipt, error) {
	var zero EmptyQuarantineReceipt
	if !instanceID.MatchString(request.ExpectedInstanceID) || inactiveAttestation != "inactive:"+request.ExpectedInstanceID {
		return zero, invalid()
	}
	// Validate before core.Open: never bootstrap a missing/partial source.
	if err := ValidateSource(ctx, dataDir); err != nil {
		return zero, err
	}
	store, err := core.Open(ctx, dataDir)
	if err != nil {
		return zero, err
	}
	defer store.Close()
	if store.InstanceID() != request.ExpectedInstanceID {
		return zero, invalid()
	}
	var record *managedRecord
	err = store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var e error
		record, e = loadManaged(ctx, tx)
		if e != nil || record == nil || record.Status.State == "running" {
			return invalid()
		}
		return nil
	})
	if err != nil {
		return zero, err
	}
	m := &Manager{store: store, dataDir: dataDir, config: record.Configuration}
	if readback {
		return m.readEmptyQuarantineOutcome(ctx, request)
	}
	return m.reconcileEmptyQuarantine(ctx, request)
}
