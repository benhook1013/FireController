package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/inbox"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprocess"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/server"
)

type restoreFixture struct {
	dir                               string
	store                             *core.Store
	token, project, worker, principal string
	handler                           http.Handler
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "source")
	s, err := core.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, init := range []func(context.Context, *core.Store) error{jobs.EnsureSchema, inbox.EnsureSchema, reviews.EnsureSchema, reviewexec.EnsureSchema} {
		if err = init(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	f := &restoreFixture{dir: dir, store: s}
	b, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	f.token = strings.TrimSpace(string(b))
	f.handler = server.New(s, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		return server.RepositoryIdentity{ID: 991, FullName: "synthetic/restore"}, nil
	}))
	mux := f.handler.(*http.ServeMux)
	jobs.Register(mux, s)
	inbox.Register(mux, s)
	reviews.Register(mux, s)
	p := f.request(t, "POST", "/api/v1/projects", map[string]any{"name": "Synthetic restore", "repository": "synthetic/restore"})
	f.project = p["id"].(string)
	w := f.request(t, "POST", "/api/v1/projects/"+f.project+"/workers", map[string]any{"name": "Fixture worker"})
	f.worker = w["id"].(string)
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/credentials", map[string]any{"token_sha256": strings.Repeat("1", 64), "permissions": []string{"read", "write"}})
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/jobs", map[string]any{"title": "Retained job", "brief": "Original brief", "worker_id": f.worker, "status": "active"})
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/messages", map[string]any{"recipient_worker_id": f.worker, "body": "Retained message with literal <text>"})
	var principal string
	if err = s.DB().QueryRow("SELECT principal_id FROM credentials WHERE is_admin=1").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	f.principal = principal
	// Retain a genuine preparing operation, reservation and fixed provider
	// intent. Restore must not settle, reconcile, dispatch or rewrite them.
	e, err := reviewexec.New(s, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	authreq := httptest.NewRequest("POST", "/", nil)
	authreq.SetPathValue("project_id", f.project)
	authreq.Header.Set("Authorization", "Bearer "+f.token)
	authreq.Header.Set("X-FC-Instance", s.InstanceID())
	authreq.Header.Set("X-FC-Worker", f.worker)
	scope, err := s.AdminScope(authreq)
	if err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	importedRun := reviews.RunInput{SourceRunID: "retained-import-run", Channel: "manual", Scope: "focused", Outcome: "completed", Complete: true, Finalized: true, Found: 1, Accepted: 1, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", Findings: []reviews.Finding{{SourceFindingID: "retained-import-finding", Title: "Original imported finding", Detail: "Original retained detail", Severity: "low", Disposition: "accepted", FixSHA: strings.Repeat("d", 40), ProofNote: "Retained fix proof"}}}
	imported := reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 991, Source: "synthetic-import", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{{Number: 1, Title: "Retained merged history", State: "merged", BaseRef: "main", CIState: "success", Mergeability: "mergeable", Complete: true, Runs: []reviews.RunInput{importedRun}}, {Number: 2, Title: "Current source", State: "open", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "pending", Mergeability: "unknown", Complete: true}}}
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/reviews/observations", imported)
	nextRevision := int64(1)
	imported.ExpectedRevision = &nextRevision
	imported.Queue[1].Title = "New metadata retains old snapshot"
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/reviews/observations", imported)

	if _, err = e.Configure(ctx, scope, "restore-config", reviewexec.Configuration{ExpectedRevision: &zero, CapabilityID: "restore-cap", Mode: "independent", Queue: []int64{2}, Channels: []string{"hosted"}}); err != nil {
		t.Fatal("configure", err)
	}
	if _, err = e.ConfigurePolicy(ctx, scope, "restore-policy", reviewexec.PolicyConfiguration{ExpectedRevision: &zero, RequiredChannels: []string{"hosted"}, MaxReviewFiles: 0}); err != nil {
		t.Fatal("policy", err)
	}
	raw, err := e.Enqueue(ctx, scope, "restore-original-request", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: strings.Repeat("a", 40)})
	if err != nil {
		t.Fatal("enqueue", err)
	}
	var receipt reviewexec.Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	attempt, err := e.StartAttempt(ctx, f.project, receipt.ID)
	if err != nil {
		t.Fatal("startattempt", err)
	}
	anchor := reviewexec.Anchor{RepositoryID: 991, Repository: "synthetic/restore", PullNumber: 2, BaseRef: "main", CandidateSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), ParentSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("b", 40), PatchSHA256: strings.Repeat("c", 64), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	query, created, err := e.BeginHostedAllowanceQuery(ctx, f.project, attempt.ID, 1, anchor, reviewprovider.Author{ID: 1, Login: "fixture-user", Kind: "User"}, reviewprovider.Author{ID: 2, Login: "fixture-bot", Kind: "Bot"})
	if err != nil || !created || query.Intent == "" {
		t.Fatal("retained query intent", err)
	}
	// Unreferenced immutable final files are still forensic records, not credit.
	cli, err := reviewruntime.NewCaptureStore(filepath.Join(dir, "review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC()
	if _, err = cli.Save(ctx, core.NewID("review_attempt"), reviewruntime.CaptureRecord{Capture: reviewprocess.Capture{Stdout: []byte("retained CLI forensic output"), Started: true}, StartedAt: stamp, FinishedAt: stamp}); err != nil {
		t.Fatal(err)
	}
	hosted, err := reviewruntime.NewHostedCaptureStore(filepath.Join(dir, "hosted-review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	id := core.NewID("review_attempt")
	intent := reviewprovider.HostedTriggerIntent{OperationID: core.NewID("review_operation"), AttemptID: id, HeadSHA: strings.Repeat("a", 40)}
	if _, err = hosted.Save(ctx, id, reviewruntime.HostedCaptureRecord{RepositoryID: 991, Repository: "synthetic/restore", PullNumber: 2, Intent: intent, PostingActor: reviewprovider.Author{ID: 1, Login: "fixture-user", Kind: "User"}, ProviderActor: reviewprovider.Author{ID: 2, Login: "fixture-bot", Kind: "Bot"}, EarliestPosting: stamp, StartedAt: stamp, FinishedAt: stamp, Snapshot: reviewprovider.HostedPublicationSnapshot{IssueComments: []reviewprovider.IssueComment{}, Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}, Complete: true, ObservedAt: stamp}}); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *restoreFixture) request(t *testing.T, method, path string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	if f.worker != "" {
		r.Header.Set("X-FC-Worker", f.worker)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", core.NewID("fixture"))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code < 200 || w.Code > 299 {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func restoreRows(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	r, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cols, err := r.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for r.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err = r.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err = r.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
func restoreBundle(t *testing.T, f *restoreFixture) Summary {
	t.Helper()
	s, err := Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func restoreTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			out[strings.TrimPrefix(path, dir+string(os.PathSeparator))] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func restoreCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("wanted %s got %v", code, err)
	}
}

func TestRestoreInactiveRotatesIdentityRevokesAndPreservesRecords(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal("created bundle inspect", err)
	}
	immutable := restoreTree(t, bundle.Path)
	var key string
	if err := f.store.DB().QueryRow("SELECT value FROM metadata WHERE key='signing_key'").Scan(&key); err != nil {
		t.Fatal(err)
	}
	tables := restoreRows(t, f.store.DB(), "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT IN ('metadata','credentials') ORDER BY name")
	original := map[string][][]any{}
	for _, row := range tables {
		name := row[0].(string)
		original[name] = restoreRows(t, f.store.DB(), "SELECT * FROM "+name+" ORDER BY rowid")
	}
	destination := filepath.Join(t.TempDir(), "restored")
	summary, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil {
		t.Fatal(err)
	}
	if summary.State != "inactive" || summary.SourceInstanceID != f.store.InstanceID() || summary.InstanceID == summary.SourceInstanceID || !instanceID.MatchString(summary.InstanceID) || summary.ArtifactCount != 2 || summary.Destination != destination {
		t.Fatalf("bad summary: %+v", summary)
	}
	encoded, err := json.Marshal(summary)
	if err != nil || len(encoded) > 4096 || bytes.Contains(encoded, []byte(key)) {
		t.Fatal("unsafe summary")
	}
	for path, body := range immutable {
		if path == "firecontroller.sqlite" || strings.HasPrefix(path, "review-captures/") || strings.HasPrefix(path, "hosted-review-captures/") {
			info, err := os.Lstat(filepath.Join(destination, path))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("restored file privacy", path, err)
			}
			_ = body
		}
	}
	if err = CheckDataDirectory(destination); err != nil {
		t.Fatal(err)
	}
	s, err := core.Open(t.Context(), destination)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.InstanceID() != summary.InstanceID {
		t.Fatal("identity not durable")
	}
	restoreCode(t, StartupGuard(t.Context(), s), "restore_inactive")
	var newKey string
	if err = s.DB().QueryRow("SELECT value FROM metadata WHERE key='signing_key'").Scan(&newKey); err != nil {
		t.Fatal(err)
	}
	if newKey == key || !digest.MatchString(newKey) {
		t.Fatal("signing identity reused or invalid")
	}
	if got := restoreRows(t, s.DB(), "SELECT count(*) FROM credentials WHERE revoked<>1")[0][0]; got != int64(0) {
		t.Fatal("credential revived", got)
	}
	expectedCredentials := restoreRows(t, f.store.DB(), "SELECT id,principal_id,token_sha256,is_admin FROM credentials ORDER BY id")
	if !reflect.DeepEqual(expectedCredentials, restoreRows(t, s.DB(), "SELECT id,principal_id,token_sha256,is_admin FROM credentials ORDER BY id")) {
		t.Fatal("credential history replaced")
	}
	for name, want := range original {
		if !reflect.DeepEqual(want, restoreRows(t, s.DB(), "SELECT * FROM "+name+" ORDER BY rowid")) {
			t.Fatalf("retained table changed: %s", name)
		}
	}
	for _, dir := range []string{"review-captures", "hosted-review-captures"} {
		if !reflect.DeepEqual(restoreTree(t, filepath.Join(f.dir, dir)), restoreTree(t, filepath.Join(destination, dir))) {
			t.Fatal("forensic artifacts changed")
		}
	}
	if _, err = os.Lstat(filepath.Join(destination, "bootstrap-admin.token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("bootstrap credential created")
	}
	if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("source bundle changed")
	}
	if _, err = s.DB().Exec("DELETE FROM metadata WHERE key='restore:state'"); err != nil {
		t.Fatal(err)
	}
	restoreCode(t, StartupGuard(t.Context(), s), "restore_inactive")
}

func TestRestoreRefusesCollisionSymlinksCancellationAndUnsafeBundle(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	for _, kind := range []string{"existing", "symlink", "canceled", "hash"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			destination := filepath.Join(parent, "restored")
			ctx := t.Context()
			selected := bundle.Path
			switch kind {
			case "existing":
				if err := os.Mkdir(destination, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(f.dir, destination); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "hash":
				copydir := filepath.Join(parent, bundle.ID)
				if err := os.Mkdir(copydir, 0700); err != nil {
					t.Fatal(err)
				}
				for path, body := range restoreTree(t, bundle.Path) {
					target := filepath.Join(copydir, path)
					if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(target, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				selected = copydir
				if err := os.WriteFile(filepath.Join(copydir, "firecontroller.sqlite"), []byte("bad database"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Restore(ctx, selected, destination); err == nil {
				t.Fatal("unsafe restore succeeded")
			}
			if kind == "existing" {
				entries, err := os.ReadDir(destination)
				if err != nil || len(entries) != 0 {
					t.Fatal("existing destination touched")
				}
			} else if kind == "symlink" {
				info, err := os.Lstat(destination)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("existing symlink touched")
				}
			} else {
				if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("usable partial destination remains")
				}
			}
		})
	}
}

func TestRestoreRefusesUnexpectedCredentialTriggersBeforeCopy(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	statement := `CREATE TRIGGER rewrite_history AFTER UPDATE ON credentials BEGIN UPDATE jobs SET brief='unexpected rewritten history'; END`
	selected := restoreModifiedBundle(t, bundle, statement)
	before := restoreTree(t, selected)
	destination := filepath.Join(t.TempDir(), "refused")
	_, err := Restore(t.Context(), selected, destination)
	restoreCode(t, err, "backup_invalid")
	if _, err = os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported trigger left destination")
	}
	if !reflect.DeepEqual(before, restoreTree(t, selected)) {
		t.Fatal("restore changed rejected bundle")
	}
	if _, err = f.store.DB().Exec(statement); err != nil {
		t.Fatal(err)
	}
	_, err = Create(t.Context(), f.store, f.dir, filepath.Join(t.TempDir(), "unsupported-source"))
	restoreCode(t, err, "backup_invalid")
}

func TestRestoreStartupGuardsAndOrdinaryRestart(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "ordinary")
	if err := CheckDataDirectory(dir); err != nil {
		t.Fatal(err)
	}
	s, err := core.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	id := s.InstanceID()
	if err = StartupGuard(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = core.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.InstanceID() != id {
		t.Fatal("ordinary restart rotated identity")
	}
	if err = StartupGuard(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`INSERT INTO metadata(key,value) VALUES('restore:state','unexpected')`); err != nil {
		t.Fatal(err)
	}
	restoreCode(t, StartupGuard(ctx, s), "restore_inactive")
	for _, kind := range []string{"file", "dangling-symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			d := t.TempDir()
			marker := filepath.Join(d, restoreMarker)
			var err error
			switch kind {
			case "file":
				err = os.WriteFile(marker, nil, 0600)
			case "dangling-symlink":
				err = os.Symlink(filepath.Join(d, "absent"), marker)
			case "directory":
				err = os.Mkdir(marker, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			restoreCode(t, CheckDataDirectory(d), "restore_inactive")
			if _, err = os.Stat(filepath.Join(d, "firecontroller.sqlite")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("guard created DB")
			}
		})
	}
}

func TestRestoreRejectsRehashedNewerSchemaAndInsideBundleWithoutMutation(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	before := restoreTree(t, bundle.Path)
	if _, err := Restore(t.Context(), bundle.Path, filepath.Join(bundle.Path, "nested-restore")); err == nil {
		t.Fatal("restore wrote into source bundle")
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("bundle changed after forbidden destination")
	}
	selected := restoreModifiedBundle(t, bundle, "PRAGMA user_version=3")
	parent := t.TempDir()
	destination := filepath.Join(parent, "newer-refused")
	if _, err := Restore(t.Context(), selected, destination); err == nil {
		t.Fatal("unsupported newer schema accepted despite valid hashes")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("newer-schema failure left destination")
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("original bundle changed")
	}
}

// Cancel at the externally visible incomplete marker, without a production
// hook, timing sleep, or an oversized fixture. The underlying cancellation
// context owns Done/Err exactly as it does for an operator interrupt.
type restoreMarkerCancellation struct {
	context.Context
	cancel context.CancelFunc
	marker string
}

func (c restoreMarkerCancellation) Err() error {
	if _, err := os.Lstat(c.marker); err == nil {
		c.cancel()
	}
	return c.Context.Err()
}
func TestRestoreCancellationAfterMarkerCleansOwnedCopy(t *testing.T) {
	f := newRestoreFixture(t)
	bundle := restoreBundle(t, f)
	before := restoreTree(t, bundle.Path)
	parent := t.TempDir()
	sibling := filepath.Join(parent, "unrelated")
	if err := os.WriteFile(sibling, []byte("preserve this file"), 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(parent, "restoring")
	base, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := restoreMarkerCancellation{Context: base, cancel: cancel, marker: filepath.Join(destination, restoreMarker)}
	if _, err := Restore(ctx, bundle.Path, destination); err == nil {
		t.Fatal("post-marker cancellation succeeded")
	}
	if !errors.Is(base.Err(), context.Canceled) {
		t.Fatal("fixture did not reach owned incomplete marker")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned canceled copy remains")
	}
	body, err := os.ReadFile(sibling)
	if err != nil || string(body) != "preserve this file" {
		t.Fatal("unrelated path changed")
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("canceled restore changed bundle")
	}
}

func restoreModifiedBundle(t *testing.T, bundle Summary, statement string) string {
	t.Helper()
	selected := filepath.Join(t.TempDir(), bundle.ID)
	if err := os.Mkdir(selected, 0700); err != nil {
		t.Fatal(err)
	}
	for path, body := range restoreTree(t, bundle.Path) {
		target := filepath.Join(selected, path)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(selected, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(statement); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	var m Manifest
	raw, err := os.ReadFile(filepath.Join(selected, "manifest.json"))
	if err != nil || json.Unmarshal(raw, &m) != nil {
		t.Fatal("manifest", err)
	}
	m.Database, err = hashPrivate(t.Context(), filepath.Join(selected, "firecontroller.sqlite"), maxDatabaseBytes)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = canonicalManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if err = os.WriteFile(filepath.Join(selected, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(selected, "manifest.sha256"), []byte(hex.EncodeToString(sum[:])+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return selected
}
