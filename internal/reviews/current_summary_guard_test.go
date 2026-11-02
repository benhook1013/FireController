package reviews

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func recentSummaryGuardFixture(t *testing.T) (*apiFixture, string, string) {
	t.Helper()
	f := newAPIFixture(t)
	pull := nativePull(1)
	for i := 0; i < 4; i++ {
		pull.Runs = append(pull.Runs, nativeRun(fmt.Sprintf("recent-guard-%d", i)))
	}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "recent-guard-import")
	var old PullResponse
	oldWire := string(nativeRead(t, f, "/pulls/1", &old))
	a := nativeAttempt("recent-guard-attempt", "prepared")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &a})
	return f, "/pulls/1?snapshot=" + old.SnapshotID, oldWire
}

func replaceCurrentRecentSummary(t *testing.T, f *apiFixture, stored string) {
	t.Helper()
	if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET recent_runs_json=? WHERE (project_id,number,version_revision) IN (SELECT m.project_id,m.number,m.version_revision FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision WHERE c.project_id=? AND c.number=1)`, stored, f.project); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentRecentSummaryCorruptionRefusesFrontAndIntegrity(t *testing.T) {
	for _, mode := range []string{"reviewer", "counts", "source", "identity", "order", "missing", "extra", "null", "malformed", "unknown-field", "duplicate-field", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			f, oldPath, oldWire := recentSummaryGuardFixture(t)
			var current PullResponse
			nativeRead(t, f, "/pulls/1", &current)
			items := append([]RunSummary{}, current.Pull.RecentRuns...)
			stored := mustJSON(items)
			switch mode {
			case "reviewer":
				items[0].Reviewer = "forged reviewer"
			case "counts":
				items[0].Accepted = 0
				items[0].Fixed = 0
			case "source":
				items[0].Source = "forged producer"
			case "identity":
				items[0].ID = items[1].ID
			case "order":
				items[0], items[1] = items[1], items[0]
			case "missing":
				items = items[:2]
			case "extra":
				items = append(items, items[0])
			case "null":
				stored = "null"
			case "malformed":
				stored = "["
			case "unknown-field":
				stored = strings.Replace(stored, `"reviewer":`, `"unknown":true,"reviewer":`, 1)
			case "duplicate-field":
				stored = strings.Replace(stored, `"reviewer":`, `"reviewer":"duplicate","reviewer":`, 1)
			case "oversized":
				stored = strings.Repeat(" ", 512<<10) + stored
			}
			if mode != "null" && mode != "malformed" && mode != "unknown-field" && mode != "duplicate-field" && mode != "oversized" {
				stored = mustJSON(items)
			}
			replaceCurrentRecentSummary(t, f, stored)
			before := historyMigrationDatabaseDigest(t, f)
			tx := historyWriterTx(t, f)
			err := validateCurrentSummaryFront(t.Context(), tx, f.project, 1)
			_ = tx.Rollback()
			if err == nil {
				t.Fatal("forged current recent summary passed native-front validation", mode)
			}
			if err = ValidateCurrentSummaries(t.Context(), f.store.DB()); err == nil {
				t.Fatal("forged current recent summary passed full integrity", mode)
			}
			a := nativeAttempt("must-not-cover-corrupt-recent", "prepared")
			_, err = nativePublish(f, NativePublication{Pull: nativePull(1), Attempt: &a})
			nativeErrorCode(t, err, "storage_unavailable")
			if historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("refused native publication changed retained history")
			}
			var historical PullResponse
			if got := string(nativeRead(t, f, oldPath, &historical)); got != oldWire {
				t.Fatal("current-only corruption/refusal changed old snapshot")
			}
		})
	}
}

func TestCurrentRecentSummaryValidAndMissingSourceRemainAccepted(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-source-%t", missing), func(t *testing.T) {
			f, oldPath, oldWire := recentSummaryGuardFixture(t)
			if missing {
				var raw string
				if err := f.store.DB().QueryRow(`SELECT p.recent_runs_json FROM review_pulls p JOIN review_workspace w ON w.project_id=p.project_id AND w.snapshot_id=p.snapshot_id WHERE p.project_id=? AND p.number=1`, f.project).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var items []map[string]any
				if json.Unmarshal([]byte(raw), &items) != nil {
					t.Fatal("invalid fixture")
				}
				for _, item := range items {
					delete(item, "source")
				}
				replaceCurrentRecentSummary(t, f, mustJSON(items))
			}
			before := historyMigrationDatabaseDigest(t, f)
			if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
				t.Fatal("valid recent summary refused", err)
			}
			if historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("integrity validation rewrote retained source")
			}
			var current PullResponse
			beforeWire := string(nativeRead(t, f, "/pulls/1", &current))
			a := nativeAttempt("after-valid-recent", "prepared")
			mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &a})
			var retained PullResponse
			if got := string(nativeRead(t, f, "/pulls/1?snapshot="+current.SnapshotID, &retained)); got != beforeWire {
				t.Fatal("publication changed original selected snapshot")
			}
			if got := string(nativeRead(t, f, oldPath, &retained)); got != oldWire {
				t.Fatal("publication changed older snapshot")
			}
		})
	}
}

func TestCurrentRecentSummaryEscapedCoverageBounds(t *testing.T) {
	f := newAPIFixture(t)
	for i := 0; i < 3; i++ {
		run := nativeRun(fmt.Sprintf("escaped-recent-%d", i))
		run.Reviewer = strings.Repeat("é", 64)
		run.Scope = strings.Repeat("🙂", 32)
		run.CoverageLimits = make([]string, 16)
		for j := range run.CoverageLimits {
			run.CoverageLimits[j] = strings.Repeat("<", 1024)
		}
		mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	}
	var stored string
	if err := f.store.DB().QueryRow(`SELECT p.recent_runs_json FROM review_pulls p JOIN review_workspace w ON w.project_id=p.project_id AND w.snapshot_id=p.snapshot_id WHERE p.project_id=? AND p.number=1`, f.project).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) <= 256<<10 {
		t.Fatal("fixture did not cover legitimate large escaped summaries", len(stored))
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal("admitted escaped coverage rejected", err)
	}
	a := nativeAttempt("after-escaped-recent", "prepared")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &a})
	t.Logf("three actual maximal escaped coverage inventories: %d recent-summary bytes", len(stored))
}

func TestCurrentRecentSummaryOversizedCanonicalScalarIsClipped(t *testing.T) {
	f, _, _ := recentSummaryGuardFixture(t)
	if _, err := f.store.DB().Exec(`UPDATE review_run_versions SET reviewer=CAST(zeroblob(1048576) AS TEXT) WHERE project_id=? AND source_run_id='recent-guard-3'`, f.project); err != nil {
		t.Fatal(err)
	}
	row := f.store.DB().QueryRowContext(t.Context(), currentRecentRunsQuery(0), f.project, int64(1))
	clipped, err := scanOrderedCurrentRun(row, 1)
	if err == nil || len(clipped.summary.Reviewer) != 129 {
		t.Fatal("canonical scalar escaped its128-byte bound plus refusal byte", len(clipped.summary.Reviewer), err)
	}
	before := historyMigrationDatabaseDigest(t, f)
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err == nil {
		t.Fatal("oversized canonical scalar passed integrity")
	}
	a := nativeAttempt("after-oversized-scalar", "prepared")
	_, err = nativePublish(f, NativePublication{Pull: nativePull(1), Attempt: &a})
	nativeErrorCode(t, err, "storage_unavailable")
	if historyMigrationDatabaseDigest(t, f) != before {
		t.Fatal("oversized scalar refusal changed stored source")
	}
}

func TestCurrentRecentSummaryMalformedOrderRefusesWithoutScanningText(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(fmt.Sprintf("staged-%t", staged), func(t *testing.T) {
			f, _, _ := recentSummaryGuardFixture(t)
			before := historyMigrationDatabaseDigest(t, f)
			tx := historyWriterTx(t, f)
			w := newHistorySnapshotWrite(f.project, 3)
			if staged {
				historyWriterSnapshot(t, tx, f.project, 3)
				if err := w.run(t.Context(), tx, nativeSource, 1, 4, nativeRun("malformed-staged-order")); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(`UPDATE review_run_memberships SET run_order='malformed-order' WHERE project_id=? AND start_revision=3`, f.project); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := tx.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(`UPDATE review_current_runs SET run_order='malformed-order' WHERE project_id=? AND id=(SELECT id FROM review_current_runs WHERE project_id=? ORDER BY run_order DESC LIMIT 1)`, f.project, f.project); err != nil {
					t.Fatal(err)
				}
			}
			_, err := w.recentCurrentRuns(t.Context(), tx, 1)
			if err == nil || strings.Contains(err.Error(), "malformed-order") || !strings.Contains(err.Error(), "NULL") {
				t.Fatal("malformed order reached numeric scanner as text", err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("malformed order read escaped rollback")
			}
		})
	}
}

func currentSummaryGuardFixture(t *testing.T) *apiFixture {
	t.Helper()
	f := newAPIFixture(t)
	pull := nativePull(1)
	pull.Attempts = []Attempt{
		currentSummaryAttempt("cli-older", "cli", "2026-10-01T00:00:00Z", ""),
		currentSummaryAttempt("cli-winner", "cli", "2026-10-01T00:01:00Z", ""),
		currentSummaryAttempt("hosted-winner", "hosted", "2026-10-01T00:02:00Z", ""),
		currentSummaryAttempt("manual-winner", "manual", "", ""),
		currentSummaryAttempt("subagent-winner", "subagent", "2026-10-01T00:03:00Z", ""),
	}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "guard-import")
	return f
}

func TestCurrentSummaryFrontValidFourChannelsAndMissingPull(t *testing.T) {
	f := currentSummaryGuardFixture(t)
	tx := historyWriterTx(t, f)
	for _, number := range []int64{1, 999} {
		if err := validateCurrentSummaryFront(t.Context(), tx, f.project, number); err != nil {
			t.Fatal("valid previous front or absent pull rejected", number, err)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	a := currentSummaryAttempt("new-native", "cli", "2026-10-01T01:00:00Z", "")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Attempt: &a})
	assertCurrentSummaryReduction(t, f, 1)
}

func TestCurrentSummaryFrontCacheDamageRefusesPublicationAtomically(t *testing.T) {
	for _, damage := range []struct{ name, sql string }{
		{"missing winner", `DELETE FROM review_current_attempt_order WHERE source_attempt_id='cli-winner'`},
		{"missing only channel", `DELETE FROM review_current_attempt_order WHERE channel='manual'`},
		{"wrong winner sort key", `UPDATE review_current_attempt_order SET sort_key=sort_key||'0' WHERE source_attempt_id='cli-winner'`},
		{"wrong winner channel", `UPDATE review_current_attempt_order SET channel='hosted' WHERE source_attempt_id='cli-winner'`},
		{"wrong winner position", `UPDATE review_current_attempt_order SET position=100 WHERE source_attempt_id='cli-winner'`},
		{"wrong winner pull", `UPDATE review_current_attempt_order SET pull_number=999 WHERE source_attempt_id='cli-winner'`},
		{"wrong run count", `UPDATE review_current_totals SET run_count=run_count+1`},
		{"wrong attempt count", `UPDATE review_current_totals SET attempt_count=attempt_count+1`},
		{"missing totals", `DELETE FROM review_current_totals`},
	} {
		t.Run(damage.name, func(t *testing.T) {
			f := currentSummaryGuardFixture(t)
			if _, err := f.store.DB().Exec(damage.sql); err != nil {
				t.Fatal(err)
			}
			before := historyMigrationDatabaseDigest(t, f)
			beforeRead := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/pulls/1", f.readToken, "", nil)
			a := currentSummaryAttempt("would-hide-corrupt-cli", "subagent", "2026-10-01T01:00:00Z", "")
			_, err := nativePublish(f, NativePublication{Pull: nativePull(1), Attempt: &a})
			nativeErrorCode(t, err, "storage_unavailable")
			if after := historyMigrationDatabaseDigest(t, f); after != before {
				t.Fatal("refused native write changed canonical bodies, caches, refs, events or metadata")
			}
			afterRead := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/pulls/1", f.readToken, "", nil)
			if afterRead.Code != beforeRead.Code || beforeRead.Code == http.StatusOK && afterRead.Body.String() != beforeRead.Body.String() {
				t.Fatal("refused native write replaced previous compact response")
			}
		})
	}
}

func TestCurrentSummaryFrontMalformedPreviousSummaryRefusesPublication(t *testing.T) {
	for _, mode := range []string{"null", "malformed", "noncanonical", "unknown field", "unknown channel", "duplicate channel", "more than four", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			f := currentSummaryGuardFixture(t)
			var before PullResponse
			nativeRead(t, f, "/pulls/1", &before)
			items := append([]AttemptSummary{}, before.Pull.LatestAttempts...)
			stored := mustJSON(items)
			switch mode {
			case "null":
				stored = "null"
			case "malformed":
				stored = "["
			case "noncanonical":
				stored = " " + stored
			case "unknown field":
				var objects []map[string]any
				if err := json.Unmarshal([]byte(stored), &objects); err != nil {
					t.Fatal(err)
				}
				objects[0]["unknown"] = true
				stored = mustJSON(objects)
			case "unknown channel":
				items[0].Channel = "unexpected"
				stored = mustJSON(items)
			case "duplicate channel":
				items[1].Channel = items[0].Channel
				stored = mustJSON(items)
			case "more than four":
				stored = mustJSON(append(items, items[0]))
			case "oversized":
				items[0].SourceAttemptID = strings.Repeat("x", 32<<10)
				stored = mustJSON(items)
			}
			if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET latest_attempts_json=? WHERE (project_id,number,version_revision) IN (SELECT m.project_id,m.number,m.version_revision FROM review_current_pulls c JOIN review_pull_memberships m ON m.project_id=c.project_id AND m.number=c.number AND m.start_revision=c.start_revision WHERE c.project_id=? AND c.number=1)`, stored, f.project); err != nil {
				t.Fatal(err)
			}
			digest := historyMigrationDatabaseDigest(t, f)
			a := currentSummaryAttempt("malformed-front-successor", "cli", "2026-10-01T01:00:00Z", "")
			_, err := nativePublish(f, NativePublication{Pull: nativePull(1), Attempt: &a})
			nativeErrorCode(t, err, "storage_unavailable")
			if historyMigrationDatabaseDigest(t, f) != digest {
				t.Fatal("refused malformed summary changed persistent state")
			}
		})
	}
}

func TestCurrentSummaryFrontLeavesNonwinningDamageToMaintenanceValidation(t *testing.T) {
	f := currentSummaryGuardFixture(t)
	if _, err := f.store.DB().Exec(`DELETE FROM review_current_attempt_order WHERE source_attempt_id='cli-older'`); err != nil {
		t.Fatal(err)
	}
	tx := historyWriterTx(t, f)
	if err := validateCurrentSummaryFront(t.Context(), tx, f.project, 1); err != nil {
		t.Fatal("front-only check unexpectedly claims full-cache inspection", err)
	}
	if err := ValidateCurrentSummaries(t.Context(), tx); err == nil {
		t.Fatal("full maintenance validation missed absent nonwinner")
	}
}
