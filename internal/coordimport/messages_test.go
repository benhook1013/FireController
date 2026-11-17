package coordimport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/coordimport"
	"github.com/benhook1013/FireController/internal/coordimportmodel"
)

func messageBatch(sender, recipient string, revision int64) coordimport.Batch {
	b := preparedBatch(sender, revision)
	b.Jobs = nil
	b.Messages = []coordimportmodel.PreparedMessage{{SourceKey: "root", SenderWorkerID: sender, RecipientWorkerID: recipient, Body: "Original historical <script>text</script>\nExact body", CreatedAt: "2024-02-01T00:00:00.123456789+01:00"}}
	return b
}
func assertImportSnapshot(t *testing.T, f *importFixture) {
	t.Helper()
	tx, err := f.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = coordimport.ValidateSnapshot(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
}
func TestPreparedMessagesMixedGraphReadsAndRetainedParentReplay(t *testing.T) {
	f := newImportFixture(t)
	recipient := f.workerFor(f.project, "Recipient")
	batch := messageBatch(f.worker, recipient, 0)
	root := batch.Messages[0]
	reply := root
	reply.SourceKey = "reply"
	reply.ReplyToSourceKey = "root"
	reply.SenderWorkerID = recipient
	reply.RecipientWorkerID = f.worker
	reply.Body = "Historical reply"
	reply.CreatedAt = "2024-02-01T01:00:00Z"
	reply.SeenAt = "2024-02-01T02:00:00Z"
	reply.AcknowledgedAt = "2024-02-01T03:00:00Z"
	batch.Messages = []coordimportmodel.PreparedMessage{reply, root}
	batch.Jobs = preparedBatch(f.worker, 0).Jobs
	before := f.snapshot(false)
	p := f.preview(batch)
	conserved(t, before, f.snapshot(false))
	if p.Counts.Messages != 2 || p.Counts.NewMessages != 2 || p.Counts.NewJobs != 1 {
		t.Fatal("mixed preview counts")
	}
	w := f.apply(batch, p, "mixed")
	receipt := checked[coordimport.Receipt](t, w, 200)
	if receipt.Revision != 1 || receipt.Counts != p.Counts || len(receipt.MessageMappings) != 2 || receipt.MessageMappings[0].SourceKey != "reply" || receipt.MessageMappings[1].SourceKey != "root" {
		t.Fatal("mixed receipt order/counts")
	}
	if strings.Contains(w.Body.String(), root.Body) || strings.Contains(w.Body.String(), reply.Body) {
		t.Fatal("receipt exposed bodies")
	}
	rm, cm := receipt.MessageMappings[1], receipt.MessageMappings[0]
	if rm.ConversationID != cm.ConversationID {
		t.Fatal("reply conversation changed")
	}
	var parent, body, created string
	var rootSeq, childSeq int64
	if err := f.store.DB().QueryRow(`SELECT sequence FROM inbox_messages WHERE project_id=? AND id=?`, f.project, rm.MessageID).Scan(&rootSeq); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT sequence,reply_to_message_id,body,created_at FROM inbox_messages WHERE project_id=? AND id=?`, f.project, cm.MessageID).Scan(&childSeq, &parent, &body, &created); err != nil {
		t.Fatal(err)
	}
	if rootSeq >= childSeq || parent != rm.MessageID || body != reply.Body || created != reply.CreatedAt {
		t.Fatal("unordered graph did not preserve native originals")
	}
	projectBase := "/api/v1/projects/" + f.project
	page := checked[struct {
		Items []struct {
			ID        string `json:"id"`
			Body      string `json:"body"`
			CreatedAt string `json:"created_at"`
		}
		HasMore bool `json:"has_more"`
	}](t, f.call("GET", projectBase+"/conversations/"+rm.ConversationID, f.admin, f.worker, "", nil), 200)
	if len(page.Items) != 2 || page.HasMore || page.Items[0].ID != rm.MessageID || page.Items[0].Body != root.Body || page.Items[0].CreatedAt != root.CreatedAt {
		t.Fatal("native conversation does not expose originals")
	}
	inbox := checked[struct {
		Items []struct {
			ID           string `json:"id"`
			Seen         bool   `json:"seen"`
			Acknowledged bool   `json:"acknowledged"`
		}
	}](t, f.call("GET", projectBase+"/inbox/"+recipient, f.admin, recipient, "", nil), 200)
	if len(inbox.Items) != 1 || inbox.Items[0].ID != rm.MessageID || inbox.Items[0].Seen || inbox.Items[0].Acknowledged {
		t.Fatal("imported pending message undiscoverable")
	}
	var historicalEvents, importEvents int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type LIKE 'message.%'`).Scan(&historicalEvents); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='coordination.imported'`).Scan(&importEvents); err != nil {
		t.Fatal(err)
	}
	if historicalEvents != 0 || importEvents != 1 {
		t.Fatal("import replayed historical events")
	}
	assertImportSnapshot(t, f)
	// Evolve recipient state using the actual native API, then neither exact replay
	// nor a new-key unchanged batch may rewind it or replace original handling data.
	checked[map[string]any](t, f.call("POST", projectBase+"/messages/"+rm.MessageID+"/ack", f.admin, recipient, "ack", map[string]any{}), 200)
	replayBefore := f.snapshot(false)
	replay := f.apply(batch, p, "mixed")
	if replay.Body.String() != w.Body.String() {
		t.Fatal("exact replay changed")
	}
	conserved(t, replayBefore, f.snapshot(false))
	batch.ExpectedRevision = &receipt.Revision
	unchangedBefore := f.snapshot(true)
	unchanged := checked[coordimport.Receipt](t, f.apply(batch, f.preview(batch), "unchanged"), 200)
	conserved(t, unchangedBefore, f.snapshot(true))
	if unchanged.Outcome != "unchanged" || unchanged.Revision != 1 || unchanged.Counts.UnchangedMessages != 2 || unchanged.Counts.NewMessages != 0 {
		t.Fatal("new-key import did not remain unchanged")
	}
	assertImportSnapshot(t, f)
	// A later batch explicitly selects a new reply to retained same-source history.
	next := messageBatch(recipient, f.worker, 1)
	next.Messages[0].SourceKey = "later"
	next.Messages[0].ReplyToSourceKey = "root"
	next.Messages[0].Body = "Later selected reply"
	nextPreview := f.preview(next)
	added := checked[coordimport.Receipt](t, f.apply(next, nextPreview, "later"), 200)
	if added.Revision != 2 || added.MessageMappings[0].ConversationID != rm.ConversationID {
		t.Fatal("retained parent source mapping lost")
	}
	f.restart()
	before = f.snapshot(false)
	again := f.apply(next, nextPreview, "later")
	checked[coordimport.Receipt](t, again, 200)
	conserved(t, before, f.snapshot(false))
	assertImportSnapshot(t, f)
	if f.lookups != 1 {
		t.Fatal("message import called repository provider")
	}
}

func TestPreparedMessagesGraphAuthorityAndAtomicRefusals(t *testing.T) {
	f := newImportFixture(t)
	recipient := f.workerFor(f.project, "Recipient")
	third := f.workerFor(f.project, "Third")
	foreign := f.workerFor(f.projectFor("owner/two"), "Foreign")
	initial := messageBatch(f.worker, recipient, 0)
	p := f.preview(initial)
	checked[coordimport.Receipt](t, f.apply(initial, p, "root"), 200)
	cases := map[string]func(*coordimport.Batch){
		"missing-parent":   func(b *coordimport.Batch) { b.Messages[0].ReplyToSourceKey = "not-imported" },
		"different-source": func(b *coordimport.Batch) { b.Source = "different"; b.Messages[0].ReplyToSourceKey = "root" },
		"third-participant": func(b *coordimport.Batch) {
			b.Messages[0].ReplyToSourceKey = "root"
			b.Messages[0].SenderWorkerID = third
		},
		"unmapped-worker":      func(b *coordimport.Batch) { b.Messages[0].RecipientWorkerID = "missing" },
		"cross-project-worker": func(b *coordimport.Batch) { b.Messages[0].RecipientWorkerID = foreign },
		"cycle": func(b *coordimport.Batch) {
			other := b.Messages[0]
			other.SourceKey = "second"
			other.ReplyToSourceKey = "later"
			b.Messages[0].ReplyToSourceKey = "second"
			b.Messages = append(b.Messages, other)
		},
		"changed-original": func(b *coordimport.Batch) {
			b.Messages[0].SourceKey = "root"
			b.Messages[0].Body = "Different original"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			b := messageBatch(f.worker, recipient, 1)
			b.Messages[0].SourceKey = "later"
			change(&b)
			before := f.snapshot(false)
			w := f.call("POST", f.base()+"/preview", f.admin, f.worker, "", b)
			if w.Code != 409 && w.Code != 404 {
				t.Fatalf("refusal status %d", w.Code)
			}
			conserved(t, before, f.snapshot(false))
		})
	}
	b := messageBatch(f.worker, recipient, 1)
	b.Messages[0].SourceKey = "later"
	p = f.preview(b)
	token := "read-only-message-import"
	f.grant(f.project, token, false)
	before := f.snapshot(false)
	errorIs(t, f.call("POST", f.base()+"/apply", token, f.worker, "denied", coordimport.ApplyRequest{PreviewSHA256: p.PreviewSHA256, Batch: b}), 403, "forbidden")
	other := f.workerFor(f.project, "Other importer")
	before = f.snapshot(false)
	errorIs(t, f.call("POST", f.base()+"/apply", f.admin, other, "wrong-worker", coordimport.ApplyRequest{PreviewSHA256: p.PreviewSHA256, Batch: b}), 409, "import_preview_mismatch")
	conserved(t, before, f.snapshot(false))
	// Stale preview/apply refuses after another valid import advances the same CAS.
	parallel := b
	parallel.Messages = append([]coordimportmodel.PreparedMessage(nil), b.Messages...)
	parallel.Messages[0].SourceKey = "parallel"
	checked[coordimport.Receipt](t, f.apply(parallel, f.preview(parallel), "parallel"), 200)
	before = f.snapshot(false)
	errorIs(t, f.apply(b, p, "stale"), 409, "coordination_import_conflict")
	conserved(t, before, f.snapshot(false))
}
func TestPreparedMessagesMixedFailureRollsBackEverything(t *testing.T) {
	for _, stage := range []string{"message-provenance", "event"} {
		t.Run(stage, func(t *testing.T) {
			f := newImportFixture(t)
			recipient := f.workerFor(f.project, "Recipient")
			b := messageBatch(f.worker, recipient, 0)
			b.Jobs = preparedBatch(f.worker, 0).Jobs
			p := f.preview(b)
			trigger := `CREATE TRIGGER message_import_failure BEFORE INSERT ON coordination_import_messages BEGIN SELECT RAISE(ABORT,'controlled failure'); END`
			if stage == "event" {
				trigger = `CREATE TRIGGER message_import_failure BEFORE INSERT ON events WHEN NEW.event_type='coordination.imported' BEGIN SELECT RAISE(ABORT,'controlled failure'); END`
			}
			if _, err := f.store.DB().Exec(trigger); err != nil {
				t.Fatal(err)
			}
			before := f.snapshot(false)
			w := f.apply(b, p, "retryable")
			if w.Code != 500 {
				t.Fatalf("failure status %d", w.Code)
			}
			conserved(t, before, f.snapshot(false))
			if _, err := f.store.DB().Exec(`DROP TRIGGER message_import_failure`); err != nil {
				t.Fatal(err)
			}
			got := checked[coordimport.Receipt](t, f.apply(b, p, "retryable"), 200)
			if got.Revision != 1 || got.Counts.NewJobs != 1 || got.Counts.NewMessages != 1 {
				t.Fatal("rollback consumed key or leaked partial import")
			}
			assertImportSnapshot(t, f)
		})
	}
}
func TestPreparedMessagesBoundsAndLegacyCanonicalBytes(t *testing.T) {
	worker := "worker_one"
	old := preparedBatch(worker, 0)
	normalized, raw, err := coordimport.Normalize(old)
	if err != nil {
		t.Fatal(err)
	}
	oldRaw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(oldRaw) || strings.Contains(string(raw), "messages") {
		t.Fatal("jobs-only canonical wire changed")
	}
	oldReceipt := coordimport.Receipt{Mappings: []coordimport.Mapping{}, Counts: coordimport.Counts{Jobs: 1, Checkpoints: 2, NewJobs: 1}}
	r, err := json.Marshal(oldReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(r), "messages") {
		t.Fatal("jobs-only receipt wire changed")
	}
	if !reflect.DeepEqual(normalized.Jobs, old.Jobs) {
		t.Fatal("normalization mutated original jobs")
	}
	b := messageBatch(worker, "worker_two", 0)
	b.Messages = nil
	if _, _, err := coordimport.Normalize(b); err == nil {
		t.Fatal("empty batch accepted")
	}
	for i := 0; i < 100; i++ {
		m := messageBatch(worker, "worker_two", 0).Messages[0]
		m.SourceKey = fmt.Sprintf("message-%03d", i)
		b.Messages = append(b.Messages, m)
	}
	n, raw, err := coordimport.Normalize(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Messages) != 100 || !strings.Contains(string(raw), `"jobs":[]`) {
		t.Fatal("messages-only normalized jobs not canonical empty array")
	}
	m := b.Messages[0]
	m.SourceKey = "message-101"
	b.Messages = append(b.Messages, m)
	if _, _, err := coordimport.Normalize(b); err == nil {
		t.Fatal("101 messages accepted")
	}
	b.Messages = b.Messages[:100]
	b.Messages[1].SourceKey = b.Messages[0].SourceKey
	if _, _, err := coordimport.Normalize(b); err == nil {
		t.Fatal("duplicate message source key accepted")
	}
	b.Messages = b.Messages[:1]
	b.Messages[0].Body = strings.Repeat("a", 16<<10)
	for len(b.Messages) < 20 {
		m := b.Messages[0]
		m.SourceKey = fmt.Sprint(len(b.Messages))
		b.Messages = append(b.Messages, m)
	}
	if _, _, err := coordimport.Normalize(b); err == nil {
		t.Fatal("oversized eventual apply accepted")
	}
}

func TestPreparedMessagesSnapshotCorruptionAndV1Upgrade(t *testing.T) {
	f := newImportFixture(t)
	recipient := f.workerFor(f.project, "Recipient")
	b := messageBatch(f.worker, recipient, 0)
	root := b.Messages[0]
	reply := root
	reply.SourceKey = "reply"
	reply.ReplyToSourceKey = "root"
	reply.SeenAt = "2024-03-01T00:00:00Z"
	reply.AcknowledgedAt = "2024-03-02T00:00:00Z"
	b.Messages = append(b.Messages, reply)
	receipt := checked[coordimport.Receipt](t, f.apply(b, f.preview(b), "original"), 200)
	assertImportSnapshot(t, f)
	rootID, replyID := receipt.MessageMappings[0].MessageID, receipt.MessageMappings[1].MessageID
	tests := map[string]func() (string, []any){
		"native-original": func() (string, []any) { return `UPDATE inbox_messages SET body='changed' WHERE id=?`, []any{rootID} },
		"root-pointer":    func() (string, []any) { return `UPDATE inbox_conversations SET root_message_id=?`, []any{replyID} },
		"reply-baseline": func() (string, []any) {
			return `UPDATE coordination_import_messages SET reply_message_id='' WHERE source_key='reply'`, nil
		},
		"missing-source-parent": func() (string, []any) { return `DELETE FROM coordination_import_messages WHERE source_key='root'`, nil },
		"source-namespace": func() (string, []any) {
			return `UPDATE coordination_import_messages SET source='different' WHERE source_key='reply'`, nil
		},
		"imported-handling-regression": func() (string, []any) {
			return `UPDATE inbox_recipient_state SET acknowledged=0 WHERE message_id=?`, []any{replyID}
		},
		"missing-recipient-state": func() (string, []any) { return `DELETE FROM inbox_recipient_state WHERE message_id=?`, []any{replyID} },
		"recipient-revision": func() (string, []any) {
			return `UPDATE inbox_recipient_state SET revision=7 WHERE message_id=?`, []any{replyID}
		},
		"blob-source-key": func() (string, []any) {
			return `UPDATE coordination_import_messages SET source_key=CAST(source_key AS BLOB) WHERE source_key='reply'`, nil
		},
		"malformed-original": func() (string, []any) {
			return `UPDATE coordination_import_messages SET item_json='{}' WHERE source_key='reply'`, nil
		},
		"batch-missing-domain": func() (string, []any) {
			return `UPDATE metadata SET value='1' WHERE key='schema:coordination_import'`, nil
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			tx, err := f.store.DB().BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			query, args := change()
			if _, err = tx.Exec(query, args...); err != nil {
				t.Fatal(err)
			}
			if err = coordimport.ValidateSnapshot(context.Background(), tx); err == nil {
				t.Fatal("corruption admitted")
			}
		})
	}
	assertImportSnapshot(t, f)
	// Genuine predecessor domain fixture: preserve exact v1 tables and records,
	// remove only additive v2 table/metadata before exercising actual startup upgrade.
	t.Run("predecessor-v1", func(t *testing.T) {
		old := newImportFixture(t)
		jobs := preparedBatch(old.worker, 0)
		original := checked[coordimport.Receipt](t, old.apply(jobs, old.preview(jobs), "jobs"), 200)
		if original.Revision != 1 {
			t.Fatal("v1 source fixture")
		}
		if _, err := old.store.DB().Exec(`DROP TABLE coordination_import_messages`); err != nil {
			t.Fatal(err)
		}
		if _, err := old.store.DB().Exec(`UPDATE metadata SET value='1' WHERE key='schema:coordination_import'`); err != nil {
			t.Fatal(err)
		}
		assertImportSnapshot(t, old)
		var before string
		if err := old.store.DB().QueryRow(`SELECT item_json FROM coordination_import_records`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := coordimport.EnsureSchema(context.Background(), old.store); err != nil {
			t.Fatal(err)
		}
		var after string
		if err := old.store.DB().QueryRow(`SELECT item_json FROM coordination_import_records`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Fatal("v2 upgrade changed v1 original")
		}
		assertImportSnapshot(t, old)
	})
}

func TestPreparedMessagesMaximumMixedBatch(t *testing.T) {
	f := newImportFixture(t)
	recipient := f.workerFor(f.project, "Recipient")
	b := messageBatch(f.worker, recipient, 0)
	m := b.Messages[0]
	b.Messages = nil
	for i := 0; i < 100; i++ {
		item := m
		item.SourceKey = fmt.Sprintf("root-%03d", i)
		b.Messages = append(b.Messages, item)
	}
	job := preparedBatch(f.worker, 0).Jobs[0]
	extra := job.Checkpoints[1]
	extra.SourceKey = "third"
	job.Checkpoints = append(job.Checkpoints, extra)
	extra.SourceKey = "fourth"
	job.Checkpoints = append(job.Checkpoints, extra)
	for i := 0; i < 25; i++ {
		item := job
		item.SourceKey = fmt.Sprintf("job-%02d", i)
		b.Jobs = append(b.Jobs, item)
	}
	before := f.snapshot(false)
	p := f.preview(b)
	conserved(t, before, f.snapshot(false))
	if p.Counts.Jobs != 25 || p.Counts.Checkpoints != 100 || p.Counts.Messages != 100 {
		t.Fatal("maximum preview counts")
	}
	receipt := checked[coordimport.Receipt](t, f.apply(b, p, "maximum"), 200)
	if len(receipt.Mappings) != 25 || len(receipt.MessageMappings) != 100 || receipt.Counts.NewMessages != 100 || receipt.Revision != 1 {
		t.Fatal("maximum receipt")
	}
	assertImportSnapshot(t, f)
	// Retained history can grow through another bounded batch. Reassigning its
	// extra message to an earlier 100-message batch is invalid provenance even
	// when the destination native graph and all source hashes remain genuine.
	next := messageBatch(f.worker, recipient, 1)
	next.Messages[0].SourceKey = "additional-root"
	next.Jobs = preparedBatch(f.worker, 1).Jobs
	added := checked[coordimport.Receipt](t, f.apply(next, f.preview(next), "additional"), 200)
	if added.Revision != 2 {
		t.Fatal("additional bounded history did not advance")
	}
	assertImportSnapshot(t, f)
	tx, err := f.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE coordination_import_messages SET import_revision=1,created_at=(SELECT created_at FROM coordination_import_batches WHERE project_id=? AND revision=1) WHERE project_id=? AND source_key='additional-root'`, f.project, f.project); err != nil {
		t.Fatal(err)
	}
	if err = coordimport.ValidateSnapshot(context.Background(), tx); err == nil {
		t.Fatal("oversized retained batch admitted")
	}
}
func TestPreparedMessagesMappedRecipientMissingRefusesPreview(t *testing.T) {
	f := newImportFixture(t)
	recipient := f.workerFor(f.project, "Recipient")
	b := messageBatch(f.worker, recipient, 0)
	receipt := checked[coordimport.Receipt](t, f.apply(b, f.preview(b), "root"), 200)
	if _, err := f.store.DB().Exec(`DELETE FROM inbox_recipient_state WHERE message_id=?`, receipt.MessageMappings[0].MessageID); err != nil {
		t.Fatal(err)
	}
	b.ExpectedRevision = &receipt.Revision
	before := f.snapshot(false)
	errorIs(t, f.call("POST", f.base()+"/preview", f.admin, f.worker, "", b), 409, "coordination_import_conflict")
	conserved(t, before, f.snapshot(false))
}
