package legacy

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

func selectedJobFixture(t *testing.T) string {
	return snapshotFixture(t, func(db *sql.DB) {
		seedJob(t, db)
		execFixture(t, db, `INSERT INTO job_briefs VALUES('private-job-id',1,'2026-01-01T00:00:00Z','Original brief — exact bytes')`)
		for _, done := range []string{"First checkpoint", "Latest checkpoint"} {
			execFixture(t, db, `INSERT INTO job_checkpoints(job_id,created_at,done,next_steps,blocker,pointers_json) VALUES('private-job-id','2026-01-02T00:00:00Z',?,'Next step','','{"pr":7,"evidence":["one","two"]}')`, done)
		}
	})
}

func jobSelection(mode string) JobSelection {
	revision := int64(0)
	return JobSelection{Version: 1, RepositoryID: 7, ExpectedRevision: &revision, Source: "synthetic-controller", Workers: map[string]string{"private-worker": "worker_native"}, JobIDs: []string{"private-job-id"}, CheckpointMode: mode}
}

func TestPrepareSelectedCurrentJobAndCheckpointFacts(t *testing.T) {
	path := selectedJobFixture(t)
	before := sourceBytes(t, path)
	for _, mode := range []string{"none", "latest", "all"} {
		t.Run(mode, func(t *testing.T) {
			batch, err := PrepareJobs(t.Context(), path, Limits{}, jobSelection(mode))
			if err != nil {
				t.Fatal(err)
			}
			if batch.SourceSHA256 == "" || batch.ExpectedRevision == nil || *batch.ExpectedRevision != 0 || len(batch.Jobs) != 1 {
				t.Fatal("wrong batch binding")
			}
			item := batch.Jobs[0]
			if item.SourceKey != "private-job-id" || item.Brief != "Original brief — exact bytes" || item.WorkerID != "worker_native" || item.SourceRevision != 1 || item.CreatedAt != "2026-01-01T00:00:00Z" {
				t.Fatal("source facts changed")
			}
			want := map[string]int{"none": 0, "latest": 1, "all": 2}[mode]
			if len(item.Checkpoints) != want {
				t.Fatal("checkpoint scope widened")
			}
			if want > 0 {
				cp := item.Checkpoints[len(item.Checkpoints)-1]
				if cp.Done != "Latest checkpoint" || cp.SourceKey != "2" || cp.ActorID != "imported-history" || cp.WorkerID != "" || cp.CreatedAt != "2026-01-02T00:00:00Z" || len(cp.Evidence) != 1 {
					t.Fatal("checkpoint original or unknown attribution changed")
				}
				var refs map[string]json.RawMessage
				if json.Unmarshal([]byte(cp.Evidence[0]), &refs) != nil || string(refs["pr"]) != "7" || string(refs["evidence"]) != "[\"one\",\"two\"]" {
					t.Fatal("non-string pointer facts changed")
				}
			}
			checkConserved(t, path, before)
		})
	}
}

func TestPrepareRefusesMissingMappingUnknownScopeAndOversizeSource(t *testing.T) {
	path := selectedJobFixture(t)
	before := sourceBytes(t, path)
	for _, kind := range []string{"no-workers", "missing-job", "duplicate-job", "unknown-mode", "no-revision", "source-invalid"} {
		t.Run(kind, func(t *testing.T) {
			s := jobSelection("latest")
			switch kind {
			case "no-workers":
				s.Workers = map[string]string{}
			case "missing-job":
				s.JobIDs = []string{"missing"}
			case "duplicate-job":
				s.JobIDs = append(s.JobIDs, s.JobIDs[0])
			case "unknown-mode":
				s.CheckpointMode = "automatic"
			case "no-revision":
				s.ExpectedRevision = nil
			case "source-invalid":
				s.Source = " padded "
			}
			_, err := PrepareJobs(t.Context(), path, Limits{}, s)
			inspectError(t, err, "snapshot_conversion_blocked", "private-job-id", "private-worker")
			checkConserved(t, path, before)
		})
	}
	oversize := snapshotFixture(t, func(db *sql.DB) {
		seedJob(t, db)
		execFixture(t, db, `INSERT INTO job_briefs VALUES('private-job-id',1,'2026-01-01T00:00:00Z',?)`, strings.Repeat("x", 32769))
	})
	original := sourceBytes(t, oversize)
	_, err := PrepareJobs(t.Context(), oversize, Limits{}, jobSelection("none"))
	inspectError(t, err, "snapshot_conversion_blocked")
	checkConserved(t, oversize, original)
}

func TestPrepareRefusesAmbiguousOrCoercedSourceFacts(t *testing.T) {
	for _, kind := range []string{"duplicate-brief", "duplicate-latest-checkpoint", "blob-title", "blob-brief", "blob-checkpoint"} {
		t.Run(kind, func(t *testing.T) {
			path := snapshotFixture(t, func(db *sql.DB) {
				seedJob(t, db)
				execFixture(t, db, `INSERT INTO job_briefs VALUES('private-job-id',1,'2026-01-01T00:00:00Z','Original brief')`)
				switch kind {
				case "duplicate-brief":
					execFixture(t, db, `DROP TABLE job_briefs; CREATE TABLE job_briefs(job_id TEXT,brief_revision INTEGER,created_at TEXT,brief TEXT); INSERT INTO job_briefs VALUES('private-job-id',1,'2026-01-01T00:00:00Z','First'),('private-job-id',1,'2026-01-01T00:00:00Z','Second')`)
				case "blob-title":
					execFixture(t, db, `UPDATE jobs SET title=CAST('Title bytes' AS BLOB)`)
				case "blob-brief":
					execFixture(t, db, `UPDATE job_briefs SET brief=CAST('Brief bytes' AS BLOB)`)
				case "blob-checkpoint":
					execFixture(t, db, `INSERT INTO job_checkpoints(job_id,created_at,done,next_steps,blocker,pointers_json) VALUES('private-job-id','2026-01-02T00:00:00Z',CAST('Done bytes' AS BLOB),'Next','','{}')`)
				case "duplicate-latest-checkpoint":
					execFixture(t, db, `DROP TABLE job_checkpoints; CREATE TABLE job_checkpoints(sequence INTEGER,job_id TEXT,created_at TEXT,done TEXT,next_steps TEXT,blocker TEXT,pointers_json TEXT); INSERT INTO job_checkpoints VALUES(9,'private-job-id','2026-01-02T00:00:00Z','First','Next','','{}'),(9,'private-job-id','2026-01-02T00:00:00Z','Contradiction','Next','','{}')`)
				}
			})
			before := sourceBytes(t, path)
			mode := "all"
			if kind == "duplicate-latest-checkpoint" {
				mode = "latest"
			}
			_, err := PrepareJobs(t.Context(), path, Limits{}, jobSelection(mode))
			inspectError(t, err, "snapshot_conversion_blocked")
			checkConserved(t, path, before)
		})
	}
}
