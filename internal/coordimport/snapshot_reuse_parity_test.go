package coordimport_test

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/benhook1013/FireController/internal/coordimport"
	"testing"
	"time"
)

// Hiding concrete transaction identity intentionally selects the unchanged path.
type unpreparedSnapshot struct{ coordimport.SnapshotReader }

func TestSnapshotReuseValidCorruptAndCancellationParity(t *testing.T) {
	f := newImportFixture(t)
	recipient := f.workerFor(f.project, "Recipient")
	batch := messageBatch(f.worker, recipient, 0)
	root := batch.Messages[0]
	for i := 1; i < 30; i++ {
		v := root
		v.SourceKey = fmt.Sprintf("reply-%d", i)
		v.ReplyToSourceKey = batch.Messages[i-1].SourceKey
		batch.Messages = append(batch.Messages, v)
	}
	batch.Jobs = preparedBatch(f.worker, 0).Jobs
	checked[coordimport.Receipt](t, f.apply(batch, f.preview(batch), "snapshot-reuse"), 200)
	for _, tc := range []struct {
		name, sql string
		valid     bool
	}{
		{"valid", "", true},
		{"body", "UPDATE inbox_messages SET body='corrupt' WHERE sequence=(SELECT max(sequence) FROM inbox_messages)", false},
		{"receipt", "UPDATE inbox_recipient_state SET revision=99", false},
		{"source-hash", "UPDATE coordination_import_messages SET item_sha256='invalid'", false},
		{"self-parent", "UPDATE inbox_messages SET reply_to_message_id=id WHERE sequence=(SELECT max(sequence) FROM inbox_messages)", false},
		{"job-original", "UPDATE job_history SET state_json='{}' WHERE revision=1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := f.store.DB().BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if tc.sql != "" {
				if _, err = tx.ExecContext(t.Context(), tc.sql); err != nil {
					t.Fatal(err)
				}
			}
			a := coordimport.ValidateSnapshot(t.Context(), unpreparedSnapshot{tx})
			b := coordimport.ValidateSnapshot(t.Context(), tx)
			if (a == nil) != tc.valid || (b == nil) != tc.valid || fmt.Sprint(a) != fmt.Sprint(b) {
				t.Fatalf("parity expected valid=%t: direct=%v reused=%v", tc.valid, a, b)
			}
		})
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if coordimport.ValidateSnapshot(ctx, tx) == nil || coordimport.ValidateSnapshot(ctx, unpreparedSnapshot{tx}) == nil {
		t.Fatal("cancelled snapshot accepted")
	}
}

func TestSnapshotReuseMatchedTiming(t *testing.T) {
	f := newImportFixture(t)
	recipient := f.workerFor(f.project, "Recipient")
	batch := messageBatch(f.worker, recipient, 0)
	root := batch.Messages[0]
	for i := 1; i < 100; i++ {
		v := root
		v.SourceKey = fmt.Sprintf("reply-%d", i)
		v.ReplyToSourceKey = batch.Messages[i-1].SourceKey
		batch.Messages = append(batch.Messages, v)
	}
	// Keep each native setup mutation within its unchanged two-second budget
	// under race instrumentation; measure the same final 100-message chain.
	all := batch.Messages
	for first := 0; first < len(all); first += 20 {
		batch.Messages = all[first : first+20]
		revision := int64(first / 20)
		batch.ExpectedRevision = &revision
		checked[coordimport.Receipt](t, f.apply(batch, f.preview(batch), fmt.Sprintf("snapshot-timing-%d", first)), 200)
	}
	var direct, reused time.Duration
	// Alternating order, same persisted data, fresh read-only transaction each time.
	for pass := 0; pass < 6; pass++ {
		for j := 0; j < 2; j++ {
			cached := (pass+j)%2 == 0
			tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			var reader coordimport.SnapshotReader = tx
			if !cached {
				reader = unpreparedSnapshot{tx}
			}
			start := time.Now()
			err = coordimport.ValidateSnapshot(t.Context(), reader)
			elapsed := time.Since(start)
			tx.Rollback()
			if err != nil {
				t.Fatal(err)
			}
			if cached {
				reused += elapsed
			} else {
				direct += elapsed
			}
		}
	}
	t.Logf("matched 6x100-message-chain modernc validation: direct=%s reused=%s (no performance threshold)", direct, reused)
}
