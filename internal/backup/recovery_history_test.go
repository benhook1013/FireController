package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	"github.com/benhook1013/FireController/internal/server"
)

func generationToken(t *testing.T) (string, string) {
	t.Helper()
	token := core.NewID("fc_admin")
	file := filepath.Join(t.TempDir(), "fresh.token")
	if err := os.WriteFile(file, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return token, file
}
func generationRead(t *testing.T, dir, query string) [][]any {
	t.Helper()
	db, err := openDatabase(t.Context(), filepath.Join(dir, "firecontroller.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	return restoreRows(t, db, query)
}
func generationBody(t *testing.T, dir string) string {
	t.Helper()
	db, err := openDatabase(t.Context(), filepath.Join(dir, "firecontroller.sqlite"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	fields, err := readRestoreFields(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
func generationCoordinate(t *testing.T, dir, token, project, worker string, oldTokens []string) {
	t.Helper()
	s, err := core.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = StartupGuard(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	mux := server.New(s, server.WithRepositoryResolver(func(context.Context, string) (server.RepositoryIdentity, error) {
		t.Fatal("coordination called provider resolver")
		return server.RepositoryIdentity{}, nil
	}))
	jobs.Register(mux, s)
	inbox.Register(mux, s)
	for _, old := range oldTokens {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+old)
		r.Header.Set("X-FC-Instance", s.InstanceID())
		if _, err = s.Authenticate(r); err == nil {
			t.Fatal("an older generation credential revived")
		}
	}
	f := &restoreFixture{dir: dir, store: s, token: token, project: project, worker: worker, handler: mux}
	f.request(t, "POST", "/api/v1/projects/"+project+"/jobs", map[string]any{"title": "Current generation local job", "worker_id": worker})
	f.request(t, "POST", "/api/v1/projects/"+project+"/messages", map[string]any{"recipient_worker_id": worker, "body": "Current generation retained message"})
}

func TestRecoveryHistoryThreeGenuineGenerationsPreserveOriginsAndBytes(t *testing.T) {
	f := activationTestFixture(t)
	dir, token, request := f.dir, f.token, f.request
	oldTokens := []string{f.source.token}
	expectedHistory := map[string]string{}
	journalQueries := []string{`SELECT * FROM review_operations ORDER BY id`, `SELECT * FROM review_execution_attempts ORDER BY id`, `SELECT * FROM review_execution_transitions ORDER BY attempt_id,revision`, `SELECT * FROM review_execution_reservations ORDER BY attempt_id`}
	journals := make([][][]any, len(journalQueries))
	for i, q := range journalQueries {
		journals[i] = generationRead(t, dir, q)
	}
	for generation := 1; generation <= 3; generation++ {
		active, err := Activate(t.Context(), dir, request)
		if err != nil {
			t.Fatal(generation, err)
		}
		generationCoordinate(t, dir, token, f.source.project, f.source.worker, oldTokens)
		beforeReplay := activationRows(t, dir)
		replay, err := Activate(t.Context(), dir, request)
		if err != nil || replay != active || !reflect.DeepEqual(beforeReplay, activationRows(t, dir)) {
			t.Fatal("generation activation replay wrote state", generation, err)
		}
		if generation == 1 && len(generationRead(t, dir, `SELECT name FROM sqlite_schema WHERE name='restore_history'`)) != 0 {
			t.Fatal("activation invented history schema")
		}
		body := generationBody(t, dir)
		sourceBefore := restoreTree(t, dir)
		receipts := generationRead(t, dir, `SELECT * FROM receipts ORDER BY rowid`)
		bundle, err := CreateFromDirectory(t.Context(), dir, filepath.Join(t.TempDir(), "bundles"))
		if err != nil {
			t.Fatal("activated source backup rejected", generation, err)
		}
		if !reflect.DeepEqual(sourceBefore, restoreTree(t, dir)) {
			t.Fatal("backup modified active source")
		}
		immutable := restoreTree(t, bundle.Path)
		if _, err = Inspect(t.Context(), bundle.Path); err != nil || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
			t.Fatal("active bundle inspection wrote or failed", err)
		}
		if generation == 3 {
			break
		}
		next := filepath.Join(t.TempDir(), "next")
		restored, err := Restore(t.Context(), bundle.Path, next)
		if err != nil {
			t.Fatal("second/later restore rejected", generation, err)
		}
		if restored.SourceInstanceID != active.InstanceID || restored.InstanceID == active.InstanceID {
			t.Fatal("generation identity reused")
		}
		if !reflect.DeepEqual(receipts, generationRead(t, next, `SELECT * FROM receipts ORDER BY rowid`)) {
			t.Fatal("mixed receipt origins or bytes rewritten")
		}
		expectedHistory[active.InstanceID] = body
		actual := generationRead(t, next, `SELECT instance_id,body_json FROM restore_history ORDER BY instance_id`)
		if len(actual) != len(expectedHistory) {
			t.Fatal("wrong archive count")
		}
		for _, row := range actual {
			if expectedHistory[row[0].(string)] != row[1].(string) {
				t.Fatal("ancestor metadata bytes changed")
			}
		}
		if got := generationRead(t, next, `SELECT count(*) FROM metadata WHERE key GLOB 'restore:*'`); !reflect.DeepEqual(got, [][]any{{int64(6)}}) {
			t.Fatal("activation extras leaked into next inactive state")
		}
		if got := generationRead(t, next, `SELECT count(*) FROM credentials WHERE revoked<>1`); !reflect.DeepEqual(got, [][]any{{int64(0)}}) {
			t.Fatal("credential revival across restore")
		}
		beforeInactive := activationRows(t, next)
		restoreCode(t, ValidateSource(t.Context(), next), "backup_invalid")
		if !reflect.DeepEqual(beforeInactive, activationRows(t, next)) {
			t.Fatal("inactive source refusal wrote data")
		}
		for i, q := range journalQueries {
			if !reflect.DeepEqual(journals[i], generationRead(t, next, q)) {
				t.Fatal("restore advanced provider journal")
			}
		}
		if !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
			t.Fatal("restore modified immutable source bundle")
		}
		oldTokens = append(oldTokens, token)
		var file string
		token, file = generationToken(t)
		dir = next
		request = ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: file}
	}
	// Original and all three activated generations retain distinct receipt origins.
	if got := generationRead(t, dir, `SELECT count(DISTINCT instance_id) FROM receipts`); !reflect.DeepEqual(got, [][]any{{int64(4)}}) {
		t.Fatal("generation receipt namespaces collapsed", got)
	}
	for i, q := range journalQueries {
		if !reflect.DeepEqual(journals[i], generationRead(t, dir, q)) {
			t.Fatal("activation resumed provider work")
		}
	}
	for _, name := range []string{"review-captures", "hosted-review-captures"} {
		if !reflect.DeepEqual(restoreTree(t, filepath.Join(f.source.dir, name)), restoreTree(t, filepath.Join(dir, name))) {
			t.Fatal("forensic artifacts changed across generations")
		}
	}
}

func secondGenerationFixture(t *testing.T) (*activationFixture, ActivationSummary) {
	t.Helper()
	f := activationTestFixture(t)
	if _, err := Activate(t.Context(), f.dir, f.request); err != nil {
		t.Fatal("first activation", err)
	}
	bundle, err := CreateFromDirectory(t.Context(), f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal("active source backup", err)
	}
	next := filepath.Join(t.TempDir(), "second")
	restored, err := Restore(t.Context(), bundle.Path, next)
	if err != nil {
		t.Fatal("second restore", err)
	}
	token, file := generationToken(t)
	f.dir, f.token = next, token
	f.request = ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: file}
	active, err := Activate(t.Context(), next, f.request)
	if err != nil {
		t.Fatal("second activation", err)
	}
	return f, active
}
func historyRewrite(t *testing.T, dir string, change func(map[string]string)) {
	t.Helper()
	rows := generationRead(t, dir, `SELECT instance_id,body_json FROM restore_history LIMIT 1`)
	var fields map[string]string
	if err := json.Unmarshal([]byte(rows[0][1].(string)), &fields); err != nil {
		t.Fatal(err)
	}
	change(fields)
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	activationExec(t, dir, `UPDATE restore_history SET body_json=? WHERE instance_id=?`, string(body), rows[0][0])
}
func TestRecoveryHistoryMalformedSourcesFailReadOnly(t *testing.T) {
	for _, kind := range []string{"noncanonical", "duplicate JSON", "unknown field", "null", "oversized", "current instance", "unlinked", "cycle", "wrong credential", "unrevoked archived admin", "unproven provider state", "future sequence", "ordinary source", "missing version", "missing table", "foreign key", "wrong PK"} {
		t.Run(kind, func(t *testing.T) {
			f, active := secondGenerationFixture(t)
			switch kind {
			case "noncanonical":
				activationExec(t, f.dir, `UPDATE restore_history SET body_json=' '||body_json`)
			case "duplicate JSON":
				rows := generationRead(t, f.dir, `SELECT body_json FROM restore_history`)
				body := rows[0][0].(string)
				activationExec(t, f.dir, `UPDATE restore_history SET body_json=?`, body[:len(body)-1]+`,"restore:state":"coordination_active"}`)
			case "unknown field":
				historyRewrite(t, f.dir, func(m map[string]string) { m["restore:unknown"] = "extra" })
			case "null":
				activationExec(t, f.dir, `UPDATE restore_history SET body_json='null'`)
			case "oversized":
				activationExec(t, f.dir, `UPDATE restore_history SET body_json=?`, strings.Repeat(" ", maxRecoveryHistoryBytes+1))
			case "current instance":
				activationExec(t, f.dir, `UPDATE restore_history SET instance_id=?`, active.InstanceID)
			case "unlinked":
				activationExec(t, f.dir, `UPDATE restore_history SET instance_id=?`, core.NewID("instance"))
			case "cycle":
				historyRewrite(t, f.dir, func(m map[string]string) { m["restore:source_instance_id"] = active.InstanceID })
			case "wrong credential":
				historyRewrite(t, f.dir, func(m map[string]string) { m["restore:activation_credential_id"] = core.NewID("credential") })
			case "unrevoked archived admin":
				var fields map[string]string
				json.Unmarshal([]byte(generationRead(t, f.dir, `SELECT body_json FROM restore_history`)[0][0].(string)), &fields)
				activationExec(t, f.dir, `UPDATE credentials SET revoked=0 WHERE id=?`, fields["restore:activation_credential_id"])
			case "unproven provider state":
				historyRewrite(t, f.dir, func(m map[string]string) { m["restore:provider_state"] = "released" })
			case "future sequence":
				historyRewrite(t, f.dir, func(m map[string]string) { m["restore:event_sequence"] = "999999" })
			case "ordinary source":
				activationExec(t, f.dir, `DELETE FROM metadata WHERE key GLOB 'restore:*'`)
			case "missing version":
				activationExec(t, f.dir, `DELETE FROM metadata WHERE key='schema:recovery_history'`)
			case "missing table":
				activationExec(t, f.dir, `DROP TABLE restore_history`)
			case "foreign key":
				activationExec(t, f.dir, `ALTER TABLE restore_history RENAME TO old_history;CREATE TABLE restore_history(instance_id TEXT PRIMARY KEY REFERENCES credentials(id),body_json TEXT NOT NULL);INSERT INTO restore_history SELECT * FROM old_history;DROP TABLE old_history`)
			case "wrong PK":
				activationExec(t, f.dir, `ALTER TABLE restore_history RENAME TO old_history;CREATE TABLE restore_history(instance_id TEXT,body_json TEXT NOT NULL);INSERT INTO restore_history SELECT * FROM old_history;DROP TABLE old_history`)
			}
			before := activationRows(t, f.dir)
			restoreCode(t, ValidateSource(t.Context(), f.dir), "backup_invalid")
			{
				s := activationOpen(t, f.dir)
				_, err := ReadRecoveryStatus(t.Context(), s)
				restoreCode(t, err, "backup_invalid")
				restoreCode(t, StartupGuard(t.Context(), s), "restore_inactive")
			}
			if !reflect.DeepEqual(before, activationRows(t, f.dir)) {
				t.Fatal("invalid history read mutated source")
			}
			// Failed creation must not publish a usable bundle or mutate the source.
			root := filepath.Join(t.TempDir(), "invalid-destination")
			if _, err := CreateFromDirectory(t.Context(), f.dir, root); err == nil {
				t.Fatal("malformed recovery backup published")
			}
			if !reflect.DeepEqual(before, activationRows(t, f.dir)) {
				t.Fatal("failed backup changed source")
			}
		})
	}
}

func TestRecoveryHistoryRetainedClockAndRehashedInspection(t *testing.T) {
	f, _ := secondGenerationFixture(t)
	historyRewrite(t, f.dir, func(m map[string]string) {
		m["restore:activated_at"] = time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	})
	bundle, err := CreateFromDirectory(t.Context(), f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal("historical clock rollback invalidated source", err)
	}
	original := restoreTree(t, bundle.Path)
	if _, err = Inspect(t.Context(), bundle.Path); err != nil || !reflect.DeepEqual(original, restoreTree(t, bundle.Path)) {
		t.Fatal("clock-safe inspection failed or wrote", err)
	}
	mutateBundleSQL(t, bundle.Path, `UPDATE restore_history SET body_json='null'`)
	before := restoreTree(t, bundle.Path)
	_, err = Inspect(t.Context(), bundle.Path)
	restoreCode(t, err, "backup_invalid")
	dest := filepath.Join(t.TempDir(), "must-not-restore")
	_, err = Restore(t.Context(), bundle.Path, dest)
	restoreCode(t, err, "backup_invalid")
	if _, err = os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("invalid history created destination")
	}
	if !reflect.DeepEqual(before, restoreTree(t, bundle.Path)) {
		t.Fatal("invalid history inspect/restore wrote source")
	}
}

// The capacity fixture synthesizes old, strictly bound history only; ordinary
// Restore/Activate APIs above prove generation creation and byte preservation.
func seedRecoveryHistoryCapacity(t *testing.T, dir string, count int) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	fields, err := readRestoreFields(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), recoveryHistoryDDL+`;INSERT INTO metadata VALUES('schema:recovery_history','1')`); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, count+1)
	for i := range ids {
		ids[i] = core.NewID("instance")
	}
	if _, err = tx.ExecContext(t.Context(), `UPDATE metadata SET value=? WHERE key='restore:source_instance_id'`, ids[0]); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		credential := core.NewID("credential")
		if _, err = tx.ExecContext(t.Context(), `INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES(?,?,?,1,1)`, credential, core.NewID("principal"), fmt.Sprintf("%064x", i+1000)); err != nil {
			t.Fatal(err)
		}
		fields["restore:source_instance_id"] = ids[i+1]
		fields["restore:activation_credential_id"] = credential
		fields["restore:event_sequence"] = "0"
		body, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.ExecContext(t.Context(), `INSERT INTO restore_history VALUES(?,?)`, ids[i], string(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryHistoryCapacityPreservesUsableCoordinationAndRestorableBundles(t *testing.T) {
	f := activationTestFixture(t)
	if _, err := Activate(t.Context(), f.dir, f.request); err != nil {
		t.Fatal(err)
	}
	seedRecoveryHistoryCapacity(t, f.dir, maxRecoveryGenerations-1)
	before := activationRows(t, f.dir)
	bundle, err := CreateFromDirectory(t.Context(), f.dir, filepath.Join(t.TempDir(), "bundles"))
	if err != nil {
		t.Fatal("999 archives must leave room for a genuine restore", err)
	}
	if !reflect.DeepEqual(before, activationRows(t, f.dir)) {
		t.Fatal("capacity backup mutated source")
	}
	next := filepath.Join(t.TempDir(), "at-capacity")
	restored, err := Restore(t.Context(), bundle.Path, next)
	if err != nil {
		t.Fatal("restore to exactly1000 archives rejected", err)
	}
	if got := generationRead(t, next, `SELECT count(*) FROM restore_history`); !reflect.DeepEqual(got, [][]any{{int64(maxRecoveryGenerations)}}) {
		t.Fatal("restore lost archived generations", got)
	}
	_, file := generationToken(t)
	if _, err = Activate(t.Context(), next, ActivationRequest{ExpectedInstanceID: restored.InstanceID, FencedPredecessorID: restored.SourceInstanceID, AdminTokenFile: file}); err != nil {
		t.Fatal("valid bounded destination cannot activate", err)
	}
	stable := activationRows(t, next)
	if err = ValidateSource(t.Context(), next); err != nil {
		t.Fatal("generic live preflight must remain usable", err)
	}
	s := activationOpen(t, next)
	if err = StartupGuard(t.Context(), s); err != nil {
		t.Fatal("bounded history blocked coordination startup", err)
	}
	if _, err = ReadRecoveryStatus(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadStatus(t.Context(), next); err != nil {
		t.Fatal("bounded history blocked backup status", err)
	}
	root := filepath.Join(t.TempDir(), "must-not-publish")
	_, err = CreateFromDirectory(t.Context(), next, root)
	restoreCode(t, err, "backup_invalid")
	if entries, err := os.ReadDir(root); err != nil && !os.IsNotExist(err) || len(entries) != 0 {
		t.Fatal("unrestorable bundle was published or left partial state", err)
	}
	if !reflect.DeepEqual(stable, activationRows(t, next)) {
		t.Fatal("capacity refusal/read changed active source")
	}
	// A fully rehashed apparent source bundle at the limit is rejected on
	// inspection too, rather than gaining a verified but unrestorable status.
	snapshotDir := t.TempDir()
	if err = os.Chmod(snapshotDir, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(snapshotDir, "snapshot.sqlite")
	if err = core.SnapshotDatabase(t.Context(), s.DB(), snapshot); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(bundle.Path, "firecontroller.sqlite"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	resealBundle(t, bundle.Path, func(m *Manifest) { m.SourceInstanceID = restored.InstanceID })
	bundleBefore := restoreTree(t, bundle.Path)
	_, err = Inspect(t.Context(), bundle.Path)
	restoreCode(t, err, "backup_invalid")
	if !reflect.DeepEqual(bundleBefore, restoreTree(t, bundle.Path)) {
		t.Fatal("capacity inspection wrote source bundle")
	}
	// Build the over-limit malformed chain separately: no accepted history may
	// exceed the supported bound, even outside the publication path.
	row := generationRead(t, next, `SELECT instance_id,body_json FROM restore_history ORDER BY instance_id LIMIT 1`)[0]
	activationExec(t, next, `INSERT INTO restore_history VALUES(?,?)`, core.NewID("instance"), row[1])
	restoreCode(t, ValidateSource(t.Context(), next), "backup_invalid")
	restoreCode(t, StartupGuard(t.Context(), s), "restore_inactive")
}
