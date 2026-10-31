package inbox_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/inbox"
)

func preparedMessage(f *fixture) inbox.PreparedMessage {
	return inbox.PreparedMessage{SourceKey: " original source 雪 ", SenderWorkerID: f.sender, RecipientWorkerID: f.recipient, Body: "  Original <literal> body 雪\n\twith spaces  ", CreatedAt: "2024-02-03T04:05:06.123456789+13:00"}
}
func preparedMessageTx(t *testing.T, f *fixture, fn func(*sql.Tx) error) error {
	t.Helper()
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func preparedMessageCounts(t *testing.T, f *fixture) string {
	t.Helper()
	var n []int
	for _, table := range []string{"inbox_conversations", "inbox_messages", "inbox_recipient_state", "events", "receipts"} {
		var count int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		n = append(n, count)
	}
	return fmt.Sprint(n)
}
func preparedMessageValidate(t *testing.T, f *fixture, id, conversation, reply string, item inbox.PreparedMessage) {
	t.Helper()
	if err := inbox.ValidatePreparedMessageSnapshot(t.Context(), f.store.DB(), f.a, id, conversation, reply, item); err != nil {
		t.Fatal("prepared snapshot invalid", err)
	}
}

func TestPreparedMessageNormalizationPreservesTextAndTimes(t *testing.T) {
	original := inbox.PreparedMessage{SourceKey: " literal source 雪 ", ReplyToSourceKey: " parent ", SenderWorkerID: "sender", RecipientWorkerID: "recipient", Body: "  Body 雪 <literal>\n\t  ", CreatedAt: "2024-02-03T04:05:06.123456789+13:00", SeenAt: "2024-02-02T15:05:06.123456789Z", AcknowledgedAt: "2024-02-03T04:05:06.123456789+13:00"}
	// Exact equal instants with distinct offsets are legitimate original facts.
	normalized, err := inbox.NormalizePreparedMessage(original)
	if err != nil || normalized != original {
		t.Fatal("rewrote historical bytes", err)
	}
	original.SourceKey = strings.Repeat("k", 200)
	original.ReplyToSourceKey = ""
	original.Body = strings.Repeat("雪", 5461) + "a"
	if len(original.Body) != inbox.MaxBodyBytes {
		t.Fatal("boundary fixture incorrect")
	}
	if _, err = inbox.NormalizePreparedMessage(original); err != nil {
		t.Fatal("exact byte limit refused", err)
	}
	encoded, err := json.Marshal(normalized)
	if err != nil || !strings.Contains(string(encoded), `"reply_to_source_key":" parent "`) {
		t.Fatal("source wire shape changed")
	}
	for name, change := range map[string]func(*inbox.PreparedMessage){
		"blank-key": func(i *inbox.PreparedMessage) { i.SourceKey = "\t " }, "key-bytes": func(i *inbox.PreparedMessage) { i.SourceKey = strings.Repeat("雪", 67) }, "key-unicode": func(i *inbox.PreparedMessage) { i.SourceKey = string([]byte{0xff}) }, "self-reply": func(i *inbox.PreparedMessage) { i.ReplyToSourceKey = i.SourceKey }, "reply-key": func(i *inbox.PreparedMessage) { i.ReplyToSourceKey = "  " }, "missing-sender": func(i *inbox.PreparedMessage) { i.SenderWorkerID = "" }, "invalid-recipient": func(i *inbox.PreparedMessage) { i.RecipientWorkerID = " padded " }, "body-bytes": func(i *inbox.PreparedMessage) { i.Body += "b" }, "body-unicode": func(i *inbox.PreparedMessage) { i.Body = string([]byte{0xff}) }, "blank-body": func(i *inbox.PreparedMessage) { i.Body = " \t\n" }, "missing-time": func(i *inbox.PreparedMessage) { i.CreatedAt = "" }, "bad-time": func(i *inbox.PreparedMessage) { i.CreatedAt = "yesterday" }, "seen-before-created": func(i *inbox.PreparedMessage) { i.SeenAt = "2024-02-02T15:05:05Z" }, "ack-without-seen": func(i *inbox.PreparedMessage) { i.SeenAt = ""; i.AcknowledgedAt = i.CreatedAt }, "ack-before-seen": func(i *inbox.PreparedMessage) {
			i.SeenAt = "2024-02-02T15:05:08Z"
			i.AcknowledgedAt = "2024-02-02T15:05:07Z"
		},
	} {
		t.Run(name, func(t *testing.T) {
			item := original
			change(&item)
			if _, err := inbox.NormalizePreparedMessage(item); err == nil {
				t.Fatal("invalid prepared message admitted")
			}
		})
	}
}
func TestPreparedMessageNativeReadsAndMonotoneReceipts(t *testing.T) {
	f := setup(t)
	root := preparedMessage(f)
	root.Body = strings.Repeat("雪", 180) + " <original>"
	reply := root
	reply.SourceKey = "reply-source"
	reply.ReplyToSourceKey = root.SourceKey
	reply.SenderWorkerID, reply.RecipientWorkerID = f.recipient, f.sender
	reply.CreatedAt = "2024-02-02T15:06:07.999999999Z"
	reply.SeenAt = "2024-02-03T04:07:08.999999999+13:00"
	acknowledged := root
	acknowledged.SourceKey = "handled-source"
	acknowledged.SeenAt = acknowledged.CreatedAt
	acknowledged.AcknowledgedAt = acknowledged.CreatedAt
	var eventsBefore, receiptsBefore int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&eventsBefore); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&receiptsBefore); err != nil {
		t.Fatal(err)
	}
	err := preparedMessageTx(t, f, func(tx *sql.Tx) error {
		if err := inbox.InsertPreparedMessage(t.Context(), tx, f.a, "imported-root", "imported-conversation", "", root); err != nil {
			return err
		}
		if err := inbox.InsertPreparedMessage(t.Context(), tx, f.a, "imported-reply", "imported-conversation", "imported-root", reply); err != nil {
			return err
		}
		return inbox.InsertPreparedMessage(t.Context(), tx, f.a, "imported-handled", "handled-conversation", "", acknowledged)
	})
	if err != nil {
		t.Fatal(err)
	}
	var eventsAfter, receiptsAfter int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&eventsAfter); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM receipts`).Scan(&receiptsAfter); err != nil {
		t.Fatal(err)
	}
	if eventsAfter != eventsBefore || receiptsAfter != receiptsBefore {
		t.Fatal("historical import replayed send/handling or receipt")
	}
	var unread page
	decode(t, f.request("GET", f.path(f.a, "inbox/"+f.recipient)+"?unread=true", f.clientA, f.recipient, "", nil), 200, &unread)
	if len(unread.Items) != 1 || unread.Items[0].ID != "imported-root" || !unread.Items[0].PreviewTruncated || unread.Items[0].Seen || unread.Items[0].Acknowledged {
		t.Fatal("pending import not discoverable through native inbox")
	}
	var conversation struct {
		Root  string `json:"root_message_id"`
		Items []struct {
			ID, Body string
			Created  string `json:"created_at"`
			Parent   string `json:"reply_to_message_id"`
		} `json:"items"`
	}
	decode(t, f.request("GET", f.path(f.a, "conversations/imported-conversation"), f.clientA, f.recipient, "", nil), 200, &conversation)
	if conversation.Root != "imported-root" || len(conversation.Items) != 2 || conversation.Items[0].Body != root.Body || conversation.Items[0].Created != root.CreatedAt || conversation.Items[1].Parent != "imported-root" || conversation.Items[1].Created != reply.CreatedAt {
		t.Fatal("native read lost literal body/time/graph")
	}
	preparedMessageValidate(t, f, "imported-root", "imported-conversation", "", root)
	preparedMessageValidate(t, f, "imported-reply", "imported-conversation", "imported-root", reply)
	preparedMessageValidate(t, f, "imported-handled", "handled-conversation", "", acknowledged)
	f.transition(f.a, f.clientA, f.recipient, "imported-root", "seen", "")
	preparedMessageValidate(t, f, "imported-root", "imported-conversation", "", root)
	f.transition(f.a, f.clientA, f.recipient, "imported-root", "ack", "")
	preparedMessageValidate(t, f, "imported-root", "imported-conversation", "", root)
	f.transition(f.a, f.clientA, f.sender, "imported-reply", "ack", "")
	preparedMessageValidate(t, f, "imported-reply", "imported-conversation", "imported-root", reply)
	before := preparedMessageCounts(t, f)
	f.transition(f.a, f.clientA, f.recipient, "imported-handled", "ack", "")
	preparedMessageValidate(t, f, "imported-handled", "handled-conversation", "", acknowledged)
	if before == preparedMessageCounts(t, f) {
		t.Fatal("ordinary noop receipt fixture not exercised")
	}
	var revision int
	if err = f.store.DB().QueryRow(`SELECT revision FROM inbox_recipient_state WHERE message_id='imported-handled'`).Scan(&revision); err != nil || revision != 1 {
		t.Fatal("noop historical handling inflated revision", revision, err)
	}
	if f.providerCalls != 2 {
		t.Fatal("message leaf or reads called provider")
	}
}
func TestPreparedMessageRefusesParentAndMappingConflictsWithoutEffects(t *testing.T) {
	for _, mode := range []string{"missing-parent", "foreign-parent", "wrong-conversation", "third-participant", "self-parent", "unmapped-source-parent", "missing-recipient", "foreign-recipient", "duplicate-root", "cycle"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			root := preparedMessage(f)
			if err := preparedMessageTx(t, f, func(tx *sql.Tx) error {
				return inbox.InsertPreparedMessage(t.Context(), tx, f.a, "root", "conversation", "", root)
			}); err != nil {
				t.Fatal(err)
			}
			item := root
			item.SourceKey = "reply"
			item.ReplyToSourceKey = root.SourceKey
			message, conversation, parent := "reply", "conversation", "root"
			switch mode {
			case "missing-parent":
				parent = "missing"
			case "foreign-parent":
				parent = f.send(f.b, f.clientB, f.bSender, f.bRecipient, "foreign original", "", "").ID
			case "wrong-conversation":
				conversation = "different"
			case "third-participant":
				item.RecipientWorkerID = f.create(f.path(f.a, "workers"), map[string]any{"name": "Third participant"}, "")
			case "self-parent":
				parent = message
			case "unmapped-source-parent":
				item.ReplyToSourceKey = ""
			case "missing-recipient":
				item.RecipientWorkerID = "missing"
			case "foreign-recipient":
				item.RecipientWorkerID = f.bRecipient
			case "duplicate-root":
				item.ReplyToSourceKey = ""
				parent = ""
			case "cycle":
				if _, err := f.store.DB().Exec(`UPDATE inbox_messages SET reply_to_message_id=id WHERE id='root'`); err != nil {
					t.Fatal(err)
				}
			}
			before := preparedMessageCounts(t, f)
			err := preparedMessageTx(t, f, func(tx *sql.Tx) error {
				return inbox.InsertPreparedMessage(t.Context(), tx, f.a, message, conversation, parent, item)
			})
			if err == nil || preparedMessageCounts(t, f) != before {
				t.Fatal("invalid import partially changed native records", err)
			}
		})
	}
}
func TestPreparedMessageCallerRollbackAndSnapshotCorruption(t *testing.T) {
	t.Run("recipient-write-failure", func(t *testing.T) {
		f := setup(t)
		before := preparedMessageCounts(t, f)
		if _, err := f.store.DB().Exec(`CREATE TRIGGER imported_receipt_fault BEFORE INSERT ON inbox_recipient_state BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
			t.Fatal(err)
		}
		item := preparedMessage(f)
		err := preparedMessageTx(t, f, func(tx *sql.Tx) error {
			return inbox.InsertPreparedMessage(t.Context(), tx, f.a, "root", "conversation", "", item)
		})
		if err == nil || before != preparedMessageCounts(t, f) {
			t.Fatal("failure was not caller-atomic", err)
		}
	})
	for _, mode := range []string{"body", "time", "parent", "root-pointer", "wrong-state-worker", "extra-state", "seen-regression", "ack-regression", "revision", "blob-body"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			item := preparedMessage(f)
			item.SeenAt = item.CreatedAt
			item.AcknowledgedAt = item.CreatedAt
			if err := preparedMessageTx(t, f, func(tx *sql.Tx) error {
				return inbox.InsertPreparedMessage(t.Context(), tx, f.a, "root", "conversation", "", item)
			}); err != nil {
				t.Fatal(err)
			}
			var query string
			var args []any
			switch mode {
			case "body":
				query = `UPDATE inbox_messages SET body='changed' WHERE id='root'`
			case "time":
				query = `UPDATE inbox_messages SET created_at='2025-01-01T00:00:00Z' WHERE id='root'`
			case "parent":
				query = `UPDATE inbox_messages SET reply_to_message_id=id WHERE id='root'`
			case "root-pointer":
				other := item
				other.SourceKey = "other"
				if err := preparedMessageTx(t, f, func(tx *sql.Tx) error {
					return inbox.InsertPreparedMessage(t.Context(), tx, f.a, "other", "other-conversation", "", other)
				}); err != nil {
					t.Fatal(err)
				}
				query = `UPDATE inbox_conversations SET root_message_id='other' WHERE id='conversation'`
			case "wrong-state-worker":
				query = `UPDATE inbox_recipient_state SET worker_id=? WHERE message_id='root'`
				args = []any{f.sender}
			case "extra-state":
				query = `INSERT INTO inbox_recipient_state(project_id,message_id,worker_id) VALUES(?,'root',?)`
				args = []any{f.a, f.sender}
			case "seen-regression":
				query = `UPDATE inbox_recipient_state SET acknowledged=0,seen=0 WHERE message_id='root'`
			case "ack-regression":
				query = `UPDATE inbox_recipient_state SET acknowledged=0 WHERE message_id='root'`
			case "revision":
				query = `UPDATE inbox_recipient_state SET revision=2 WHERE message_id='root'`
			case "blob-body":
				query = `UPDATE inbox_messages SET body=CAST(body AS BLOB) WHERE id='root'`
			}
			if _, err := f.store.DB().Exec(query, args...); err != nil {
				t.Fatal(err)
			}
			before := preparedMessageCounts(t, f)
			if err := inbox.ValidatePreparedMessageSnapshot(t.Context(), f.store.DB(), f.a, "root", "conversation", "", item); err == nil || preparedMessageCounts(t, f) != before {
				t.Fatal("corrupt retained import accepted or repaired", err)
			}
		})
	}
}
func TestPreparedMessageCancellationAndDirectAcknowledgment(t *testing.T) {
	f := setup(t)
	item := preparedMessage(f)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := preparedMessageCounts(t, f)
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = inbox.InsertPreparedMessage(ctx, tx, f.a, "canceled", "canceled-conversation", "", item)
	tx.Rollback()
	if err == nil || before != preparedMessageCounts(t, f) {
		t.Fatal("canceled import wrote rows", err)
	}
	if err = preparedMessageTx(t, f, func(tx *sql.Tx) error {
		return inbox.InsertPreparedMessage(t.Context(), tx, f.a, "direct-ack", "direct-conversation", "", item)
	}); err != nil {
		t.Fatal(err)
	}
	f.transition(f.a, f.clientA, f.recipient, "direct-ack", "ack", "")
	preparedMessageValidate(t, f, "direct-ack", "direct-conversation", "", item)
	var revision int
	if err = f.store.DB().QueryRow(`SELECT revision FROM inbox_recipient_state WHERE message_id='direct-ack'`).Scan(&revision); err != nil || revision != 2 {
		t.Fatal("direct ack native transition not preserved", err)
	}
	normalized, err := inbox.NormalizePreparedMessage(item)
	if err != nil || !reflect.DeepEqual(normalized, item) {
		t.Fatal("caller input altered", err)
	}
	if !time.Date(2024, 2, 3, 4, 5, 6, 123456789, time.FixedZone("source", 13*60*60)).Equal(func() time.Time { v, _ := time.Parse(time.RFC3339Nano, item.CreatedAt); return v }()) {
		t.Fatal("time fixture invalid")
	}
}
