package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type activationFixture struct {
	source     *restoreFixture
	dir, token string
	request    ActivationRequest
}

func activationTestFixture(t *testing.T) *activationFixture {
	t.Helper()
	source := newRestoreFixture(t)
	bundle := restoreBundle(t, source)
	dir := filepath.Join(t.TempDir(), "inactive")
	restored, err := Restore(t.Context(), bundle.Path, dir)
	if err != nil {
		t.Fatal(err)
	}
	token := "synthetic-fresh-activation-token-1234567890"
	file := filepath.Join(t.TempDir(), "admin.token")
	if err = os.WriteFile(file, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return &activationFixture{source: source, dir: dir, token: token, request: ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: file}}
}
func activationRows(t *testing.T, dir string) map[string][][]any {
	t.Helper()
	db, err := openDatabase(t.Context(), filepath.Join(dir, "firecontroller.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result := map[string][][]any{}
	for _, row := range restoreRows(t, db, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`) {
		name := row[0].(string)
		result[name] = restoreRows(t, db, `SELECT * FROM "`+name+`" ORDER BY rowid`)
	}
	return result
}
func activationExec(t *testing.T, dir, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func activationOpen(t *testing.T, dir string) *core.Store {
	t.Helper()
	s, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestActivationPreservesHistoryRotatesAccessAndReplaysReadOnly(t *testing.T) {
	f := activationTestFixture(t)
	before := activationRows(t, f.dir)
	summary, err := Activate(t.Context(), f.dir, f.request)
	if err != nil || summary.State != "coordination_active" || !summary.ProvidersHeld || summary.InstanceID != f.request.ExpectedInstanceID || summary.SourceInstanceID != f.request.FencedPredecessorID || summary.CredentialID == "" || summary.ActivatedAt == "" {
		t.Fatal("local activation failed", summary, err)
	}
	encoded, err := json.Marshal(summary)
	if err != nil || len(encoded) > 4096 || strings.Contains(string(encoded), f.token) || strings.Contains(string(encoded), f.request.AdminTokenFile) {
		t.Fatal("unsafe activation summary")
	}
	after := activationRows(t, f.dir)
	for name, rows := range before {
		if name != "metadata" && name != "credentials" && !reflect.DeepEqual(rows, after[name]) {
			t.Fatalf("activation changed retained table %s", name)
		}
	}
	if len(after["credentials"]) != len(before["credentials"])+1 {
		t.Fatal("activation did not add exactly one credential")
	}
	if !reflect.DeepEqual(after["credentials"][:len(before["credentials"])], before["credentials"]) {
		t.Fatal("old credential history changed")
	}
	for _, row := range before["credentials"] {
		if row[4] != int64(1) {
			t.Fatal("fixture did not revoke predecessor credentials")
		}
	}
	oldMetadata := map[string]any{}
	for _, row := range before["metadata"] {
		oldMetadata[row[0].(string)] = row[1]
	}
	for _, row := range after["metadata"] {
		if row[0] != "restore:state" {
			if value, ok := oldMetadata[row[0].(string)]; ok && value != row[1] {
				t.Fatal("original restore provenance changed")
			}
		}
	}
	s := activationOpen(t, f.dir)
	status, err := ReadRecoveryStatus(t.Context(), s)
	if err != nil || status.State != summary.State || !status.ProvidersHeld || status.CredentialID != summary.CredentialID || StartupGuard(t.Context(), s) != nil {
		t.Fatal("activated local startup remains fenced", status, err)
	}
	auth := func(token, instance string) (core.Scope, error) {
		r := httptest.NewRequest("POST", "/", nil)
		r.SetPathValue("project_id", f.source.project)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("X-FC-Instance", instance)
		return s.AdminScope(r)
	}
	scope, err := auth(f.token, summary.InstanceID)
	if err != nil {
		t.Fatal("fresh administrator cannot authorize", err)
	}
	if _, err = auth(f.source.token, summary.InstanceID); err == nil {
		t.Fatal("predecessor credential revived")
	}
	if _, err = auth(f.token, summary.SourceInstanceID); err == nil {
		t.Fatal("predecessor binding transparently continued")
	}
	_, err = s.Mutate(t.Context(), scope, core.Mutation{Operation: "activation.probe", Target: f.source.project, Key: "new-instance-key", Request: "explicit local mutation"}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return map[string]string{"id": "current"}, core.AppendEvent(ctx, tx, scope, "activation.probe", f.source.project, 1, nil)
	})
	if err != nil {
		t.Fatal("activated local mutation failed", err)
	}
	var origin string
	if err = s.DB().QueryRow(`SELECT instance_id FROM receipts WHERE key='new-instance-key'`).Scan(&origin); err != nil || origin != summary.InstanceID {
		t.Fatal("new receipt adopted predecessor origin", err)
	}
	// New coordination credentials and revoking the original activation admin do
	// not rewrite historical activation or resurrect that admin during retry.
	hash := sha256.Sum256([]byte("another-local-client-token-1234567890"))
	if _, err = s.DB().Exec(`INSERT INTO credentials VALUES(?,?,?,0,0)`, core.NewID("credential"), core.NewID("principal"), hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id=?`, summary.CredentialID); err != nil {
		t.Fatal(err)
	}
	beforeReplay := activationRows(t, f.dir)
	replay, err := Activate(t.Context(), f.dir, f.request)
	if err != nil || replay != summary || !reflect.DeepEqual(beforeReplay, activationRows(t, f.dir)) {
		t.Fatal("historical activation replay wrote or changed credentials", err)
	}
	if _, err = ReadRecoveryStatus(t.Context(), s); err != nil || StartupGuard(t.Context(), s) != nil {
		t.Fatal("revoked activation credential blocked valid startup", err)
	}
	if _, err = auth(f.token, summary.InstanceID); err == nil {
		t.Fatal("replay resurrected revoked activation administrator")
	}
	changed := f.request
	other := filepath.Join(t.TempDir(), "other.token")
	os.WriteFile(other, []byte("different-fresh-activation-token-123456"), 0600)
	changed.AdminTokenFile = other
	_, err = Activate(t.Context(), f.dir, changed)
	restoreCode(t, err, "restore_conflict")
	if !reflect.DeepEqual(beforeReplay, activationRows(t, f.dir)) {
		t.Fatal("changed token changed activation")
	}
}

func TestActivationRefusalsDoNotMutateRestore(t *testing.T) {
	for _, kind := range []string{"wrong instance", "wrong fence", "used token", "short token", "oversized token", "nonascii token", "whitespace token", "public token", "symlink token", "marker", "unknown metadata", "unrevoked predecessor", "newer schema", "canceled", "writer busy"} {
		t.Run(kind, func(t *testing.T) {
			f := activationTestFixture(t)
			in := f.request
			ctx := t.Context()
			want := "backup_invalid"
			cleanup := func() {}
			switch kind {
			case "wrong instance":
				in.ExpectedInstanceID = core.NewID("instance")
				want = "restore_conflict"
			case "wrong fence":
				in.FencedPredecessorID = core.NewID("instance")
				want = "restore_conflict"
			case "used token":
				os.WriteFile(in.AdminTokenFile, []byte(f.source.token), 0600)
				want = "restore_conflict"
			case "short token":
				os.WriteFile(in.AdminTokenFile, []byte("short"), 0600)
			case "oversized token":
				os.WriteFile(in.AdminTokenFile, []byte(strings.Repeat("x", 257)), 0600)
			case "nonascii token":
				os.WriteFile(in.AdminTokenFile, []byte(strings.Repeat("x", 32)+"雪"), 0600)
			case "whitespace token":
				os.WriteFile(in.AdminTokenFile, []byte(strings.Repeat("x", 32)+" x"), 0600)
			case "public token":
				os.Chmod(in.AdminTokenFile, 0644)
			case "symlink token":
				link := filepath.Join(t.TempDir(), "link")
				os.Symlink(in.AdminTokenFile, link)
				in.AdminTokenFile = link
			case "marker":
				os.WriteFile(filepath.Join(f.dir, restoreMarker), []byte("incomplete"), 0600)
				want = "restore_inactive"
			case "unknown metadata":
				activationExec(t, f.dir, `INSERT INTO metadata VALUES('restore:unknown','unproven')`)
			case "unrevoked predecessor":
				activationExec(t, f.dir, `UPDATE credentials SET revoked=0 WHERE is_admin=1`)
			case "newer schema":
				activationExec(t, f.dir, `PRAGMA user_version=3`)
			case "canceled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
				want = "backup_unavailable"
			case "writer busy":
				db, err := sql.Open("sqlite", filepath.Join(f.dir, "firecontroller.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				conn, err := db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = conn.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
					t.Fatal(err)
				}
				if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
					t.Fatal(err)
				}
				cleanup = func() { conn.ExecContext(context.Background(), `ROLLBACK`); conn.Close(); db.Close() }
				want = "backup_unavailable"
			}
			defer cleanup()
			before := activationRows(t, f.dir)
			result, err := Activate(ctx, f.dir, in)
			restoreCode(t, err, want)
			if result != (ActivationSummary{}) || !reflect.DeepEqual(before, activationRows(t, f.dir)) {
				t.Fatal("refused activation changed local records")
			}
		})
	}
}

func TestActivationCannotBootstrapOrActivateOrdinaryInstallation(t *testing.T) {
	source := newRestoreFixture(t)
	token := filepath.Join(t.TempDir(), "fresh.token")
	os.WriteFile(token, []byte("synthetic-fresh-activation-token-1234567890"), 0600)
	in := ActivationRequest{ExpectedInstanceID: source.store.InstanceID(), FencedPredecessorID: core.NewID("instance"), AdminTokenFile: token}
	before := activationRows(t, source.dir)
	_, err := Activate(t.Context(), source.dir, in)
	restoreCode(t, err, "backup_invalid")
	if !reflect.DeepEqual(before, activationRows(t, source.dir)) {
		t.Fatal("ordinary installation changed")
	}
	for _, existing := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "missing")
		if existing {
			os.Mkdir(dir, 0700)
		}
		_, err = Activate(t.Context(), dir, in)
		restoreCode(t, err, "backup_invalid")
		if _, err = os.Stat(filepath.Join(dir, "firecontroller.sqlite")); !os.IsNotExist(err) {
			t.Fatal("activation initialized missing database")
		}
		if _, err = os.Stat(filepath.Join(dir, "bootstrap-admin.token")); !os.IsNotExist(err) {
			t.Fatal("activation bootstrapped credential")
		}
	}
}

func TestRecoveryStatusStrictMetadataAndRetainedClock(t *testing.T) {
	f := activationTestFixture(t)
	summary, err := Activate(t.Context(), f.dir, f.request)
	if err != nil {
		t.Fatal(err)
	}
	s := activationOpen(t, f.dir)
	// Retained activation timestamps are structural, even after clock rollback.
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	activationExec(t, f.dir, `UPDATE metadata SET value=? WHERE key='restore:activated_at'`, future)
	if status, err := ReadRecoveryStatus(t.Context(), s); err != nil || status.ActivatedAt != future {
		t.Fatal("retained activation clock rejected", err)
	}
	activationExec(t, f.dir, `UPDATE metadata SET value=? WHERE key='restore:activated_at'`, summary.ActivatedAt)
	for _, query := range []string{`UPDATE metadata SET value='released' WHERE key='restore:provider_state'`, `DELETE FROM metadata WHERE key='restore:activation_credential_id'`, `UPDATE metadata SET value='bad' WHERE key='restore:activated_at'`, `UPDATE credentials SET is_admin=0 WHERE id='` + summary.CredentialID + `'`, `INSERT INTO metadata VALUES('restore:extra','unexpected')`} {
		tx, err := s.DB().BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(query); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		before := activationRows(t, f.dir)
		_, err = ReadRecoveryStatus(t.Context(), s)
		restoreCode(t, err, "backup_invalid")
		restoreCode(t, StartupGuard(t.Context(), s), "restore_inactive")
		if !reflect.DeepEqual(before, activationRows(t, f.dir)) {
			t.Fatal("invalid status read wrote state")
		}
		// Restore only this test's deliberately altered fields between cases.
		activationExec(t, f.dir, `DELETE FROM metadata WHERE key='restore:extra';INSERT INTO metadata(key,value) VALUES('restore:provider_state','held') ON CONFLICT(key) DO UPDATE SET value=excluded.value;INSERT INTO metadata(key,value) VALUES('restore:activation_credential_id',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value;UPDATE metadata SET value=? WHERE key='restore:activated_at';UPDATE credentials SET is_admin=1 WHERE id=?`, summary.CredentialID, summary.ActivatedAt, summary.CredentialID)
	}
}

func TestActivationMigratesV1ReceiptOriginsWithoutRewritingHistory(t *testing.T) {
	source, db := receiptOriginSource(t, 1)
	db.Close()
	bundle, err := CreateFromDirectory(t.Context(), source.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "inactive")
	restored, err := Restore(t.Context(), bundle.Path, dir)
	if err != nil {
		t.Fatal(err)
	}
	before := activationRows(t, dir)
	token := filepath.Join(t.TempDir(), "fresh.token")
	os.WriteFile(token, []byte("synthetic-fresh-activation-token-1234567890"), 0600)
	_, err = Activate(t.Context(), dir, ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	after := activationRows(t, dir)
	if len(before["receipts"]) != len(after["receipts"]) {
		t.Fatal("v1 receipts lost")
	}
	for i, row := range after["receipts"] {
		if row[0] != source.store.InstanceID() || !reflect.DeepEqual(row[1:], before["receipts"][i]) {
			t.Fatal("v1 predecessor receipts relabeled or rewritten")
		}
	}
}

func TestActivationCanceledHistoricalLookupRemainsUnavailable(t *testing.T) {
	f := activationTestFixture(t)
	summary, err := Activate(t.Context(), f.dir, f.request)
	if err != nil {
		t.Fatal(err)
	}
	s := activationOpen(t, f.dir)
	status, err := ReadRecoveryStatus(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if status.CredentialID != summary.CredentialID {
		t.Fatal("missing retained activation binding")
	}
	hash, err := activationTokenHash(f.request.AdminTokenFile)
	if err != nil {
		t.Fatal(err)
	}
	before := activationRows(t, f.dir)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	restoreCode(t, checkActivationRequest(ctx, s.DB(), status, f.request, hash), "backup_unavailable")
	if !reflect.DeepEqual(before, activationRows(t, f.dir)) {
		t.Fatal("canceled historical lookup changed state")
	}
}
