package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"github.com/benhook1013/FireController/internal/core"
)

func EnsureSelectionSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_selection", SelectionSchemaSQL(), []string{selectionQuarantineTableSQL})
}

// SelectionSchemaSQL returns an independent canonical DDL slice.
func SelectionSchemaSQL() []string {
	return []string{
		`CREATE TABLE review_selections(
 id TEXT NOT NULL PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id),actor_id TEXT NOT NULL,worker_id TEXT NOT NULL,channel TEXT NOT NULL CHECK(channel IN ('cli','hosted')),
 configuration_revision INTEGER NOT NULL CHECK(configuration_revision>=1),policy_revision INTEGER NOT NULL CHECK(policy_revision>=1),capability_fingerprint TEXT NOT NULL,
 expected_pull_number INTEGER NOT NULL CHECK(expected_pull_number BETWEEN 0 AND 2147483647),state TEXT NOT NULL CHECK(state IN ('queued','selecting','selected','held','empty')),
 reason TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>=1),operation_id TEXT UNIQUE REFERENCES review_operations(id),created_at TEXT NOT NULL,updated_at TEXT NOT NULL,
 CHECK((state='selected' AND operation_id IS NOT NULL) OR (state<>'selected' AND operation_id IS NULL)),
 FOREIGN KEY(project_id,configuration_revision) REFERENCES review_execution_config(project_id,revision),FOREIGN KEY(project_id,policy_revision) REFERENCES review_policy_config(project_id,revision))`,
		`CREATE UNIQUE INDEX review_selections_pending_channel ON review_selections(project_id,channel) WHERE state IN ('queued','selecting')`,
		`CREATE INDEX review_selections_pending ON review_selections(state,created_at,id)`,
	}
}

// The diagnostic binding is not an operation foreign key. It is only used for a
// well-typed historical link and cannot confer authority.
const selectionQuarantineTableSQL = `CREATE TABLE review_selection_quarantine(
 selection_id TEXT NOT NULL PRIMARY KEY REFERENCES review_selections(id),
 project_id TEXT NOT NULL REFERENCES projects(id),
 original_operation_id TEXT NOT NULL CHECK(typeof(original_operation_id)='text' AND length(CAST(original_operation_id AS BLOB)) BETWEEN 1 AND 128),
 original_state TEXT NOT NULL CHECK(original_state IN ('queued','selecting')),
 observed_revision INTEGER NOT NULL CHECK(typeof(observed_revision)='integer' AND observed_revision>0),
 UNIQUE(project_id,original_operation_id))`

const linkedSelectionCustodyHeld = "selection_custody_quarantined"

// requireUnquarantinedSelectionLink only denies work. The custody row cannot
// replace active selection attribution or authorize a new provider action.
func requireUnquarantinedSelectionLink(ctx context.Context, q EvidenceQuery, project, operation string) error {
	var proved bool
	err := q.QueryRowContext(ctx, `SELECT COALESCE(s.project_id=d.project_id AND s.state='held' AND
	  s.reason='linked_selection_quarantined' AND s.operation_id IS NULL AND
	  typeof(d.observed_revision)='integer' AND d.observed_revision>0 AND
	  typeof(s.revision)='integer' AND s.revision=CASE WHEN d.observed_revision=9223372036854775807
	     THEN d.observed_revision ELSE d.observed_revision+1 END AND
	  d.original_state IN ('queued','selecting') AND
	  (o.id IS NULL OR (o.project_id=s.project_id AND o.actor_id=s.actor_id AND o.worker_id=s.worker_id AND
	     o.channel=s.channel AND o.configuration_revision=s.configuration_revision AND o.policy_revision=s.policy_revision AND
	     o.capability_fingerprint=s.capability_fingerprint AND (s.expected_pull_number=0 OR s.expected_pull_number=o.pull_number)))
	  AND NOT EXISTS(SELECT 1 FROM review_selections live WHERE live.operation_id=d.original_operation_id)
	  AND NOT EXISTS(SELECT 1 FROM review_automatic_selections a WHERE a.selection_id=s.id AND
	    (a.project_id<>s.project_id OR a.actor_id<>s.actor_id OR a.worker_id<>s.worker_id OR
	     typeof(a.pull_number)<>'integer' OR a.pull_number<0 OR a.pull_number>2147483647 OR
	     (a.pull_number<>0 AND (o.id IS NULL OR o.pull_number<>a.pull_number)) OR
	     (a.pull_number=0 AND a.pull_basis_sha256<>''))), 0)
	  FROM review_selection_quarantine d LEFT JOIN review_selections s ON s.id=d.selection_id
	  LEFT JOIN review_operations o ON o.id=d.original_operation_id
	  WHERE d.project_id=? AND d.original_operation_id=?`, project, operation).Scan(&proved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !proved {
		// An unknown custody row cannot establish ordinary ineligibility.
		return runUnavailable()
	}
	return automaticProjectStateFailure(linkedSelectionCustodyHeld, "This review operation is held for selection custody recovery.", 403)
}

func isLinkedSelectionCustodyHeld(err error) bool {
	var failure *core.Error
	return errors.As(err, &failure) && failure.Code == linkedSelectionCustodyHeld
}
