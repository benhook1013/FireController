package reviews

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func metadataRead(t *testing.T, f *apiFixture, number int64, snapshot string) PullResponse {
	t.Helper()
	route := fmt.Sprintf("/pulls/%d", number)
	if snapshot != "" {
		route += "?snapshot=" + url.QueryEscape(snapshot)
	}
	var got PullResponse
	nativeRead(t, f, route, &got)
	return got
}
func TestPullMetadataHTTPUnknownZeroAndNativeHeadRetention(t *testing.T) {
	f := newAPIFixture(t)
	pull := nativePull(1)
	zero := false
	pull.Diff = &PullDiff{}
	pull.IsDraft = &zero
	pull.MergedAt = "2026-01-01T00:02:03.123456789Z"
	first := f.observe(t, reviewRequest(0, []PullInput{pull, nativePull(2)}), "metadata-initial")["id"].(string)
	saved := metadataRead(t, f, 1, first)
	if saved.Pull.Diff == nil || *saved.Pull.Diff != (PullDiff{}) || saved.Pull.IsDraft == nil || *saved.Pull.IsDraft || saved.Pull.MergedAt != pull.MergedAt {
		t.Fatal("known zero/false/time lost", saved.Pull)
	}
	unknown := metadataRead(t, f, 2, "")
	if unknown.Pull.Diff != nil || unknown.Pull.IsDraft != nil || unknown.Pull.MergedAt != "" {
		t.Fatal("unknown metadata fabricated")
	}
	body := nativeRead(t, f, "/pulls/1?snapshot="+url.QueryEscape(first), &PullResponse{})
	incoming := pull
	incoming.Diff = nil
	incoming.IsDraft = nil
	incoming.MergedAt = ""
	a := nativeAttempt("metadata-wait", "prepared")
	mustNativePublish(t, f, NativePublication{Pull: incoming, Attempt: &a})
	same := metadataRead(t, f, 1, "")
	if !reflect.DeepEqual(same.Pull.Diff, pull.Diff) || !reflect.DeepEqual(same.Pull.IsDraft, pull.IsDraft) || same.Pull.MergedAt != pull.MergedAt {
		t.Fatal("unrelated native attempt discarded known metadata")
	}
	incoming.HeadSHA = strings.Repeat("c", 40)
	incoming.State = "open"
	a = nativeAttempt("metadata-new-head", "prepared")
	a.HeadSHA = incoming.HeadSHA
	mustNativePublish(t, f, NativePublication{Pull: incoming, Attempt: &a})
	changed := metadataRead(t, f, 1, "")
	if changed.Pull.Diff != nil || changed.Pull.IsDraft != nil || changed.Pull.MergedAt != "" {
		t.Fatal("new head inherited old metadata")
	}
	incoming.Diff = &PullDiff{3, 5, 7}
	draft := true
	incoming.IsDraft = &draft
	a = nativeAttempt("metadata-fresh-head", "prepared")
	a.HeadSHA = incoming.HeadSHA
	mustNativePublish(t, f, NativePublication{Pull: incoming, Attempt: &a})
	fresh := metadataRead(t, f, 1, "")
	if !reflect.DeepEqual(fresh.Pull.Diff, incoming.Diff) || fresh.Pull.IsDraft == nil || !*fresh.Pull.IsDraft {
		t.Fatal("fresh same-head known facts lost")
	}
	if !reflect.DeepEqual(body, nativeRead(t, f, "/pulls/1?snapshot="+url.QueryEscape(first), &PullResponse{})) {
		t.Fatal("pinned metadata changed")
	}
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if got := metadataRead(t, f, 1, ""); !reflect.DeepEqual(got.Pull.Diff, incoming.Diff) {
		t.Fatal("startup lost metadata")
	}
	if err := ValidatePullMetadataHistory(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}
func TestPullMetadataHTTPInvalidAndIsolation(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	base := "/api/v1/projects/" + f.project + "/reviews/observations"
	cases := []map[string]any{{"diff": map[string]any{}}, {"diff": map[string]any{"files": 0, "additions": 0}}, {"diff": map[string]any{"files": 0, "additions": nil, "deletions": 0}}, {"diff": map[string]any{"files": -1, "additions": 0, "deletions": 0}}, {"diff": map[string]any{"files": 2147483648, "additions": 0, "deletions": 0}}, {"diff": map[string]any{"files": 1.5, "additions": 0, "deletions": 0}}, {"is_draft": "false"}, {"merged_at": "not-a-time"}, {"state": "open", "merged_at": "2026-01-01T00:00:00Z"}}
	raw, _ := json.Marshal(reviewRequest(0, []PullInput{p}))
	for i, change := range cases {
		var document map[string]any
		json.Unmarshal(raw, &document)
		item := document["queue"].([]any)[0].(map[string]any)
		for key, value := range change {
			item[key] = value
		}
		w := f.call("POST", base, f.admin, fmt.Sprint("bad-meta-", i), document)
		if w.Code != 400 {
			t.Fatalf("invalid case %d admitted: %d", i, w.Code)
		}
	}
	for _, table := range []string{"review_snapshots", "review_pull_versions", "events", "receipts"} {
		var count int
		f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&count)
		if count != 0 {
			t.Fatal("invalid input wrote", table, count)
		}
	}
	f.observe(t, reviewRequest(0, []PullInput{p}), "metadata-own")
	if _, err := f.store.DB().Exec(`INSERT INTO projects(id,name,github_repository_id,repository) VALUES('metadata-other','Other',702,'owner/other')`); err != nil {
		t.Fatal(err)
	}
	w := f.call(http.MethodGet, "/api/v1/projects/metadata-other/reviews/pulls/1", f.readToken, "", nil)
	if w.Code != 403 {
		t.Fatal("cross-project read admitted", w.Code)
	}
}
func TestPullMetadataSchema6UpgradePreservesIdentityAndOldFacts(t *testing.T) {
	f := newAPIFixtureWithSchema(t, func(ctx context.Context, s *core.Store) error {
		m := append(reviewSchemaMigrations(), historyMigrationSQL(), CurrentSummarySchemaSQL(), CurrentRunOrderSchemaSQL())
		return s.EnsureSchemaWithBackfill(ctx, "reviews", m, backfillCurrentReviewIndexes)
	})
	var instance, signing string
	f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='instance_id'`).Scan(&instance)
	f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='signing_key'`).Scan(&signing)
	// Genuine predecessor physical row, not a version-number relabel of a new schema.
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO review_snapshots VALUES(?, 'metadata-v6',1,701,'old-import','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',?)`, f.project, strings.Repeat("a", 64))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_pull_versions VALUES(?,1,1,'Before metadata','merged',?,'main',?,'success','unknown',1,0,0,'[]','[]')`, f.project, strings.Repeat("a", 40), strings.Repeat("b", 40))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_pull_memberships VALUES(?,1,1,NULL,1,0)`, f.project)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_current_pulls VALUES(?,1,1,0)`, f.project)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_workspace VALUES(?,1,'metadata-v6')`, f.project)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO review_current_totals VALUES(?,1,0,0,0)`, f.project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := metadataLegacyRows(t, f)
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	got := metadataRead(t, f, 1, "metadata-v6")
	if got.Pull.Title != "Before metadata" || got.Pull.Diff != nil || got.Pull.IsDraft != nil || got.Pull.MergedAt != "" {
		t.Fatal("old facts fabricated or changed")
	}
	var nextInstance, nextSigning, version string
	f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='instance_id'`).Scan(&nextInstance)
	f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='signing_key'`).Scan(&nextSigning)
	f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:reviews'`).Scan(&version)
	if version != "7" || instance != nextInstance || signing != nextSigning {
		t.Fatal("migration changed identity/version")
	}
	if after := metadataLegacyRows(t, f); !reflect.DeepEqual(before, after) {
		t.Fatal("schema7 migration changed old logical facts or identity/grants/receipts/events")
	}
	var metadata string
	f.store.DB().QueryRow(`SELECT metadata_json FROM review_pull_versions`).Scan(&metadata)
	if metadata != "{}" {
		t.Fatal("old metadata not unknown")
	}
	after := historyMigrationDatabaseDigest(t, f)
	if err := EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if after != historyMigrationDatabaseDigest(t, f) {
		t.Fatal("second startup changed data")
	}
}

func TestPullMetadataQueueEditAndAdditivePreserve(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	draft := false
	p.Diff = &PullDiff{2, 3, 4}
	p.IsDraft = &draft
	p.MergedAt = "2026-01-01T00:00:00Z"
	f.observe(t, reviewRequest(0, []PullInput{p}), "metadata-additive")
	omitted := p
	omitted.Diff = nil
	omitted.IsDraft = nil
	omitted.MergedAt = ""
	transferApplyFor(t, f, transferRequest(1, TransferGroup{Source: "added-producer", Pulls: []PullInput{omitted}}), "metadata-transfer")
	q := QueuePullInput{Number: p.Number, Title: p.Title, State: p.State, HeadSHA: p.HeadSHA, BaseSHA: p.BaseSHA, BaseRef: p.BaseRef, CIState: p.CIState, Mergeability: p.Mergeability}
	w := queueEditHTTP(f, queueEditRequest(2, []QueuePullInput{q}, nil), "metadata-queue-edit")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	kept := metadataRead(t, f, 1, "")
	if !reflect.DeepEqual(kept.Pull.Diff, p.Diff) || kept.Pull.IsDraft == nil || kept.Pull.MergedAt != p.MergedAt {
		t.Fatal("additive/edit discarded metadata")
	}
	q.HeadSHA = strings.Repeat("d", 40)
	q.State = "open"
	w = queueEditHTTP(f, queueEditRequest(3, []QueuePullInput{q}, nil), "metadata-queue-move")
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	moved := metadataRead(t, f, 1, "")
	if moved.Pull.Diff != nil || moved.Pull.IsDraft != nil || moved.Pull.MergedAt != "" {
		t.Fatal("metadata followed old head")
	}
}

// Compare every pre-existing logical table, omitting only the authorized schema
// version and new metadata column; no historical body or operational row may change.
func metadataLegacyRows(t *testing.T, f *apiFixture) map[string]string {
	t.Helper()
	names, err := f.store.DB().Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var name string
		if err = names.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err = names.Err(); err != nil {
		t.Fatal(err)
	}
	names.Close()
	result := map[string]string{}
	for _, table := range tables {
		rows, err := f.store.DB().Query(`SELECT * FROM "` + table + `" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var retained [][]any
		for rows.Next() {
			values := make([]any, len(columns))
			targets := make([]any, len(columns))
			for i := range values {
				targets[i] = &values[i]
			}
			if err = rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			if table == "metadata" && values[0] == "schema:reviews" {
				continue
			}
			var old []any
			for i, column := range columns {
				if column != "metadata_json" {
					old = append(old, values[i])
				}
			}
			retained = append(retained, old)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		result[table] = mustJSON(retained)
	}
	return result
}
