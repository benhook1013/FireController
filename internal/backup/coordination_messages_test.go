package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/reviewexec"
)

func messageBackupCall(t *testing.T, f *restoreFixture, method, path, worker, key string, body any) []byte {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("X-FC-Instance", f.store.InstanceID())
	r.Header.Set("X-FC-Worker", worker)
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code < 200 || w.Code > 299 {
		t.Fatalf("controlled message fixture HTTP status %d: %s", w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}
func messageBackupApply(t *testing.T, f *restoreFixture, batch coordimport.Batch, key string) (coordimport.Receipt, []byte) {
	t.Helper()
	base := "/api/v1/projects/" + f.project + "/coordination/imports"
	var preview coordimport.Preview
	if json.Unmarshal(messageBackupCall(t, f, "POST", base+"/preview", f.worker, "", batch), &preview) != nil {
		t.Fatal("invalid preview")
	}
	raw := messageBackupCall(t, f, "POST", base+"/apply", f.worker, key, coordimport.ApplyRequest{PreviewSHA256: preview.PreviewSHA256, Batch: batch})
	var receipt coordimport.Receipt
	if json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("invalid import receipt")
	}
	return receipt, raw
}
func messageBackupBatch(f *restoreFixture, recipient string, revision int64) coordimport.Batch {
	return coordimport.Batch{Version: 1, RepositoryID: 991, ExpectedRevision: &revision, Source: "selected-message-export", SourceSHA256: strings.Repeat("a", 64), Jobs: []jobs.PreparedJob{}, Messages: []coordimportmodel.PreparedMessage{{SourceKey: "root", SenderWorkerID: f.worker, RecipientWorkerID: recipient, Body: "  Historical <literal body>\noriginal spacing  ", CreatedAt: "2024-01-02T03:04:05.123456789+13:00"}}}
}
func messageBackupJob(f *restoreFixture) jobs.PreparedJob {
	return jobs.PreparedJob{SourceKey: "selected-job", SourceRevision: 19, Title: "Original selected job", Brief: "Original current snapshot", WorkerID: f.worker, Status: "parked", CreatedAt: "2024-01-01T00:00:00Z", UpdatedAt: "2024-02-01T00:00:00Z", Checkpoints: []jobs.PreparedCheckpoint{}}
}
func messageBackupFixture(t *testing.T, mixed bool) (*restoreFixture, coordimport.Batch, coordimport.Receipt) {
	t.Helper()
	f := newRestoreFixture(t)
	if err := coordimport.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	coordimport.Register(f.handler.(*http.ServeMux), f.store)
	recipient := f.request(t, "POST", "/api/v1/projects/"+f.project+"/workers", map[string]any{"name": "Historical recipient"})["id"].(string)
	batch := messageBackupBatch(f, recipient, 0)
	root := batch.Messages[0]
	reply := root
	reply.SourceKey = "historical-reply"
	reply.ReplyToSourceKey = "root"
	reply.SenderWorkerID = recipient
	reply.RecipientWorkerID = f.worker
	reply.Body = "Original handled reply"
	reply.CreatedAt = "2024-01-02T04:00:00+13:00"
	reply.SeenAt = "2024-01-02T05:00:00+13:00"
	reply.AcknowledgedAt = "2024-01-02T06:00:00+13:00"
	// Child-first input tests preserved receipt order and canonical native insertion.
	batch.Messages = []coordimportmodel.PreparedMessage{reply, root}
	if mixed {
		batch.Jobs = []jobs.PreparedJob{messageBackupJob(f)}
	}
	base := "/api/v1/projects/" + f.project + "/coordination/imports"
	var firstPreview coordimport.Preview
	if json.Unmarshal(messageBackupCall(t, f, "POST", base+"/preview", f.worker, "", batch), &firstPreview) != nil {
		t.Fatal("invalid first preview")
	}
	initialRequest := coordimport.ApplyRequest{PreviewSHA256: firstPreview.PreviewSHA256, Batch: batch}
	original := messageBackupCall(t, f, "POST", base+"/apply", f.worker, "initial-message-import", initialRequest)
	var receipt coordimport.Receipt
	if json.Unmarshal(original, &receipt) != nil {
		t.Fatal("invalid first receipt")
	}
	if receipt.Revision != 1 || receipt.Outcome != "imported" || receipt.Counts.NewMessages != 2 || len(receipt.MessageMappings) != 2 || receipt.MessageMappings[0].SourceKey != "historical-reply" || receipt.MessageMappings[1].SourceKey != "root" || receipt.Counts.NewJobs != len(batch.Jobs) {
		t.Fatal("import shape/counts incorrect")
	}
	rootID, replyID := receipt.MessageMappings[1].MessageID, receipt.MessageMappings[0].MessageID
	messageBackupCall(t, f, "POST", "/api/v1/projects/"+f.project+"/messages/"+rootID+"/ack", recipient, "native-ack", map[string]any{})
	// A genuine later native reply joins the imported conversation without rewriting it.
	nativeReply := f.request(t, "POST", "/api/v1/projects/"+f.project+"/messages", map[string]any{"recipient_worker_id": recipient, "reply_to_message_id": replyID, "body": "Later native follow-up"})
	if nativeReply["id"] == nil {
		t.Fatal("native reply not created")
	}
	// Exact initial replay and new-key unchanged selection must preserve evolved flags.
	beforeReplay := backupRows(t, f.store.DB())
	replay := messageBackupCall(t, f, "POST", base+"/apply", f.worker, "initial-message-import", initialRequest)
	if !bytes.Equal(replay, original) || !reflect.DeepEqual(beforeReplay, backupRows(t, f.store.DB())) {
		t.Fatal("exact replay changed evolved state or original receipt")
	}
	batch.ExpectedRevision = &receipt.Revision
	unchanged, raw := messageBackupApply(t, f, batch, "unchanged-after-native")
	if unchanged.Outcome != "unchanged" || unchanged.Counts.UnchangedMessages != 2 || unchanged.Counts.NewMessages != 0 || unchanged.Revision != 1 || bytes.Contains(raw, []byte(root.Body)) {
		t.Fatal("evolved selection changed or leaked original state")
	}
	if err := coordimport.ValidateSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	return f, batch, receipt
}
func TestCoordinationMessagesBackupPreservesOriginalAndNativeEvolution(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(map[bool]string{false: "messages-only", true: "mixed-jobs-messages"}[mixed], func(t *testing.T) {
			f, batch, receipt := messageBackupFixture(t, mixed)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			immutable := restoreTree(t, bundle.Path)
			if _, err := Inspect(t.Context(), bundle.Path); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "inactive")
			restored, err := Restore(t.Context(), bundle.Path, destination)
			if err != nil || restored.State != "inactive" {
				t.Fatal(err)
			}
			db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err = coordimport.ValidateSnapshot(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			if err = reviewexec.ValidateRoundSnapshot(t.Context(), db); err != nil {
				t.Fatal("round domain changed", err)
			}
			after := backupRows(t, db)
			for table, rows := range before {
				if table != "metadata" && table != "credentials" && after[table] != rows {
					t.Fatal("restored retained rows changed", table)
				}
			}
			for i, selected := range batch.Messages {
				mapping := receipt.MessageMappings[i]
				var body, at, parent, source, sourceKey, sourceSHA, itemJSON string
				if err = db.QueryRow(`SELECT m.body,m.created_at,COALESCE(m.reply_to_message_id,''),p.source,p.source_key,p.source_sha256,p.item_json FROM inbox_messages m JOIN coordination_import_messages p ON p.project_id=m.project_id AND p.message_id=m.id WHERE m.project_id=? AND m.id=?`, f.project, mapping.MessageID).Scan(&body, &at, &parent, &source, &sourceKey, &sourceSHA, &itemJSON); err != nil {
					t.Fatal(err)
				}
				var original coordimportmodel.PreparedMessage
				if json.Unmarshal([]byte(itemJSON), &original) != nil || !reflect.DeepEqual(original, selected) || body != selected.Body || at != selected.CreatedAt || source != batch.Source || sourceKey != selected.SourceKey || sourceSHA != batch.SourceSHA256 {
					t.Fatal("original source facts changed")
				}
				if selected.ReplyToSourceKey != "" && parent != receipt.MessageMappings[1].MessageID {
					t.Fatal("imported reply edge changed")
				}
				var seen, ack, revision int
				if err = db.QueryRow(`SELECT seen,acknowledged,revision FROM inbox_recipient_state WHERE project_id=? AND message_id=?`, f.project, mapping.MessageID).Scan(&seen, &ack, &revision); err != nil {
					t.Fatal(err)
				}
				expected := 1
				if selected.SourceKey == "root" {
					expected = 2
				}
				if seen != 1 || ack != 1 || revision != expected {
					t.Fatal("handling baseline/native ack evolution changed")
				}
			}
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
				t.Fatal("read/restore mutated original source/bundle")
			}
		})
	}
}
func TestCoordinationMessagesBackupResealedCorruptionRefused(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		physical    bool
	}{
		{"missing-parent-provenance", `DELETE FROM coordination_import_messages WHERE source_key='root'`, true},
		{"blob-source-key", `UPDATE coordination_import_messages SET source_key=CAST(source_key AS BLOB) WHERE source_key='root'`, true},
		{"wrong-storage-class", `UPDATE coordination_import_messages SET item_json=CAST(item_json AS BLOB)`, true},
		{"wrong-item-hash", `UPDATE coordination_import_messages SET item_sha256='` + strings.Repeat("b", 64) + `'`, true},
		{"wrong-source-digest", `UPDATE coordination_import_messages SET source_sha256='` + strings.Repeat("b", 64) + `'`, true},
		{"wrong-original-parent", `UPDATE coordination_import_messages SET reply_message_id='' WHERE source_key='historical-reply'`, true},
		{"handling-regressed", `UPDATE inbox_recipient_state SET acknowledged=0 WHERE message_id IN (SELECT message_id FROM coordination_import_messages WHERE source_key='historical-reply')`, true},
		{"unexpected-index", `CREATE INDEX arbitrary_message_provenance ON coordination_import_messages(source)`, false},
		{"wrong-physical-type", `ALTER TABLE coordination_import_messages ADD COLUMN extra_credit INTEGER`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _ := messageBackupFixture(t, true)
			before := backupRows(t, f.store.DB())
			bundle := restoreBundle(t, f)
			mutateBundleSQL(t, bundle.Path, tc.query)
			assertCurrentSummaryInventoryRefused(t, bundle, tc.physical)
			if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
				t.Fatal("refusal changed source")
			}
		})
	}
	t.Run("rehash-original-body", func(t *testing.T) {
		f, _, _ := messageBackupFixture(t, true)
		bundle := restoreBundle(t, f)
		var raw string
		if err := f.store.DB().QueryRow(`SELECT item_json FROM coordination_import_messages WHERE source_key='root'`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var item coordimportmodel.PreparedMessage
		if json.Unmarshal([]byte(raw), &item) != nil {
			t.Fatal("original decode")
		}
		item.Body = "Rehashed false historical body"
		changed, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(changed)
		mutateBundleSQL(t, bundle.Path, `UPDATE coordination_import_messages SET item_json=?,item_sha256=? WHERE source_key='root'`, string(changed), hex.EncodeToString(sum[:]))
		assertCurrentSummaryInventoryRefused(t, bundle, true)
	})
}
func TestCoordinationMessagesBackupVersionOnePredecessor(t *testing.T) {
	f, _, _ := coordinationImportRestoreFixture(t)
	var count int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM coordination_import_messages`).Scan(&count); err != nil || count != 0 {
		t.Fatal("predecessor table not empty", err)
	}
	if _, err := f.store.DB().Exec(`DROP TABLE coordination_import_messages; UPDATE metadata SET value='1' WHERE key='schema:coordination_import'`); err != nil {
		t.Fatal(err)
	}
	before := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	if _, err := Inspect(t.Context(), bundle.Path); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "inactive")
	if _, err := Restore(t.Context(), bundle.Path, dest); err != nil {
		t.Fatal(err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(dest, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	if err = db.QueryRow(`SELECT value FROM metadata WHERE key='schema:coordination_import'`).Scan(&version); err != nil || version != "1" {
		t.Fatal("inspection/restore migrated old schema", err)
	}
	if err = db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='coordination_import_messages'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("restore initialized optional message schema", err)
	}
	after := backupRows(t, db)
	for _, table := range []string{"coordination_import_state", "coordination_import_records", "coordination_import_batches", "jobs", "job_history", "job_checkpoints", "receipts", "events"} {
		if before[table] != after[table] {
			t.Fatal("predecessor state changed", table)
		}
	}
}
func TestCoordinationMessagesBackupRetainedBatchCardinalityRefused(t *testing.T) {
	f := newRestoreFixture(t)
	if err := coordimport.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	coordimport.Register(f.handler.(*http.ServeMux), f.store)
	recipient := f.request(t, "POST", "/api/v1/projects/"+f.project+"/workers", map[string]any{"name": "Recipient"})["id"].(string)
	batch := messageBackupBatch(f, recipient, 0)
	root := batch.Messages[0]
	batch.Messages = nil
	for i := 0; i < 100; i++ {
		item := root
		item.SourceKey = fmt.Sprintf("selected-%03d", i)
		batch.Messages = append(batch.Messages, item)
	}
	first, _ := messageBackupApply(t, f, batch, "first100")
	if first.Counts.NewMessages != 100 {
		t.Fatal("maximum actual batch not imported")
	}
	batch = messageBackupBatch(f, recipient, 1)
	batch.Messages[0].SourceKey = "additional"
	batch.Jobs = []jobs.PreparedJob{messageBackupJob(f)}
	second, _ := messageBackupApply(t, f, batch, "second")
	if second.Revision != 2 || second.Counts.NewMessages != 1 || second.Counts.NewJobs != 1 {
		t.Fatal("second bounded batch missing")
	}
	bundle := restoreBundle(t, f)
	mutateBundleSQL(t, bundle.Path, `UPDATE coordination_import_messages SET import_revision=1,created_at=(SELECT created_at FROM coordination_import_batches WHERE project_id=? AND revision=1) WHERE project_id=? AND source_key='additional'`, f.project, f.project)
	assertCurrentSummaryInventoryRefused(t, bundle, true)
}
