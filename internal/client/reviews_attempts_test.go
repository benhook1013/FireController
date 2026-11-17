package client_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewAttemptsExplicitPagesPreserveSourceDurationAndNoEffects(t *testing.T) {
	f := newReviewClientFixture(t)
	snapshot := f.importSnapshot(t)
	var beforeEvents, beforeReceipts int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&beforeReceipts); err != nil {
		t.Fatal(err)
	}
	beforeCalls := f.calls.Load()
	first, err := f.client.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{PageOptions: client.PageOptions{Limit: 1}, Snapshot: snapshot})
	if err != nil || len(first.Items) != 1 || !first.HasMore || first.Cursor == "" || first.SnapshotID != snapshot || first.Items[0].Source != "explicit-import" || first.Items[0].SourceAttemptID != "attempt-zero-duration" || first.Items[0].DurationSeconds == nil || *first.Items[0].DurationSeconds != 0 {
		t.Fatal("first explicit attempt page changed source, order or measured zero", first, err)
	}
	// The fixture is already connected: one page must make exactly one read.
	if f.calls.Load()-beforeCalls != 1 {
		t.Fatal("attempt page performed hidden calls", f.calls.Load()-beforeCalls)
	}
	second, err := f.client.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{PageOptions: client.PageOptions{Limit: 1, Cursor: first.Cursor}, Snapshot: snapshot})
	if err != nil || len(second.Items) != 1 || second.HasMore || second.Cursor != "" || second.SnapshotID != first.SnapshotID || second.Items[0].SourceAttemptID != "attempt-unknown-duration" || second.Items[0].DurationSeconds != nil || second.Items[0].Source != first.Items[0].Source {
		t.Fatal("explicit second attempt page lost its tail, snapshot or unknown duration", second, err)
	}
	if f.calls.Load()-beforeCalls != 2 {
		t.Fatal("continuation performed hidden calls", f.calls.Load()-beforeCalls)
	}
	var events, receipts int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&receipts); err != nil || events != beforeEvents || receipts != beforeReceipts {
		t.Fatal("attempt reads changed durable state", err)
	}
}

func TestReviewAttemptsSelectorsFailBeforeHTTP(t *testing.T) {
	f := newReviewClientFixture(t)
	before := f.calls.Load()
	for _, test := range []struct {
		number  int64
		options client.ReviewAttemptOptions
	}{
		{0, client.ReviewAttemptOptions{}},
		{2147483648, client.ReviewAttemptOptions{}},
		{1, client.ReviewAttemptOptions{Snapshot: "bad"}},
		{1, client.ReviewAttemptOptions{Snapshot: "review_snapshot_" + strings.Repeat("A", 32)}},
		{1, client.ReviewAttemptOptions{PageOptions: client.PageOptions{Limit: -1}}},
		{1, client.ReviewAttemptOptions{PageOptions: client.PageOptions{Limit: 101}}},
		{1, client.ReviewAttemptOptions{PageOptions: client.PageOptions{Cursor: " bad"}}},
		{1, client.ReviewAttemptOptions{PageOptions: client.PageOptions{Cursor: "bad\x00"}}},
		{1, client.ReviewAttemptOptions{PageOptions: client.PageOptions{Cursor: strings.Repeat("c", 4097)}}},
	} {
		_, err := f.client.ReviewAttempts(t.Context(), test.number, test.options)
		checkError(t, err, "validation_failed")
	}
	if f.calls.Load() != before {
		t.Fatal("invalid local attempt selectors reached service")
	}
}

func TestReviewAttemptsValidateExactPageShapeAndProducerIdentity(t *testing.T) {
	path := "/api/v1/projects/project-one/reviews/pulls/1/attempts"
	item := client.ReviewAttemptItem{Source: "original producer", ReviewAttempt: client.ReviewAttempt{SourceAttemptID: "same-id", Channel: "manual", State: "unknown", HeadSHA: strings.Repeat("a", 40)}}
	page := client.ReviewAttempts{ProjectID: "project-one", Revision: 1, SnapshotID: "review_snapshot_" + strings.Repeat("b", 32), Source: "snapshot publisher", ObservedAt: "2026-10-15T12:00:00Z", RecordedAt: "2026-10-15T12:01:00Z", Items: []client.ReviewAttemptItem{item}}
	encode := func(value any) string {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	body := encode(page)
	valid := newReviewMock(t, body, path)
	got, err := valid.client.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{Snapshot: page.SnapshotID})
	if err != nil || len(got.Items) != 1 || got.Items[0].Source != item.Source || got.Source != page.Source || got.Items[0].DurationSeconds != nil {
		t.Fatal("flat attempt wire/source or unknown duration changed", got, err)
	}
	for _, test := range []struct{ name, body string }{
		{"foreign_project", strings.Replace(body, `"project-one"`, `"other"`, 1)},
		{"wrong_snapshot", strings.Replace(body, page.SnapshotID, "review_snapshot_"+strings.Repeat("c", 32), 1)},
		{"missing_source", strings.Replace(body, `,"source":"original producer"`, "", 1)},
		{"empty_source", strings.Replace(body, `"source":"original producer"`, `"source":""`, 1)},
		{"null_source", strings.Replace(body, `"source":"original producer"`, `"source":null`, 1)},
		{"overlong_source", strings.Replace(body, "original producer", strings.Repeat("s", 129), 1)},
		{"missing_duration", strings.Replace(body, `,"duration_seconds":null`, "", 1)},
		{"negative_duration", strings.Replace(body, `"duration_seconds":null`, `"duration_seconds":-1`, 1)},
		{"string_duration", strings.Replace(body, `"duration_seconds":null`, `"duration_seconds":"unknown"`, 1)},
		{"missing_state", strings.Replace(body, `,"state":"unknown"`, "", 1)},
		{"unknown_state", strings.Replace(body, `"state":"unknown"`, `"state":"ready"`, 1)},
		{"noncanonical_head", strings.Replace(body, strings.Repeat("a", 40), strings.Repeat("A", 40), 1)},
		{"negative_revision", strings.Replace(body, `"revision":1`, `"revision":-1`, 1)},
		{"unknown_field", strings.TrimSuffix(body, "}") + `,"merge_ready":true}`},
		{"unknown_item_field", strings.Replace(body, `"source_attempt_id":"same-id"`, `"source_attempt_id":"same-id","private_capture":"secret"`, 1)},
		{"duplicate_field", strings.Replace(body, `"revision":1`, `"revision":1,"revision":1`, 1)},
		{"null_items", strings.Replace(body, `"items":[`+encode(item)+`]`, `"items":null`, 1)},
		{"missing_items", strings.Replace(body, `,"items":[`+encode(item)+`]`, "", 1)},
		{"missing_has_more", strings.Replace(body, `,"has_more":false`, "", 1)},
		{"more_without_cursor", strings.Replace(body, `"has_more":false`, `"has_more":true`, 1)},
		{"cursor_without_more", strings.TrimSuffix(body, "}") + `,"cursor":"continuation"}`},
		{"oversized_response", strings.Repeat(" ", (1<<20)+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.body == body {
				t.Fatal("malformed fixture did not change input")
			}
			mock := newReviewMock(t, test.body, path)
			_, err := mock.client.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{Snapshot: page.SnapshotID})
			checkError(t, err, "invalid_response")
		})
	}
	page.Items = append(page.Items, item)
	duplicate := newReviewMock(t, encode(page), path)
	_, err = duplicate.client.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{})
	checkError(t, err, "invalid_response")
	page.Items[1].Source = "another producer"
	mixed := newReviewMock(t, encode(page), path)
	got, err = mixed.client.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{})
	if err != nil || len(got.Items) != 2 || got.Items[0].SourceAttemptID != got.Items[1].SourceAttemptID || got.Items[0].Source == got.Items[1].Source {
		t.Fatal("distinct producers' identical external attempt IDs were collapsed", got, err)
	}
	_, err = mixed.client.ReviewAttempts(t.Context(), 1, client.ReviewAttemptOptions{PageOptions: client.PageOptions{Limit: 1}})
	checkError(t, err, "invalid_response")
}

func TestReviewCompactSummaryAcceptsInt64LifetimeCounts(t *testing.T) {
	f := newReviewClientFixture(t)
	f.importSnapshot(t)
	pull, err := f.client.ReviewPull(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	pull.Pull.RunCount, pull.Pull.AttemptCount = math.MaxInt64, math.MaxInt64
	pull.Pull.RunCounts = client.ReviewChannelRunCounts{Manual: math.MaxInt64}
	body, err := json.Marshal(pull)
	if err != nil {
		t.Fatal(err)
	}
	mock := newReviewMock(t, string(body), "/api/v1/projects/project-one/reviews/pulls/1")
	got, err := mock.client.ReviewPull(t.Context(), 1, "")
	if err != nil || got.Pull.RunCount != math.MaxInt64 || got.Pull.AttemptCount != math.MaxInt64 {
		t.Fatal("compact totals retained old per-page bounds", got, err)
	}
}
