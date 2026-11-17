package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/historycollect"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// A controlled read-only provider returns genuine typed facts for the engine.
// Its second review page rate-limits, leaving two committed pages and the exact
// unfinished reviews/page-two checkpoint. No capture SQL is manufactured.
type historyCollectionBackupProvider struct{ calls atomic.Int64 }

func (p *historyCollectionBackupProvider) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	p.calls.Add(1)
	return reviewprovider.Repository{ID: 991, FullName: "synthetic/restore", DefaultBranch: "main"}, nil
}
func (p *historyCollectionBackupProvider) Pull(context.Context, string, int64) (reviewprovider.Pull, error) {
	p.calls.Add(1)
	return reviewprovider.Pull{Number: 47, RepositoryID: 991, Title: "Retained selected merged history", State: "closed", Merged: true, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 2}, nil
}
func (p *historyCollectionBackupProvider) ReviewsPage(_ context.Context, _ string, _ int64, page int64) (reviewprovider.Page[reviewprovider.HostedReview], error) {
	p.calls.Add(1)
	if page == 2 {
		return reviewprovider.Page[reviewprovider.HostedReview]{}, &reviewprovider.Error{Code: "provider_rate_limited", Message: "Controlled provider limit", Status: 429}
	}
	return reviewprovider.Page[reviewprovider.HostedReview]{Items: []reviewprovider.HostedReview{{ID: 201, Body: "Untrusted original <script>literal</script> source", State: "commented", HeadSHA: strings.Repeat("a", 40), Author: reviewprovider.Author{ID: 31, Login: "synthetic-reviewer", Kind: "Bot"}, SubmittedAt: time.Date(2026, 10, 1, 1, 2, 3, 123456789, time.UTC)}}, NextPage: 2}, nil
}
func (p *historyCollectionBackupProvider) ReviewCommentsPage(context.Context, string, int64, int64) (reviewprovider.Page[reviewprovider.ReviewComment], error) {
	p.calls.Add(1)
	return reviewprovider.Page[reviewprovider.ReviewComment]{Items: []reviewprovider.ReviewComment{}}, nil
}
func (p *historyCollectionBackupProvider) IssueCommentsPage(context.Context, string, int64, int64) (reviewprovider.Page[reviewprovider.IssueComment], error) {
	p.calls.Add(1)
	return reviewprovider.Page[reviewprovider.IssueComment]{Items: []reviewprovider.IssueComment{}}, nil
}

func historyCollectionRestoreFixture(t *testing.T) (*restoreFixture, *historyCollectionBackupProvider, string, string) {
	t.Helper()
	f := newRestoreFixture(t)
	if err := historycollect.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	provider := &historyCollectionBackupProvider{}
	e, err := historycollect.New(f.store, provider, strings.Repeat("9", 64))
	if err != nil {
		t.Fatal(err)
	}
	scope := routePolicyInventoryScope(t, f)
	raw, err := e.Enqueue(t.Context(), scope, "collection-backup-captured", historycollect.Selection{PullNumbers: []int64{47}})
	if err != nil {
		t.Fatal(err)
	}
	var captured historycollect.Receipt
	if err = json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	joined, err := e.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Even a failed assertion must join this owned worker before Store cleanup.
	stopped := false
	defer func() {
		if !stopped {
			cancel()
			<-joined
		}
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		op, err := e.Read(t.Context(), scope, captured.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.State == "paused" {
			if op.Reason != "rate_limited" || op.PageCount != 2 || op.RecordCount != 2 || op.Position != (historycollect.Position{PullIndex: 0, Resource: "reviews", Page: 2}) || op.PassNumber != 2 {
				t.Fatalf("engine partial state reason=%s pages=%d records=%d pass=%d resource=%s page=%d", op.Reason, op.PageCount, op.RecordCount, op.PassNumber, op.Position.Resource, op.Position.Page)
			}
			break
		}
		select {
		case err := <-joined:
			t.Fatalf("collection worker ended before expected pause: %v", err)
		case <-deadline.C:
			t.Fatal("collection worker did not pause within bounded fixture deadline")
		case <-tick.C:
		}
	}
	cancel()
	if err := <-joined; err != nil {
		t.Fatal(err)
	}
	stopped = true
	raw, err = e.Enqueue(t.Context(), scope, "collection-backup-pending", historycollect.Selection{PullNumbers: []int64{12, 47}})
	if err != nil {
		t.Fatal(err)
	}
	var pending historycollect.Receipt
	if err = json.Unmarshal(raw, &pending); err != nil {
		t.Fatal(err)
	}
	if op, err := e.Read(t.Context(), scope, pending.ID); err != nil || op.State != "queued" || op.PageCount != 0 {
		t.Fatal("pending collection fixture unavailable")
	}
	pages, err := e.Pages(t.Context(), scope, captured.ID, 20, "")
	if err != nil || len(pages.Items) != 2 || pages.HasMore {
		t.Fatal("canonical captured page list unavailable", err)
	}
	for _, page := range pages.Items {
		if _, err = e.Capture(t.Context(), scope, captured.ID, page.ID); err != nil {
			t.Fatalf("canonical %s capture read refused: %v", page.Resource, err)
		}
	}
	if _, err := validateSchema(t.Context(), f.store.DB(), false); err != nil {
		t.Fatal("canonical collection physical schema refused", err)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = historycollect.ValidateSnapshot(t.Context(), tx)
	tx.Rollback()
	if err != nil {
		t.Fatal("canonical engine records failed owning logical validation", err)
	}
	return f, provider, captured.ID, pending.ID
}

func historyCollectionQueries() map[string]string {
	return map[string]string{
		"operations":        `SELECT * FROM history_collections ORDER BY id`,
		"immutable-pages":   `SELECT * FROM history_collection_pages ORDER BY collection_id,sequence`,
		"source-identities": `SELECT * FROM history_collection_seen ORDER BY collection_id,pass_number,record_id`,
		"receipts":          `SELECT * FROM receipts ORDER BY instance_id,principal_id,project_id,operation,target,key`,
		"events":            `SELECT * FROM events ORDER BY sequence`,
		"jobs":              `SELECT * FROM jobs ORDER BY project_id,id`,
		"messages":          `SELECT * FROM inbox_messages ORDER BY sequence`,
		"native-journal":    `SELECT * FROM review_operations ORDER BY id`,
		"native-evidence":   `SELECT * FROM review_execution_evidence ORDER BY id`,
	}
}
func TestHistoryCollectionBundlePreservesCapturedPagesAndPendingCheckpoint(t *testing.T) {
	f, provider, _, _ := historyCollectionRestoreFixture(t)
	queries := historyCollectionQueries()
	before := map[string][][]any{}
	for name, q := range queries {
		before[name] = restoreRows(t, f.store.DB(), q)
	}
	calls := provider.calls.Load()
	bundle := restoreBundle(t, f)
	tree := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tree, restoreTree(t, bundle.Path)) {
		t.Fatal("inspection modified immutable collection bundle")
	}
	target := filepath.Join(t.TempDir(), "restored-collection")
	restored, err := Restore(t.Context(), bundle.Path, target)
	if err != nil {
		t.Fatal(err)
	}
	if restored.State != "inactive" || restored.InstanceID == f.store.InstanceID() {
		t.Fatal("collection restore activated or reused old identity")
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, q := range queries {
		if !reflect.DeepEqual(before[name], restoreRows(t, db, q)) {
			t.Fatalf("restore changed retained %s", name)
		}
		if !reflect.DeepEqual(before[name], restoreRows(t, f.store.DB(), q)) {
			t.Fatalf("backup changed live %s", name)
		}
	}
	var state, version string
	var activeOriginal int
	if err = db.QueryRow(`SELECT value FROM metadata WHERE key='restore:state'`).Scan(&state); err != nil || state != "inactive" {
		t.Fatal("restore was not held inactive")
	}
	if err = db.QueryRow(`SELECT value FROM metadata WHERE key='schema:history_collection'`).Scan(&version); err != nil || version != "1" {
		t.Fatal("collection schema version not retained")
	}
	if err = db.QueryRow(`SELECT count(*) FROM credentials WHERE is_admin=1 AND revoked=0 AND token_sha256 IN (SELECT token_sha256 FROM credentials WHERE principal_id=?)`, f.principal).Scan(&activeOriginal); err != nil || activeOriginal != 0 {
		t.Fatal("restore resurrected original collection credential")
	}
	if provider.calls.Load() != calls {
		t.Fatal("backup/inspection/restore invoked collection provider")
	}
}

func TestHistoryCollectionBundlePredecessorRemainsOptional(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	before := restoreTree(t, bundle.Path)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored-predecessor")
	if _, err := Restore(t.Context(), bundle.Path, target); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(target, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name LIKE 'history_collection%'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("predecessor restore installed collection schema")
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("predecessor inspection changed source bundle")
	}
}

func TestHistoryCollectionBundleRejectsResealedCorruptionBeforeDestinationWrites(t *testing.T) {
	f, _, captured, _ := historyCollectionRestoreFixture(t)
	cases := map[string]string{
		"cross-project-page": `UPDATE history_collection_pages SET project_id='another-project' WHERE sequence=1`,
		"missing-page":       `DELETE FROM history_collection_seen WHERE page_id IN (SELECT id FROM history_collection_pages WHERE sequence=1); DELETE FROM history_collection_pages WHERE sequence=1`,
		"seen-mismatch":      `UPDATE history_collection_seen SET record_id='999' WHERE record_id='201'`,
		"checkpoint":         `UPDATE history_collections SET position_json=json_set(position_json,'$.page',3) WHERE id=?`,
		"page-count":         `UPDATE history_collections SET page_count=page_count+1 WHERE id=?`,
		"record-count":       `UPDATE history_collections SET record_count=record_count+1 WHERE id=?`,
		"body-hash":          `UPDATE history_collection_pages SET body_sha256='` + strings.Repeat("0", 64) + `' WHERE sequence=2`,
		"pending-index":      `DROP INDEX history_collection_pending`,
		"nullable-body":      `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,'body_json TEXT NOT NULL','body_json TEXT') WHERE name='history_collection_pages'; PRAGMA writable_schema=OFF`,
		"seen-key":           `PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql=replace(sql,'PRIMARY KEY(collection_id,pass_number,record_id)','PRIMARY KEY(collection_id,record_id,pass_number)') WHERE name='history_collection_seen'; PRAGMA writable_schema=OFF`,
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			bundle := restoreBundle(t, f)
			if strings.Contains(query, "?") {
				mutateBundleSQL(t, bundle.Path, query, captured)
			} else {
				mutateBundleSQL(t, bundle.Path, query)
			}
			historyCollectionRefuseBundle(t, bundle.Path)
		})
	}
	t.Run("rehash-wrong-body-scope", func(t *testing.T) {
		bundle := restoreBundle(t, f)
		db, err := sql.Open("sqlite", filepath.Join(bundle.Path, "firecontroller.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		var id, body string
		if err = db.QueryRow(`SELECT id,body_json FROM history_collection_pages WHERE sequence=2`).Scan(&id, &body); err != nil {
			db.Close()
			t.Fatal(err)
		}
		var capture historycollect.Capture
		if err = json.Unmarshal([]byte(body), &capture); err != nil {
			db.Close()
			t.Fatal(err)
		}
		capture.RepositoryID = 992
		b, err := json.Marshal(capture)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if _, err = db.Exec(`UPDATE history_collection_pages SET body_json=?,body_sha256=? WHERE id=?`, string(b), hex.EncodeToString(sum[:]), id); err != nil {
			db.Close()
			t.Fatal(err)
		}
		if err = db.Close(); err != nil {
			t.Fatal(err)
		}
		resealBundle(t, bundle.Path, nil)
		historyCollectionRefuseBundle(t, bundle.Path)
	})
}
func historyCollectionRefuseBundle(t *testing.T, path string) {
	t.Helper()
	_, err := Inspect(t.Context(), path)
	backupCode(t, err, "backup_invalid")
	target := filepath.Join(t.TempDir(), "must-not-exist")
	_, err = Restore(t.Context(), path, target)
	backupCode(t, err, "backup_invalid")
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("malformed collection wrote restore destination")
	}
}

func TestHistoryCollectionBundleCancellationIsUnavailable(t *testing.T) {
	f, _, _, _ := historyCollectionRestoreFixture(t)
	bundle := restoreBundle(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Inspect(ctx, bundle.Path)
	backupCode(t, err, "backup_unavailable")
}
