package reviews

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestAttemptReasonRetainsArchiveSummaryAndPinnedHistory(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	first := nativeAttempt("older-uncertain", "uncertain")
	first.Reason = "hosted_finished_inventory_unproven"
	p.Attempts = []Attempt{first}
	snapshot := f.observe(t, reviewRequest(0, []PullInput{p}), "reason-original")["id"].(string)
	pinned := nativeRead(t, f, "/pulls/1/attempts?snapshot="+snapshot, &AttemptsResponse{})
	second := nativeAttempt("newer-held", "held")
	second.StartedAt = "2026-01-01T00:01:00Z"
	second.Reason = "repository_changed"
	p.Attempts = nil
	mustNativePublish(t, f, NativePublication{Pull: p, Attempt: &second})
	got := metadataRead(t, f, 1, "")
	if len(got.Pull.LatestAttempts) != 1 || got.Pull.LatestAttempts[0].Reason != second.Reason {
		t.Fatal("latest reason missing", got.Pull.LatestAttempts)
	}
	var archive AttemptsResponse
	nativeRead(t, f, "/pulls/1/attempts", &archive)
	reasons := map[string]string{}
	for _, a := range archive.Items {
		reasons[a.SourceAttemptID] = a.Reason
	}
	if reasons[first.SourceAttemptID] != first.Reason || reasons[second.SourceAttemptID] != second.Reason {
		t.Fatal("older reason lost", reasons)
	}
	if after := nativeRead(t, f, "/pulls/1/attempts?snapshot="+snapshot, &AttemptsResponse{}); !bytes.Equal(pinned, after) {
		t.Fatal("pinned old reason changed")
	}
	if err := ValidateAttemptReasonsHistory(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	// Current JSON and authoritative typed versions must remain coherent.
	if _, err := f.store.DB().Exec(`UPDATE review_attempt_versions SET reason='different_safe_code' WHERE source_attempt_id=?`, second.SourceAttemptID); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err == nil {
		t.Fatal("summary mismatch accepted")
	}
}

func TestAttemptReasonRefusesMalformedInputAndRetainedValues(t *testing.T) {
	for _, tc := range []struct{ state, reason string }{{"uncertain", "provider /private/path"}, {"uncertain", "UPPER"}, {"held", strings.Repeat("a", 129)}, {"active", "looks_safe"}, {"completed", "looks_safe"}} {
		t.Run(tc.state+"/"+tc.reason[:min(len(tc.reason), 12)], func(t *testing.T) {
			f := newAPIFixture(t)
			p := nativePull(1)
			a := nativeAttempt("invalid-reason", tc.state)
			a.Reason = tc.reason
			p.Attempts = []Attempt{a}
			before := importDatabaseState(t, f)
			w := f.call(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "invalid-reason", reviewRequest(0, []PullInput{p}))
			if w.Code != 400 || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
				t.Fatal("invalid diagnostic admitted", w.Code)
			}
		})
	}
	f := newAPIFixture(t)
	p := nativePull(1)
	p.Attempts = []Attempt{nativeAttempt("legacy-no-reason", "held")}
	f.observe(t, reviewRequest(0, []PullInput{p}), "old-shape")
	before := importDatabaseState(t, f)
	for _, invalid := range []string{`null`, ` null `, `""`} {
		raw := strings.Replace(mustJSON(reviewRequest(1, []PullInput{p})), `"state":"held"`, `"state":"held","reason":`+invalid, 1)
		if w := f.call(http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "invalid-wire-reason", json.RawMessage(raw)); w.Code != 400 || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
			t.Fatal("null/empty diagnostic accepted", w.Code)
		}
	}
	body := nativeRead(t, f, "/pulls/1/attempts", &AttemptsResponse{})
	if bytes.Contains(body, []byte(`"reason"`)) {
		t.Fatal("absent legacy reason became present")
	}
	if _, err := f.store.DB().Exec(`UPDATE review_attempt_versions SET reason='unsafe/path'`); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAttemptReasonsHistory(t.Context(), f.store.DB()); err == nil {
		t.Fatal("unsafe retained code accepted")
	}
	if w := f.call(http.MethodGet, "/api/v1/projects/"+f.project+"/reviews/pulls/1/attempts", f.readToken, "", nil); w.Code != 500 {
		t.Fatal("unsafe retained archive read", w.Code)
	}
}

func TestAttemptReasonPopulatedSchema7UpgradeKeepsOldWire(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	p.Attempts = []Attempt{nativeAttempt("schema-seven-old", "uncertain"), nativeAttempt("schema-seven-new", "held")}
	p.Attempts[1].StartedAt = "2026-01-01T00:01:00Z"
	snapshot := f.observe(t, reviewRequest(0, []PullInput{p}), "schema-seven")["id"].(string)
	removeReasonForPre8Fixture(t, f.store)
	if _, err := f.store.DB().Exec(`UPDATE metadata SET value='7' WHERE key='schema:reviews'`); err != nil {
		t.Fatal(err)
	}
	routes := []string{"/pulls/1", "/pulls/1/attempts?snapshot=" + snapshot}
	before := make([][]byte, len(routes))
	for i, path := range routes {
		before[i] = []byte(historyMigrationRead(t, f, path).Body)
	}
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	for i, path := range routes {
		if after := []byte(historyMigrationRead(t, f, path).Body); !bytes.Equal(before[i], after) {
			t.Fatal("upgrade changed old HTTP wire", path)
		}
	}
	var known int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_attempt_versions WHERE reason=''`).Scan(&known); err != nil || known != 2 {
		t.Fatal("old absence not retained", known, err)
	}
	after := historyMigrationDatabaseDigest(t, f)
	if err := EnsureSchema(context.Background(), f.store); err != nil || after != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("current restart changed data", err)
	}
}

func TestAttemptReasonHistoricalSummaryGrammarIsValidated(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	a := nativeAttempt("historical-diagnostic", "uncertain")
	a.Reason = "safe_old_code"
	p.Attempts = []Attempt{a}
	f.observe(t, reviewRequest(0, []PullInput{p}), "historical-reason")
	p.Attempts = nil
	b := nativeAttempt("current-diagnostic", "held")
	b.Reason = "safe_new_code"
	b.StartedAt = "2026-01-01T00:01:00Z"
	mustNativePublish(t, f, NativePublication{Pull: p, Attempt: &b})
	var original string
	if err := f.store.DB().QueryRow(`SELECT latest_attempts_json FROM review_pull_versions WHERE version_revision=1`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{`"unsafe/path"`, `null`, `""`} {
		changed := strings.Replace(original, `"safe_old_code"`, invalid, 1)
		if changed == original {
			t.Fatal("historical fixture did not change")
		}
		if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET latest_attempts_json=? WHERE version_revision=1`, changed); err != nil {
			t.Fatal(err)
		}
		if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
			t.Fatal("fixture accidentally damaged current reduction", err)
		}
		if err := ValidateAttemptReasonsHistory(t.Context(), f.store.DB()); err == nil {
			t.Fatal("unsafe/null/empty historical summary reason accepted")
		}
	}
}
