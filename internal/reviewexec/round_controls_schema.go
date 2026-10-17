package reviewexec

import (
	"context"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

// EnsureRoundSchema installs explicit additional-round accounting. It never
// derives grants or completion credit from existing execution/imported history.
func EnsureRoundSchema(ctx context.Context, store *core.Store) error {
	return store.EnsureSchema(ctx, "review_rounds", RoundSchemaV1SQL(), roundV2MigrationSQL())
}

var roundSchemaV1SQL = []string{
	`CREATE TABLE review_round_controls(
 project_id TEXT NOT NULL REFERENCES projects(id),pull_number INTEGER NOT NULL CHECK(pull_number BETWEEN 1 AND 2147483647),channel TEXT NOT NULL CHECK(channel IN ('cli','hosted')),
 revision INTEGER NOT NULL CHECK(revision>=1),epoch INTEGER NOT NULL CHECK(epoch>=1),grant_revision INTEGER NOT NULL CHECK(grant_revision>=1 AND grant_revision<=revision),
 closed INTEGER NOT NULL CHECK(closed IN (0,1)),minimum INTEGER NOT NULL CHECK(minimum BETWEEN 0 AND 1000),maximum INTEGER NOT NULL CHECK(maximum BETWEEN 1 AND 1000 AND minimum<=maximum),
 taper INTEGER NOT NULL CHECK((channel='hosted' AND taper=1) OR (channel='cli' AND taper=3)),used INTEGER NOT NULL CHECK(used BETWEEN 0 AND maximum),dry INTEGER NOT NULL CHECK(dry BETWEEN 0 AND used),tapered INTEGER NOT NULL CHECK(tapered IN (0,1)),
 actor_id TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(project_id,pull_number,channel),
 FOREIGN KEY(project_id,pull_number,channel,grant_revision,epoch) REFERENCES review_round_changes(project_id,pull_number,channel,revision,epoch) DEFERRABLE INITIALLY DEFERRED);`,
	`CREATE TABLE review_round_changes(
 project_id TEXT NOT NULL REFERENCES projects(id),pull_number INTEGER NOT NULL,channel TEXT NOT NULL CHECK(channel IN ('cli','hosted')),revision INTEGER NOT NULL CHECK(revision>=1),epoch INTEGER NOT NULL CHECK(epoch>=1),
 action TEXT NOT NULL CHECK(action IN ('grant','close')),body_json TEXT NOT NULL,worker_id TEXT NOT NULL,
 PRIMARY KEY(project_id,pull_number,channel,revision),UNIQUE(project_id,pull_number,channel,revision,epoch));`,
	`CREATE TABLE review_round_operation_bindings(
 operation_id TEXT NOT NULL PRIMARY KEY REFERENCES review_operations(id),project_id TEXT NOT NULL,pull_number INTEGER NOT NULL,channel TEXT NOT NULL,epoch INTEGER NOT NULL,grant_revision INTEGER NOT NULL,
 UNIQUE(operation_id,project_id,pull_number,channel,epoch),FOREIGN KEY(project_id,pull_number,channel,grant_revision,epoch) REFERENCES review_round_changes(project_id,pull_number,channel,revision,epoch));`,
	`CREATE INDEX review_round_bindings_scope ON review_round_operation_bindings(project_id,pull_number,channel,epoch,operation_id);`,
	`CREATE TABLE review_round_completions(
 operation_id TEXT NOT NULL PRIMARY KEY,evidence_id TEXT NOT NULL UNIQUE REFERENCES review_execution_evidence(id),project_id TEXT NOT NULL,pull_number INTEGER NOT NULL,channel TEXT NOT NULL,epoch INTEGER NOT NULL,
 ordinal INTEGER NOT NULL CHECK(ordinal BETWEEN 1 AND 1000),accepted INTEGER NOT NULL CHECK(accepted BETWEEN 0 AND 100),completed_at TEXT NOT NULL,
 UNIQUE(project_id,pull_number,channel,epoch,ordinal),FOREIGN KEY(operation_id,project_id,pull_number,channel,epoch) REFERENCES review_round_operation_bindings(operation_id,project_id,pull_number,channel,epoch));`,
	`CREATE INDEX review_round_operations_scope ON review_operations(project_id,pull_number,channel,state,id);`,
}

// RoundSchemaV1SQL retains the original canonical predecessor declarations.
func RoundSchemaV1SQL() []string { return append([]string(nil), roundSchemaV1SQL...) }

// RoundSchemaSQL returns the current canonical physical schema as a fresh slice.
func RoundSchemaSQL() []string {
	out := RoundSchemaV1SQL()
	out[0] = roundControlV2SQL()
	return out
}

func roundControlV2SQL() string {
	s := roundSchemaV1SQL[0]
	s = strings.Replace(s, "maximum BETWEEN 1 AND 1000", "maximum BETWEEN 0 AND 1000", 1)
	s = strings.Replace(s, "CHECK((channel='hosted' AND taper=1) OR (channel='cli' AND taper=3))", "CHECK(taper BETWEEN 1 AND 1000)", 1)
	s = strings.Replace(s, "dry BETWEEN 0 AND used", "dry BETWEEN 0 AND used+initial_dry", 1)
	s = strings.Replace(s, "actor_id TEXT NOT NULL", "initial_dry INTEGER NOT NULL CHECK(initial_dry BETWEEN 0 AND 1000),initially_tapered INTEGER NOT NULL CHECK(initially_tapered IN (0,1)),origin_source TEXT NOT NULL,origin_key TEXT NOT NULL,origin_sha256 TEXT NOT NULL,actor_id TEXT NOT NULL", 1)
	return s
}

func roundV2MigrationSQL() []string {
	return []string{
		`ALTER TABLE review_round_controls RENAME TO legacy_review_round_controls`,
		roundControlV2SQL(),
		`INSERT INTO review_round_controls(` + roundColumns + `) SELECT ` + roundColumnsV1 + `,0,0,'','','' FROM legacy_review_round_controls`,
		`DROP TABLE legacy_review_round_controls`,
	}
}
