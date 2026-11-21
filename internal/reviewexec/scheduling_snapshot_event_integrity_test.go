package reviewexec

import (
	"database/sql"
	"testing"
)

func schedulingEventSnapshotFixture(t *testing.T) *sql.Tx {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback() })
	// These are in-memory snapshot inputs, not changes to a running project.
	// Keep event revision untyped to model the native value delivered to Scan.
	for _, statement := range []string{
		"CREATE TABLE metadata(key,value)",
		"INSERT INTO metadata VALUES ('schema:review_scheduling','2')",
		"CREATE TABLE events(sequence INTEGER PRIMARY KEY, project_id, resource_id, event_type, revision, actor_id, payload_json)",
		"CREATE TABLE review_scheduling(project_id,revision,automatic_hosted,automatic_cli,actor_id,worker_id,credential_id,updated_at)",
		"CREATE TABLE review_pull_scheduling(project_id,pull_number,revision,paused,reason,actor_id,updated_at)",
		"CREATE TABLE review_automatic_selections(selection_id,project_id,scheduling_revision,basis_sha256,settled_basis_sha256,pull_number,pull_basis_sha256,actor_id,worker_id,credential_id)",
		"CREATE TABLE review_selections(id,project_id,channel,state,actor_id,worker_id,operation_id)",
		"CREATE TABLE review_operations(id,project_id,pull_number,channel,actor_id,worker_id)",
		"CREATE TABLE workers(project_id,id)",
		"CREATE TABLE credentials(id,principal_id)",
	} {
		if _, err := tx.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	return tx
}

func TestSchedulingSnapshotRequiresEnabledProjectProjection(t *testing.T) {
	tx := schedulingEventSnapshotFixture(t)
	if err := ValidateSchedulingSnapshot(t.Context(), tx); err != nil {
		t.Fatalf("project with no scheduling history: %v", err)
	}
	// An ordinary configure event in a partial snapshot with no control row.
	_, err := tx.ExecContext(t.Context(), `INSERT INTO events(project_id,resource_id,event_type,revision,actor_id,payload_json)
		VALUES ('test-project','test-project','review.scheduling_updated',1,'test-actor','{"automatic_hosted":true}')`)
	if err != nil {
		t.Fatal(err)
	}
	attemptCode(t, ValidateSchedulingSnapshot(t.Context(), tx), "storage_unavailable")
}

func TestSchedulingSnapshotRequiresNativeEventRevision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision any
		valid    bool
	}{{"integer", int64(1), true}, {"real", float64(1), false}} {
		t.Run(tc.name, func(t *testing.T) {
			tx := schedulingEventSnapshotFixture(t)
			_, err := tx.ExecContext(t.Context(), `INSERT INTO events(project_id,resource_id,event_type,revision,actor_id,payload_json)
				VALUES ('test-project','2','review.pull_scheduling_updated',?,'test-actor','{"pull_number":2,"paused":true}')`, tc.revision)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO review_pull_scheduling VALUES ('test-project',2,1,1,'','test-actor',?)`, stamp()); err != nil {
				t.Fatal(err)
			}
			err = ValidateSchedulingSnapshot(t.Context(), tx)
			if tc.valid {
				if err != nil {
					t.Fatalf("native revision refused: %v", err)
				}
			} else {
				attemptCode(t, err, "storage_unavailable")
			}
		})
	}
}

// Different projects can legitimately schedule the same pull number. Each
// one's newest event must remain attached to its own represented control.
func TestSchedulingSnapshotPullEventStreamKeepsProjectIdentity(t *testing.T) {
	tx := schedulingEventSnapshotFixture(t)
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO review_pull_scheduling VALUES
		('test-project',2,2,0,'','test-actor',?),
		('other-project',2,1,1,'','other-actor',?)`, stamp(), stamp()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO events(project_id,resource_id,event_type,revision,actor_id,payload_json) VALUES
		('test-project','2','review.pull_scheduling_updated',1,'test-actor','{"pull_number":2,"paused":true}'),
		('other-project','2','review.pull_scheduling_updated',1,'other-actor','{"pull_number":2,"paused":true}'),
		('test-project','2','review.pull_scheduling_updated',2,'test-actor','{"pull_number":2,"paused":false}'),
		('other-project','2','ordinary.event',9,'other-actor','{}')`); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSchedulingSnapshot(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
}
