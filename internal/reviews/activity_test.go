package reviews

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func activityHTTP(t *testing.T, f *apiFixture, query string) ActivityResponse {
	t.Helper()
	w := f.call(http.MethodGet, importBase(f)+"/activity"+query, f.readToken, "", nil)
	var result ActivityResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("activity: %d %s", w.Code, w.Body.String())
	}
	return result
}
func activityTestRun(id, channel, at string) RunInput {
	return RunInput{SourceRunID: id, Channel: channel, Scope: "Original retained comparison", Outcome: "completed", Complete: true, Attributable: true, StartedAt: at, FinishedAt: at, Findings: []Finding{}, CoverageLimits: []string{}}
}
func activityTestAttempt(id, state, reason, at string) Attempt {
	return Attempt{SourceAttemptID: id, Channel: "cli", State: state, Reason: reason, StartedAt: at, FinishedAt: at}
}

func TestActivityEmptyDefaultsAndStrictWindow(t *testing.T) {
	f := newAPIFixture(t)
	before := historyMigrationDatabaseDigest(t, f)
	empty := activityHTTP(t, f, "")
	asOf, err := time.Parse(time.RFC3339Nano, empty.AsOf)
	if err != nil {
		t.Fatal(err)
	}
	since, _ := time.Parse(time.RFC3339Nano, empty.Since)
	until, _ := time.Parse(time.RFC3339Nano, empty.Until)
	if empty.ProjectID != f.project || empty.Timezone != "UTC" || !until.Equal(asOf) || until.Sub(since) != 24*time.Hour || empty.Totals != (ActivityCounts{}) || empty.Diagnostics != (ActivityDiagnostics{}) || empty.Coverage.Basis != "current_retained_results" || empty.Buckets == nil || len(empty.Coverage.Limits) == 0 {
		t.Fatal(empty)
	}
	for _, bucket := range empty.Buckets {
		if bucket.Hosted != 0 || bucket.CLI != 0 {
			t.Fatal(bucket)
		}
	}
	for _, query := range []string{"?hours=0", "?hours=169", "?hours=01", "?hours=1&hours=2", "?since=2025-01-01T00:00:00Z", "?until=2025-01-01T00:00:00Z", "?since=x&until=x", "?since=2025-01-01T00:00:00Z&until=2025-01-09T00:00:00Z", "?since=2025-01-02T00:00:00Z&until=2025-01-01T00:00:00Z", "?since=2099-01-01T00:00:00Z&until=2099-01-02T00:00:00Z", "?hours=1&since=2025-01-01T00:00:00Z&until=2025-01-02T00:00:00Z", "?timezone=Local", "?timezone=not_a_zone", "?timezone=", "?pull_number=0", "?pull_number=2147483648", "?private=true"} {
		if w := f.call(http.MethodGet, importBase(f)+"/activity"+query, f.readToken, "", nil); w.Code != 400 {
			t.Fatalf("query=%s status=%d", query, w.Code)
		}
	}
	if w := f.call(http.MethodGet, importBase(f)+"/activity", "", "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("activity read changed retained data")
	}
}

func TestActivityOriginalFinishOffsetsDSTAndSeparateDiagnostics(t *testing.T) {
	f := newAPIFixture(t)
	merged, closed, retired := nativePull(1), nativePull(2), nativePull(3)
	merged.State, closed.State, retired.State = "merged", "closed", "open"
	merged.Runs = []RunInput{activityTestRun("hosted-offset", "hosted", "2025-11-02T17:30:00+13:00"), activityTestRun("cli-before-fold", "cli", "2025-11-02T05:00:00.000000001Z")}
	merged.Runs[0].Finalized = false
	closed.Runs = []RunInput{activityTestRun("cli-after-fold", "cli", "2025-11-02T01:00:00-05:00"), activityTestRun("exact-exclusive-until", "cli", "2025-11-02T07:15:00Z"), activityTestRun("before-inclusive-since", "hosted", "2025-11-02T04:14:59.999999999Z")}
	retired.Runs = []RunInput{activityTestRun("just-before-until", "hosted", "2025-11-02T07:14:59.999999999Z"), activityTestRun("incomplete", "cli", "2025-11-02T06:30:00Z"), activityTestRun("failed-capture", "cli", "2025-11-02T06:30:00Z"), {SourceRunID: "undated-source", Channel: "hosted", Scope: "Incomplete original", Outcome: "unknown"}}
	retired.Runs[1].Complete = false
	retired.Runs[2].Outcome = "failed"
	retired.Runs[2].Complete = false
	retired.Attempts = []Attempt{activityTestAttempt("failed", "failed", "", "2025-11-02T06:30:00Z"), activityTestAttempt("held", "held", "source_unavailable", "2025-11-02T06:30:00Z"), activityTestAttempt("cooldown", "rate_limited", "hosted_allowance_cooldown", "2025-11-02T06:30:00Z"), activityTestAttempt("interrupted", "failed", "execution_interrupted", "2025-11-02T06:30:00Z"), activityTestAttempt("uncertain", "uncertain", "publication_not_proven", "2025-11-02T06:30:00Z"), activityTestAttempt("cancelled", "cancelled", "", "2025-11-02T06:30:00Z"), activityTestAttempt("active", "active", "", "2025-11-02T06:30:00Z"), activityTestAttempt("completed-attempt", "completed", "", "2025-11-02T06:30:00Z"), {SourceAttemptID: "undated-attempt", Channel: "cli", State: "held"}}
	first := historyObservationPost(t, f, reviewRequest(0, []PullInput{merged, closed, retired}), "activity-original")
	edit := queueEditHTTP(f, queueEditRequest(first.Revision, nil, []RetiredPull{{Number: 3, Reason: "defunct"}}), "retire-queue")
	if edit.Code != 201 {
		t.Fatal(edit.Code, edit.Body.String())
	}
	query := "?since=2025-11-02T04:15:00Z&until=2025-11-02T07:15:00Z&timezone=America%2FNew_York"
	before := historyMigrationDatabaseDigest(t, f)
	result := activityHTTP(t, f, query)
	if result.Totals != (ActivityCounts{Hosted: 2, CLI: 2}) || result.AveragesPerHour != (ActivityAverages{Hosted: 2.0 / 3, CLI: 2.0 / 3}) || result.Diagnostics != (ActivityDiagnostics{Failed: 1, Held: 2, Interrupted: 1, Unproven: 2}) || result.Coverage.ExcludedIncompleteResults != 1 || result.Coverage.UndatedResults != 1 || result.Coverage.UndatedAttempts != 1 {
		t.Fatal(result)
	}
	labels := []string{}
	for _, bucket := range result.Buckets {
		labels = append(labels, bucket.Label)
	}
	if !reflect.DeepEqual(labels, []string{"2025-11-02T00:00:00-04:00", "2025-11-02T01:00:00-04:00", "2025-11-02T01:00:00-05:00", "2025-11-02T02:00:00-05:00"}) || !result.Buckets[0].Partial || result.Buckets[1].Partial || result.Buckets[2].Partial || !result.Buckets[3].Partial {
		t.Fatal("DST/partial hours", result.Buckets)
	}
	if result.Buckets[0].Hosted != 1 || result.Buckets[1].CLI != 1 || result.Buckets[2].CLI != 1 || result.Buckets[3].Hosted != 1 {
		t.Fatal(result.Buckets)
	}
	selected := activityHTTP(t, f, query+"&pull_number=3")
	if selected.PullNumber != 3 || selected.Totals != (ActivityCounts{Hosted: 1}) {
		t.Fatal("retired PR excluded", selected)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "Original retained") || strings.Contains(string(raw), "failed-capture") {
		t.Fatal("activity exposed source text")
	}
	if before != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("activity mutated history")
	}
	// The snapshot is recorded now; its records' original finish dates stay in 2025.
	if now := activityHTTP(t, f, "?hours=1"); now.Totals != (ActivityCounts{}) {
		t.Fatal("snapshot time substituted for original finish", now)
	}
}

func TestActivityCurrentIdentitySurvivesAdjudicationVersionsAndIsolation(t *testing.T) {
	f := newAPIFixture(t)
	run := nativeUndecidedRun("same-provider-result", 1)
	first := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	original := nativeSnapshotBytes(t, f, first.ID)
	input := nativeAdjudicationInput(run)
	decideNativeFinding(&input.Findings[0], "accepted")
	if _, changed, err := applyNativeAdjudication(f, input); err != nil || !changed {
		t.Fatal(changed, err)
	}
	input.Finalized = true
	input.FrozenCounts = &NativeCounts{Found: 1, Accepted: 1}
	if _, changed, err := applyNativeAdjudication(f, input); err != nil || !changed {
		t.Fatal(changed, err)
	}
	result := activityHTTP(t, f, "?since=2026-01-01T00:00:00Z&until=2026-01-01T01:00:00Z")
	if result.Totals.CLI != 1 || nativeSnapshotBytes(t, f, first.ID) != original {
		t.Fatal("versions counted twice or original bytes changed", result)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO projects VALUES('activity-other','Other',702,'owner/other')`); err != nil {
		t.Fatal(err)
	}
	if w := f.call(http.MethodGet, "/api/v1/projects/activity-other/reviews/activity", f.readToken, "", nil); w.Code != 403 {
		t.Fatal("cross-project activity disclosed", w.Code)
	}
}

func TestActivityCandidateLimitRefusesWithoutPartialTotals(t *testing.T) {
	f := newAPIFixture(t)
	runs, attempts := nativePull(1), nativePull(2)
	for index := 0; index <= activityRowLimit; index++ {
		runs.Runs = append(runs.Runs, activityTestRun(fmt.Sprintf("cap-run-%d", index), "cli", "2025-01-01T00:30:00Z"))
		attempts.Attempts = append(attempts.Attempts, activityTestAttempt(fmt.Sprintf("cap-attempt-%d", index), "failed", "", "2025-01-01T00:30:00Z"))
	}
	publicWindowArchive(t, f, 1, []PullInput{runs, attempts})
	query := "?since=2025-01-01T00:00:00Z&until=2025-01-01T01:00:00Z"
	for _, pull := range []string{"1", "2"} {
		w := f.call(http.MethodGet, importBase(f)+"/activity"+query+"&pull_number="+pull, f.readToken, "", nil)
		var body struct {
			Error  struct{ Code, Message string }
			Totals any
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Totals != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		var envelope map[string]json.RawMessage
		if json.Unmarshal(w.Body.Bytes(), &envelope) != nil || len(envelope) != 2 || envelope["error"] == nil || envelope["request_id"] == nil {
			t.Fatal("capacity/deadline refusal returned partial activity", w.Body.String())
		}
		if w.Code == 409 {
			if body.Error.Code != "review_activity_limit" || !strings.Contains(body.Error.Message, "Narrow") {
				t.Fatal(w.Body.String())
			}
		} else if w.Code != 503 || body.Error.Code != "overloaded" {
			t.Fatal(w.Code, w.Body.String())
		}
		t.Logf("PR %s: fail-closed activity refusal HTTP %d / %s", pull, w.Code, body.Error.Code)
	}
	empty := activityHTTP(t, f, "?since=2025-01-02T00:00:00Z&until=2025-01-02T01:00:00Z&pull_number=3")
	if empty.Totals != (ActivityCounts{}) || empty.Diagnostics != (ActivityDiagnostics{}) {
		t.Fatal(empty)
	}
}

func TestActivitySupportedOriginalOffsets(t *testing.T) {
	f := newAPIFixture(t)
	pull := nativePull(1)
	instant := time.Date(2025, 1, 1, 12, 34, 0, 0, time.UTC)
	for index, offset := range []int{14 * 3600, 23*3600 + 59*60, -23*3600 - 59*60, 24 * 3600, -24 * 3600} {
		at := instant.In(time.FixedZone("original", offset)).Format(time.RFC3339Nano)
		pull.Runs = append(pull.Runs, activityTestRun(fmt.Sprintf("offset-%d", index), "cli", at))
		pull.Attempts = append(pull.Attempts, activityTestAttempt(fmt.Sprintf("offset-attempt-%d", index), "held", "source_unavailable", at))
	}
	historyObservationPost(t, f, reviewRequest(0, []PullInput{pull}), "original-offsets")
	result := activityHTTP(t, f, "?since=2025-01-01T12:00:00Z&until=2025-01-01T13:00:00Z")
	if result.Totals.CLI != 5 || result.Diagnostics.Held != 5 {
		t.Fatal("supported original offsets were silently omitted", result.Totals)
	}
}

func TestActivityUsesProvenNativeObserverReconciliationNotGuessedImportAliases(t *testing.T) {
	f := newAPIFixture(t)
	external, prior := hostedReconciliationExternal(t, f, 1)
	query := "?since=2026-10-20T00:00:00Z&until=2026-10-20T01:00:00Z"
	if got := activityHTTP(t, f, query); got.Totals.Hosted != 1 {
		t.Fatal(got)
	}
	original := nativeSnapshotBytes(t, f, prior.ID)
	replacement := activityTestRun("canonical-native", "hosted", external.FinishedAt)
	replacement.HeadSHA = external.HeadSHA
	native := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &replacement, ExternalHostedSourceID: external.SourceRunID})
	if got := activityHTTP(t, f, query); got.Totals.Hosted != 1 || nativeSnapshotBytes(t, f, prior.ID) != original {
		t.Fatal("proven observer alias counted twice or historical source changed", got)
	}
	imported := nativePull(1)
	imported.Runs = []RunInput{activityTestRun(external.SourceRunID, "hosted", external.FinishedAt)}
	transferApplyFor(t, f, transferRequest(native.Revision, TransferGroup{Source: "separate-imported-producer", Pulls: []PullInput{imported}}), "separate-producer")
	if got := activityHTTP(t, f, query); got.Totals.Hosted != 2 {
		t.Fatal("distinct imported provenance was guessed away", got)
	}
}
