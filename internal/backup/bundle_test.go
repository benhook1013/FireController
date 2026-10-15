package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
)

func backupCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *core.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("wanted safe %s, got %v", code, err)
	}
}
func backupRows(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			t.Fatal("table names unavailable")
		}
		names = append(names, name)
	}
	rows.Close()
	out := map[string]string{}
	for _, name := range names {
		items := restoreRows(t, db, `SELECT * FROM "`+name+`"`)
		var encoded []string
		for _, row := range items {
			b, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			encoded = append(encoded, string(b))
		}
		sort.Strings(encoded)
		h := sha256.Sum256([]byte(strings.Join(encoded, "\n")))
		out[name] = hex.EncodeToString(h[:])
	}
	return out
}
func bundleNativeReferences(t *testing.T, f *restoreFixture) (string, string) {
	t.Helper()
	ctx := t.Context()
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}, {ID: "bundle-cli", Provider: "coderabbit", Account: "cli-account", Channels: []string{"cli"}}})
	if err != nil {
		t.Fatal(err)
	}
	var queryAttempt string
	if err = f.store.DB().QueryRow(`SELECT attempt_id FROM review_hosted_allowance_queries LIMIT 1`).Scan(&queryAttempt); err != nil {
		t.Fatal(err)
	}
	query, err := e.ReadHostedAllowanceQuery(ctx, f.project, queryAttempt)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := reviewruntime.NewHostedCaptureStore(filepath.Join(f.dir, "hosted-review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	intent := reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: queryAttempt, HeadSHA: query.Anchor.CandidateSHA}
	ref, err := hs.Save(ctx, queryAttempt, reviewruntime.HostedCaptureRecord{RepositoryID: 991, Repository: "synthetic/restore", PullNumber: 2, Intent: intent, PostingActor: query.PostingActor, ProviderActor: query.ProviderActor, EarliestPosting: at, StartedAt: at, FinishedAt: at, Snapshot: reviewprovider.HostedPublicationSnapshot{IssueComments: []reviewprovider.IssueComment{}, Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}, ObservedAt: at, Complete: true}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.RecordHostedObservation(ctx, f.project, queryAttempt, reviewexec.HostedObservationInput{Kind: "allowance_query", CaptureID: ref.ID, CaptureSHA256: ref.SHA256, CaptureBytes: ref.Bytes, StartedAt: at.Format(time.RFC3339Nano), FinishedAt: at.Format(time.RFC3339Nano), ObservedAt: at.Format(time.RFC3339Nano), ExpectedAnchor: query.Anchor, Intent: query.Intent})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.SetPathValue("project_id", f.project)
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("X-FC-Worker", f.worker)
	scope, err := f.store.AdminScope(r)
	if err != nil {
		t.Fatal(err)
	}
	previous := int64(1)
	_, err = e.Configure(ctx, scope, "bundle-config", reviewexec.Configuration{ExpectedRevision: &previous, CapabilityID: "bundle-cli", Mode: "independent", Queue: []int64{4}, Channels: []string{"cli"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := e.Enqueue(ctx, scope, "bundle-cli-review", reviewexec.Request{ExpectedConfigurationRevision: 2, PullNumber: 4, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal(err)
	}
	var receipt reviewexec.Receipt
	json.Unmarshal(raw, &receipt)
	attempt, err := e.StartAttempt(ctx, f.project, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	anchor := query.Anchor
	anchor.PullNumber = 4
	attempt, err = e.Admit(ctx, f.project, attempt.ID, attempt.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	cli, err := reviewruntime.NewCaptureStore(filepath.Join(f.dir, "review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	at = time.Now().UTC()
	captureRef, err := cli.Save(ctx, attempt.ID, reviewruntime.CaptureRecord{Capture: reviewprocess.Capture{Stdout: []byte("synthetic captured raw bytes"), Started: true}, StartedAt: at, FinishedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.RecordReviewEvidence(ctx, f.project, attempt.ID, reviewexec.ReviewEvidenceInput{CaptureID: captureRef.ID, CaptureSHA256: captureRef.SHA256, CaptureBytes: captureRef.Bytes, StartedAt: at.Format(time.RFC3339Nano), FinishedAt: at.Format(time.RFC3339Nano), ExpectedAnchor: anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, Findings: []reviewprovider.ParsedFinding{}, ReviewedFiles: []string{"example.go"}}})
	if err != nil {
		t.Fatal(err)
	}
	return "review-captures/" + attempt.ID + ".json", "hosted-review-captures/" + queryAttempt + "-hosted-" + ref.SHA256 + ".json"
}
func TestBundleCreateInspectFullStateReferencesAndForensicFiles(t *testing.T) {
	f := newRestoreFixture(t)
	cli, hosted := bundleNativeReferences(t, f)
	before := backupRows(t, f.store.DB())
	// Unrelated private files and caches must never leak into the bundle.
	if err := os.WriteFile(filepath.Join(f.dir, "review-captures", ".capture-pending"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "review-captures", "unrelated-secret"), []byte("excluded"), 0600); err != nil {
		t.Fatal(err)
	}
	created, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "complete" || created.ArtifactCount != 4 || created.SourceInstanceID != f.store.InstanceID() {
		t.Fatal(created)
	}
	inspected, err := Inspect(t.Context(), created.Path)
	if err != nil || inspected.State != "verified" || inspected.DatabaseBytes != created.DatabaseBytes || inspected.ArtifactCount != 4 {
		t.Fatal(inspected, err)
	}
	m, err := inspectManifest(t.Context(), created.Path)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, a := range m.Artifacts {
		paths[a.Path] = true
	}
	if !paths[cli] || !paths[hosted] {
		t.Fatal("referenced source missing")
	}
	if _, err = os.Lstat(filepath.Join(created.Path, "bootstrap-admin.token")); !os.IsNotExist(err) {
		t.Fatal("credential copied")
	}
	snapshot, err := openSnapshot(t.Context(), filepath.Join(created.Path, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	after := backupRows(t, snapshot)
	snapshot.Close()
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("backup changed or dropped canonical records")
	}
	tree := restoreTree(t, created.Path)
	for i := 0; i < 2; i++ {
		if _, err = Inspect(t.Context(), created.Path); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(tree, restoreTree(t, created.Path)) {
		t.Fatal("inspection mutated bundle")
	}
	if b, _ := json.Marshal(inspected); len(b)+1 > 4096 || strings.Contains(string(b), "retained CLI forensic output") || strings.Contains(string(b), f.token) {
		t.Fatal("summary private or oversized")
	}
	// The snapshot and files are independent of every source-side directory.
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(f.dir); err != nil {
		t.Fatal(err)
	}
	if _, err = Inspect(t.Context(), created.Path); err != nil {
		t.Fatal("bundle depends on source", err)
	}
}
func TestBundlePreservesRecognizedMalformedForensicsWithoutCredit(t *testing.T) {
	f := newRestoreFixture(t)
	id := core.NewID("review_attempt")
	path := filepath.Join(f.dir, "review-captures", id+".json")
	raw := []byte("old or malformed raw diagnostic, not interpreted")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	created, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(created.Path, "review-captures", id+".json"))
	if err != nil || string(copied) != string(raw) {
		t.Fatal("forensic bytes discarded", err)
	}
	var n int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_execution_evidence`).Scan(&n); err != nil || n != 0 {
		t.Fatal("file gained credit", err)
	}
}
func TestBundleFailureCleansOwnReservationPreservesExistingAndSource(t *testing.T) {
	f := newRestoreFixture(t)
	cli, _ := bundleNativeReferences(t, f)
	root := filepath.Join(t.TempDir(), "bundles")
	created, err := Create(t.Context(), f.store, f.dir, root)
	if err != nil {
		t.Fatal(err)
	}
	existing := restoreTree(t, created.Path)
	before := backupRows(t, f.store.DB())
	if err = os.Remove(filepath.Join(f.dir, filepath.FromSlash(cli))); err != nil {
		t.Fatal(err)
	}
	_, err = Create(t.Context(), f.store, f.dir, root)
	backupCode(t, err, "backup_invalid")
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != created.ID {
		t.Fatal("failed reservation remains", err)
	}
	if !reflect.DeepEqual(existing, restoreTree(t, created.Path)) || !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("failed backup rewrote previous bundle/source")
	}
}
func TestBundleRejectsUnsafeDestinationsMismatchedSourceAndCancellation(t *testing.T) {
	f := newRestoreFixture(t)
	for _, destination := range []string{f.dir, filepath.Join(f.dir, "nested"), filepath.Dir(f.dir)} {
		_, err := Create(t.Context(), f.store, f.dir, destination)
		backupCode(t, err, "backup_invalid")
	}
	sibling := newRestoreFixture(t)
	_, err := Create(t.Context(), f.store, sibling.dir, filepath.Join(t.TempDir(), "bundles"))
	backupCode(t, err, "backup_invalid")
	link := filepath.Join(t.TempDir(), "link")
	target := filepath.Join(t.TempDir(), "private")
	if os.Mkdir(target, 0700) != nil || os.Symlink(target, link) != nil {
		t.Fatal("link fixture")
	}
	_, err = Create(t.Context(), f.store, f.dir, link)
	backupCode(t, err, "backup_invalid")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	destination := filepath.Join(t.TempDir(), "never-created")
	_, err = Create(ctx, f.store, f.dir, destination)
	backupCode(t, err, "backup_unavailable")
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatal("canceled creation touched destination")
	}
}
func TestBundleReadOnlyPreflightNeverBootstrapsOrMigrates(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "selected")
			if os.Mkdir(dir, 0700) != nil {
				t.Fatal("mkdir")
			}
			path := filepath.Join(dir, "firecontroller.sqlite")
			if kind == "empty" {
				if os.WriteFile(path, nil, 0600) != nil {
					t.Fatal("empty")
				}
			}
			if kind == "foreign" {
				if os.WriteFile(path, []byte("foreign bytes"), 0600) != nil {
					t.Fatal("foreign")
				}
			}
			before, _ := os.ReadFile(path)
			backupCode(t, ValidateSource(t.Context(), dir), "backup_invalid")
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("preflight changed bytes")
			}
			if _, err := os.Lstat(filepath.Join(dir, "bootstrap-admin.token")); !os.IsNotExist(err) {
				t.Fatal("preflight bootstrapped")
			}
		})
	}
	f := newRestoreFixture(t)
	before := backupRows(t, f.store.DB())
	if err := ValidateSource(t.Context(), f.dir); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("preflight mutated source")
	}
}
