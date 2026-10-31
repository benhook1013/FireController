package reviews

import "strings"

// HistorySchemaSQL returns independent canonical physical definitions for
// migration and read-only backup inspection.
func HistorySchemaSQL() []string {
	out := HistorySchemaSQLV10()
	out[0] = strings.Replace(out[0], " metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (length(CAST(metadata_json AS BLOB)) <= 512),", " metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (length(CAST(metadata_json AS BLOB)) <= 512), delivery_selected INTEGER NOT NULL DEFAULT 1 CHECK (delivery_selected IN (0,1)),", 1)
	return out
}
func HistorySchemaSQLV10() []string { return append([]string(nil), historySchemaStatements...) }

// historySchemaStatements defines only the physical retained-history tables.
// The owning migration installs these statements before backfill and reader /
// writer conversion; declaring them does not advance the reviews schema.
//
// Payload rows are immutable by writer contract. Membership closure is the only
// in-place historical change. Cross-interval overlap and current-reference
// visibility at the workspace revision require canonical logical validation;
// the DDL enforces exact same-project body, owner, order and snapshot references.
var historySchemaStatementsV4 = []string{
	`CREATE TABLE review_pull_versions (
 project_id TEXT NOT NULL,
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 version_revision INTEGER NOT NULL CHECK (version_revision >= 1),
 title TEXT NOT NULL,
 state TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 base_ref TEXT NOT NULL,
 base_sha TEXT NOT NULL,
 ci_state TEXT NOT NULL,
 mergeability TEXT NOT NULL,
 complete INTEGER NOT NULL CHECK (complete IN (0,1)),
 run_count INTEGER NOT NULL CHECK (run_count >= 0),
 attempt_count INTEGER NOT NULL CHECK (attempt_count >= 0),
 recent_runs_json TEXT NOT NULL,
 latest_attempts_json TEXT NOT NULL,
 PRIMARY KEY (project_id,number,version_revision),
 FOREIGN KEY (project_id,version_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE TABLE review_run_versions (
 project_id TEXT NOT NULL,
 id TEXT NOT NULL,
 version_revision INTEGER NOT NULL CHECK (version_revision >= 1),
 pull_number INTEGER NOT NULL CHECK (pull_number > 0 AND pull_number <= 2147483647),
 source TEXT NOT NULL,
 source_run_id TEXT NOT NULL,
 channel TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 reviewer TEXT NOT NULL,
 scope TEXT NOT NULL,
 outcome TEXT NOT NULL,
 attributable INTEGER NOT NULL CHECK (attributable IN (0,1)),
 complete INTEGER NOT NULL CHECK (complete IN (0,1)),
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL,
 found INTEGER NOT NULL CHECK (found >= 0),
 accepted INTEGER NOT NULL CHECK (accepted >= 0),
 routed INTEGER NOT NULL CHECK (routed >= 0),
 finalized INTEGER NOT NULL CHECK (finalized IN (0,1)),
 fixed INTEGER NOT NULL CHECK (fixed >= 0),
 coverage_limits_json TEXT NOT NULL,
 PRIMARY KEY (project_id,id,version_revision),
 UNIQUE (project_id,source,source_run_id,version_revision),
 UNIQUE (project_id,id,version_revision,pull_number),
 FOREIGN KEY (project_id,version_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE INDEX review_run_versions_source_revision ON review_run_versions(project_id,source,source_run_id,finalized,version_revision DESC);`,
	`CREATE TABLE review_finding_versions (
 project_id TEXT NOT NULL,
 run_id TEXT NOT NULL,
 run_version_revision INTEGER NOT NULL CHECK (run_version_revision >= 1),
 pull_number INTEGER NOT NULL CHECK (pull_number > 0 AND pull_number <= 2147483647),
 position INTEGER NOT NULL CHECK (position >= 0),
 source_finding_id TEXT NOT NULL,
 title TEXT NOT NULL,
 detail TEXT NOT NULL,
 severity TEXT NOT NULL,
 disposition TEXT NOT NULL,
 decision_reason TEXT NOT NULL,
 actor TEXT NOT NULL,
 decided_at TEXT NOT NULL,
 fix_sha TEXT NOT NULL,
 proof_note TEXT NOT NULL,
 resolved_at TEXT NOT NULL,
 route_json TEXT NOT NULL DEFAULT 'null',
 PRIMARY KEY (project_id,run_id,run_version_revision,source_finding_id),
 UNIQUE (project_id,run_id,run_version_revision,position),
 FOREIGN KEY (project_id,run_id,run_version_revision,pull_number) REFERENCES review_run_versions(project_id,id,version_revision,pull_number) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE TABLE review_attempt_versions (
 project_id TEXT NOT NULL,
 source TEXT NOT NULL,
 source_attempt_id TEXT NOT NULL,
 version_revision INTEGER NOT NULL CHECK (version_revision >= 1),
 pull_number INTEGER NOT NULL CHECK (pull_number > 0 AND pull_number <= 2147483647),
 channel TEXT NOT NULL,
 head_sha TEXT NOT NULL,
 state TEXT NOT NULL,
 started_at TEXT NOT NULL,
 finished_at TEXT NOT NULL,
 duration_seconds INTEGER CHECK (duration_seconds IS NULL OR duration_seconds >= 0),
 source_run_id TEXT NOT NULL,
 trigger_id TEXT NOT NULL,
 provider_review_id TEXT NOT NULL,
 PRIMARY KEY (project_id,source,source_attempt_id,version_revision),
 UNIQUE (project_id,source,source_attempt_id,version_revision,pull_number),
 FOREIGN KEY (project_id,version_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE TABLE review_retired_versions (
 project_id TEXT NOT NULL,
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 version_revision INTEGER NOT NULL CHECK (version_revision >= 1),
 reason TEXT NOT NULL CHECK (reason IN ('superseded','defunct')),
 PRIMARY KEY (project_id,number,version_revision),
 FOREIGN KEY (project_id,version_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE TABLE review_pull_memberships (
 project_id TEXT NOT NULL,
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 start_revision INTEGER NOT NULL CHECK (start_revision >= 1),
 end_revision INTEGER CHECK (end_revision IS NULL OR end_revision > start_revision),
 version_revision INTEGER NOT NULL CHECK (version_revision >= 1 AND version_revision <= start_revision),
 position INTEGER NOT NULL CHECK (position >= 0),
 PRIMARY KEY (project_id,number,start_revision),
 UNIQUE (project_id,number,start_revision,position),
 FOREIGN KEY (project_id,start_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION,
 FOREIGN KEY (project_id,end_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION,
 FOREIGN KEY (project_id,number,version_revision) REFERENCES review_pull_versions(project_id,number,version_revision) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE UNIQUE INDEX review_pull_memberships_open ON review_pull_memberships(project_id,number) WHERE end_revision IS NULL;`,
	`CREATE INDEX review_pull_memberships_page ON review_pull_memberships(project_id,position,start_revision DESC);`,
	`CREATE TABLE review_run_memberships (
 project_id TEXT NOT NULL,
 id TEXT NOT NULL,
 start_revision INTEGER NOT NULL CHECK (start_revision >= 1),
 end_revision INTEGER CHECK (end_revision IS NULL OR end_revision > start_revision),
 version_revision INTEGER NOT NULL CHECK (version_revision >= 1 AND version_revision <= start_revision),
 pull_number INTEGER NOT NULL CHECK (pull_number > 0 AND pull_number <= 2147483647),
 run_order INTEGER NOT NULL CHECK (run_order >= 0),
 PRIMARY KEY (project_id,id,start_revision),
 UNIQUE (project_id,id,start_revision,pull_number,run_order),
 FOREIGN KEY (project_id,start_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION,
 FOREIGN KEY (project_id,end_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION,
 FOREIGN KEY (project_id,id,version_revision,pull_number) REFERENCES review_run_versions(project_id,id,version_revision,pull_number) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE UNIQUE INDEX review_run_memberships_open ON review_run_memberships(project_id,id) WHERE end_revision IS NULL;`,
	`CREATE INDEX review_run_memberships_page ON review_run_memberships(project_id,pull_number,run_order DESC,start_revision DESC);`,
	`CREATE TABLE review_attempt_memberships (
 project_id TEXT NOT NULL,
 source TEXT NOT NULL,
 source_attempt_id TEXT NOT NULL,
 start_revision INTEGER NOT NULL CHECK (start_revision >= 1),
 end_revision INTEGER CHECK (end_revision IS NULL OR end_revision > start_revision),
 version_revision INTEGER NOT NULL CHECK (version_revision >= 1 AND version_revision <= start_revision),
 pull_number INTEGER NOT NULL CHECK (pull_number > 0 AND pull_number <= 2147483647),
 position INTEGER NOT NULL CHECK (position >= 0),
 PRIMARY KEY (project_id,source,source_attempt_id,start_revision),
 UNIQUE (project_id,source,source_attempt_id,start_revision,pull_number,position),
 FOREIGN KEY (project_id,start_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION,
 FOREIGN KEY (project_id,end_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION,
 FOREIGN KEY (project_id,source,source_attempt_id,version_revision,pull_number) REFERENCES review_attempt_versions(project_id,source,source_attempt_id,version_revision,pull_number) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE UNIQUE INDEX review_attempt_memberships_open ON review_attempt_memberships(project_id,source,source_attempt_id) WHERE end_revision IS NULL;`,
	`CREATE INDEX review_attempt_memberships_page ON review_attempt_memberships(project_id,pull_number,position DESC,start_revision DESC);`,
	`CREATE TABLE review_retired_memberships (
 project_id TEXT NOT NULL,
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 start_revision INTEGER NOT NULL CHECK (start_revision >= 1),
 end_revision INTEGER CHECK (end_revision IS NULL OR end_revision > start_revision),
 version_revision INTEGER NOT NULL CHECK (version_revision >= 1 AND version_revision <= start_revision),
 position INTEGER NOT NULL CHECK (position >= 0),
 PRIMARY KEY (project_id,number,start_revision),
 UNIQUE (project_id,number,start_revision,position),
 FOREIGN KEY (project_id,start_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION,
 FOREIGN KEY (project_id,end_revision) REFERENCES review_snapshots(project_id,revision) ON UPDATE NO ACTION ON DELETE NO ACTION,
 FOREIGN KEY (project_id,number,version_revision) REFERENCES review_retired_versions(project_id,number,version_revision) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE UNIQUE INDEX review_retired_memberships_open ON review_retired_memberships(project_id,number) WHERE end_revision IS NULL;`,
	`CREATE INDEX review_retired_memberships_page ON review_retired_memberships(project_id,position,start_revision DESC);`,
	`CREATE TABLE review_current_pulls (
 project_id TEXT NOT NULL,
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 start_revision INTEGER NOT NULL CHECK (start_revision >= 1),
 position INTEGER NOT NULL CHECK (position >= 0),
 PRIMARY KEY (project_id,number),
 UNIQUE (project_id,position),
 FOREIGN KEY (project_id,number,start_revision,position) REFERENCES review_pull_memberships(project_id,number,start_revision,position) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE TABLE review_current_runs (
 project_id TEXT NOT NULL,
 id TEXT NOT NULL,
 start_revision INTEGER NOT NULL CHECK (start_revision >= 1),
 pull_number INTEGER NOT NULL CHECK (pull_number > 0 AND pull_number <= 2147483647),
 run_order INTEGER NOT NULL CHECK (run_order >= 0),
 PRIMARY KEY (project_id,id),
 UNIQUE (project_id,pull_number,run_order),
 FOREIGN KEY (project_id,id,start_revision,pull_number,run_order) REFERENCES review_run_memberships(project_id,id,start_revision,pull_number,run_order) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE TABLE review_current_attempts (
 project_id TEXT NOT NULL,
 source TEXT NOT NULL,
 source_attempt_id TEXT NOT NULL,
 start_revision INTEGER NOT NULL CHECK (start_revision >= 1),
 pull_number INTEGER NOT NULL CHECK (pull_number > 0 AND pull_number <= 2147483647),
 position INTEGER NOT NULL CHECK (position >= 0),
 PRIMARY KEY (project_id,source,source_attempt_id),
 UNIQUE (project_id,pull_number,position),
 FOREIGN KEY (project_id,source,source_attempt_id,start_revision,pull_number,position) REFERENCES review_attempt_memberships(project_id,source,source_attempt_id,start_revision,pull_number,position) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
	`CREATE TABLE review_current_retired (
 project_id TEXT NOT NULL,
 number INTEGER NOT NULL CHECK (number > 0 AND number <= 2147483647),
 start_revision INTEGER NOT NULL CHECK (start_revision >= 1),
 position INTEGER NOT NULL CHECK (position >= 0),
 PRIMARY KEY (project_id,number),
 UNIQUE (project_id,position),
 FOREIGN KEY (project_id,number,start_revision,position) REFERENCES review_retired_memberships(project_id,number,start_revision,position) ON UPDATE NO ACTION ON DELETE NO ACTION
);`,
}

// Version four is frozen for old backups and the original migration. Version seven appends metadata.
func HistorySchemaSQLV6() []string { return append([]string(nil), historySchemaStatementsV4...) }

func HistorySchemaSQLV7() []string {
	out := HistorySchemaSQLV6()
	out[0] = strings.Replace(out[0], " latest_attempts_json TEXT NOT NULL,\n", " latest_attempts_json TEXT NOT NULL, metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (length(CAST(metadata_json AS BLOB)) <= 512),\n", 1)
	return out
}

var historySchemaStatements = func() []string {
	out := HistorySchemaSQLV7()
	for i, statement := range out {
		if strings.HasPrefix(statement, "CREATE TABLE review_attempt_versions ") {
			out[i] = strings.Replace(statement, " provider_review_id TEXT NOT NULL,\n", " provider_review_id TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '' CHECK (length(CAST(reason AS BLOB)) <= 128),\n", 1)
		}
	}
	return out
}()
