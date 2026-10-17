package reviews

import (
	"encoding/json"
	"strings"
	"testing"
)

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
			var pull PullResponse
			beforeWire := nativeRead(t, f, "/pulls/1", &pull)
			a := currentSummaryAttempt("would-hide-corrupt-cli", "subagent", "2026-10-01T01:00:00Z", "")
			_, err := nativePublish(f, NativePublication{Pull: nativePull(1), Attempt: &a})
			nativeErrorCode(t, err, "storage_unavailable")
			if after := historyMigrationDatabaseDigest(t, f); after != before {
				t.Fatal("refused native write changed canonical bodies, caches, refs, events or metadata")
			}
			if afterWire := nativeRead(t, f, "/pulls/1", &pull); string(afterWire) != string(beforeWire) {
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
