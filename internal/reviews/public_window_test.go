package reviews

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/publicsite"
)

const publicWindowSource = "PRIVATE_ARCHIVE_PRODUCER"
const publicWindowSalt = "public_salt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// Seed a trusted retained archive through the owning immutable history writer.
// This bypasses observation/native admission limits only in the fixture; it
// creates no executor journal, captured evidence or native coverage authority.
func publicWindowArchive(t *testing.T, f *apiFixture, revision int64, queue []PullInput) string {
	t.Helper()
	tx := historyWriterTx(t, f)
	id := fmt.Sprintf("public_window_snapshot_%d", revision)
	_, err := tx.ExecContext(t.Context(), `INSERT INTO review_snapshots(project_id,snapshot_id,revision,repository_id,source,observed_at,recorded_at,payload_sha256) VALUES(?,?,?,701,?,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',?)`, f.project, id, revision, publicWindowSource, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	write := newHistorySnapshotWrite(f.project, revision)
	req := reviewRequest(revision-1, queue)
	req.Source = publicWindowSource
	if err := write.observation(t.Context(), tx, req); err != nil {
		t.Fatal(err)
	}
	if err := write.applyCurrent(t.Context(), tx, id, revision-1); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHistoryReferences(t.Context(), tx); err != nil {
		t.Fatal("trusted archive has invalid retained references", err)
	}
	if err := ValidateCurrentSummaries(t.Context(), tx); err != nil {
		t.Fatal("trusted archive has invalid derived current rows", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func publicWindowRun(number int64, channel string, order int) RunInput {
	return RunInput{SourceRunID: fmt.Sprintf("PRIVATE_SOURCE_%d_%s_%d", number, channel, order), Channel: channel, Outcome: "completed", Finalized: true, Complete: true,
		StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00.123Z", Scope: "PRIVATE_SCOPE", Reviewer: "PRIVATE_REVIEWER", CoverageLimits: []string{"PRIVATE_COVERAGE"}}
}

func publicWindowRead(t *testing.T, f *apiFixture, manifest publicsite.Manifest) []publicsite.ReviewFacts {
	t.Helper()
	tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	facts, err := PublicSiteReviewFacts(t.Context(), tx, f.project, manifest, publicWindowSalt)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

func publicWindowKey(project string, number int64, run RunInput) string {
	key, _ := hex.DecodeString(strings.TrimPrefix(publicWindowSalt, "public_salt_"))
	mac := hmac.New(sha256.New, key)
	for _, value := range []string{project, fmt.Sprint(number), publicWindowSource, run.SourceRunID, run.Channel} {
		fmt.Fprintf(mac, "%d:%s", len(value), value)
	}
	return "public_round_" + hex.EncodeToString(mac.Sum(nil)[:16])
}

func TestPublicSiteWindowIndependentEligibleChannelBounds(t *testing.T) {
	f := newAPIFixture(t)
	queue := []PullInput{nativePull(1), nativePull(2), nativePull(3), nativePull(99)}
	for i, counts := range [][2]int{{104, 102}, {210, 3}, {100, 0}, {1, 0}} {
		for channelIndex, channel := range []string{"cli", "hosted"} {
			for order := range counts[channelIndex] {
				queue[i].Runs = append(queue[i].Runs, publicWindowRun(queue[i].Number, channel, order))
			}
		}
	}
	queue[0].Runs[103].Complete = false
	queue[0].Runs[103].Found, queue[0].Runs[103].Accepted, queue[0].Runs[103].Routed = 3, 1, 1
	queue[0].Runs[102].StartedAt, queue[0].Runs[102].FinishedAt = "", ""
	// More than 100 newer private drafts must not create a public has-more flag
	// or displace the exactly 100 eligible CLI rounds on PR3.
	for i := range 101 {
		draft := publicWindowRun(3, "cli", 100+i)
		draft.Finalized = false
		queue[2].Runs = append(queue[2].Runs, draft)
	}
	for _, channel := range []string{"manual", "subagent"} {
		queue[0].Runs = append(queue[0].Runs, publicWindowRun(1, channel, 0))
	}
	failed := publicWindowRun(1, "hosted", 102)
	failed.Outcome, failed.Finalized = "failed", false
	queue[0].Runs = append(queue[0].Runs, failed)
	first := publicWindowArchive(t, f, 1, queue)
	manifest := publicsite.Manifest{Reviews: []publicsite.EnrolledReview{{Number: 1}, {Number: 2}, {Number: 3}, {Number: 77}}}
	before := historyMigrationDatabaseDigest(t, f)
	facts := publicWindowRead(t, f, manifest)
	if len(facts) != 3 || facts[0].Number != 1 || facts[1].Number != 2 || facts[2].Number != 3 || before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("source read changed state or selected missing/unenrolled PRs")
	}
	if len(facts[0].CLI) != 100 || len(facts[0].Hosted) != 100 || !facts[0].CLIHasMore || !facts[0].HostedHasMore {
		t.Fatal("independent overflow was not represented", len(facts[0].CLI), len(facts[0].Hosted))
	}
	if len(facts[1].CLI) != 100 || len(facts[1].Hosted) != 3 || !facts[1].CLIHasMore || facts[1].HostedHasMore {
		t.Fatal("busy CLI channel starved Hosted or invented its overflow")
	}
	if len(facts[2].CLI) != 100 || len(facts[2].Hosted) != 0 || facts[2].CLIHasMore || facts[2].HostedHasMore {
		t.Fatal("private drafts affected eligible selection or coverage")
	}
	for i, counts := range [][2]int{{104, 102}, {210, 3}, {100, 0}} {
		for channelIndex, channel := range []string{"cli", "hosted"} {
			rounds := facts[i].CLI
			if channel == "hosted" {
				rounds = facts[i].Hosted
			}
			for position, round := range rounds {
				want := publicWindowRun(int64(i+1), channel, counts[channelIndex]-1-position)
				if round.Key != publicWindowKey(f.project, int64(i+1), want) || !round.Completed || !round.Finalized {
					t.Fatal("source window lost position or stable opaque identity", i, channel, position)
				}
			}
		}
	}
	latest, unknown := facts[0].CLI[0], facts[0].CLI[1]
	if latest.Complete || latest.Found != 3 || latest.Accepted != 1 || latest.Routed != 1 || latest.DurationMilliseconds == nil || *latest.DurationMilliseconds != 60123 || unknown.ObservedAt != "" || unknown.DurationMilliseconds != nil {
		t.Fatal("incomplete finalized summary/counts or unknown duration semantics changed", latest, unknown)
	}
	encoded, _ := json.Marshal(facts)
	for _, sentinel := range []string{"PRIVATE_", f.project, first, publicWindowSalt, "source_run_id", "reviewer", "coverage_limits"} {
		if bytes.Contains(encoded, []byte(sentinel)) {
			t.Fatal("source adapter exposed private data", sentinel)
		}
	}

	// Private metadata, reviewer corrections and an active attempt
	// advance private versions without altering any selected public fact/flag.
	queue[0].Title = "PRIVATE_UPDATED_TITLE"
	queue[0].Runs[103].Reviewer = "PRIVATE_UPDATED_REVIEWER"
	queue[2].Runs[200].Reviewer = "PRIVATE_UPDATED_DRAFT_REVIEWER"
	queue[0].Attempts = []Attempt{nativeAttempt("PRIVATE_ACTIVE_ATTEMPT", "active")}
	publicWindowArchive(t, f, 2, queue)
	if got := publicWindowRead(t, f, manifest); !reflect.DeepEqual(got, facts) {
		t.Fatal("private-only updates changed bounded public facts")
	}
	publicWindowArchive(t, f, 3, []PullInput{queue[0], queue[2], queue[3]})
	omitted := publicWindowRead(t, f, manifest)
	if len(omitted) != 2 || omitted[0].Number != 1 || omitted[1].Number != 3 {
		t.Fatal("missing current PR produced invented public facts")
	}
	var retained int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_runs WHERE project_id=? AND snapshot_id=? AND pull_number=2`, f.project, first).Scan(&retained); err != nil || retained != 213 {
		t.Fatal("windowing/omission removed retained private history", retained, err)
	}
}

func TestPublicSiteWindowQueriesUseCurrentPositionIndexes(t *testing.T) {
	f := newAPIFixture(t)
	for _, channel := range []string{"cli", "hosted"} {
		rows, err := f.store.DB().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+publicSiteChannelRoundsSQL, f.project, 1, channel, 101)
		if err != nil {
			t.Fatal(err)
		}
		var plan string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan += detail + "\n"
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !strings.Contains(plan, "SEARCH c USING INDEX") || !strings.Contains(plan, "project_id=? AND pull_number=?") || strings.Contains(plan, "SCAN ") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatal("source window did not use scoped current-position access", plan, err)
		}
	}
}
