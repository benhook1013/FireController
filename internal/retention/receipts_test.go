package retention

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestReceiptRetentionCutoffAndDryRun(t *testing.T) {
	ctx := t.Context()
	store, err := core.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cutoff := time.Date(2026, 1, 20, 12, 34, 56, 0, time.UTC)
	// A zero-precision boundary and its immediate nanosecond neighbors make a
	// lexical comparison using variable-width RFC3339 formatting fail this test.
	_, err = store.DB().ExecContext(ctx, `
		WITH RECURSIVE numbers(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM numbers WHERE n<503)
		INSERT INTO receipts(instance_id,principal_id,project_id,operation,target,key,fingerprint,response_json,created_at)
		SELECT ?, 'p', '', 'test', 'resource', 'key-' || n, 'fingerprint', '{}',
			CASE WHEN n<=501 THEN ? WHEN n=502 THEN ? ELSE ? END FROM numbers
	`, store.InstanceID(), core.ReceiptTimestamp(cutoff.Add(-time.Nanosecond)),
		core.ReceiptTimestamp(cutoff), core.ReceiptTimestamp(cutoff.Add(time.Nanosecond)))
	if err != nil {
		t.Fatal(err)
	}
	run := func(dry bool, deleted, rows int64) {
		t.Helper()
		p := &pass{store: store, dryRun: dry, deleted: map[string]int64{}}
		if err := pruneReceipts(ctx, p, cutoff); err != nil {
			t.Fatal(err)
		}
		var retained int64
		if err := store.DB().QueryRowContext(ctx, `SELECT count(*) FROM receipts`).Scan(&retained); err != nil {
			t.Fatal(err)
		}
		if p.deleted["receipts"] != deleted || retained != rows {
			t.Fatalf("dry-run=%v: deleted=%d (want %d), retained=%d (want %d)", dry, p.deleted["receipts"], deleted, retained, rows)
		}
	}
	run(true, 501, 503)
	run(false, 501, 2)
	run(false, 0, 2)
}

func TestReceiptRetentionReopensExpiredMutationKey(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "data")
	store, err := core.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token, err := os.ReadFile(filepath.Join(dir, "bootstrap-admin.token"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(ctx, "POST", "/receipt-probe", nil)
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("X-FC-Instance", store.InstanceID())
	scope, err := store.AdminScope(request)
	if err != nil {
		t.Fatal(err)
	}
	m := core.Mutation{Operation: "retention.probe", Target: "resource", Key: "unchanged-key"}
	calls := 0
	apply := func(context.Context, *sql.Tx) (any, error) {
		calls++
		return map[string]int{"call": calls}, nil
	}
	if _, err := store.Mutate(ctx, scope, m, apply); err != nil {
		t.Fatal(err)
	}
	if replay, err := store.Mutate(ctx, scope, m, apply); err != nil || string(replay) != `{"call":1}` || calls != 1 {
		t.Fatalf("retained exact replay applied twice: calls=%d response=%s err=%v", calls, replay, err)
	}
	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	if _, err := store.DB().ExecContext(ctx, `UPDATE receipts SET created_at=? WHERE key=?`,
		core.ReceiptTimestamp(cutoff.Add(-time.Nanosecond)), m.Key); err != nil {
		t.Fatal(err)
	}
	p := &pass{store: store, deleted: map[string]int64{}}
	if err := pruneReceipts(ctx, p, cutoff); err != nil || p.deleted["receipts"] != 1 {
		t.Fatalf("expiry did not remove the exact receipt: %v, count=%d", err, p.deleted["receipts"])
	}
	if fresh, err := store.Mutate(ctx, scope, m, apply); err != nil || string(fresh) != `{"call":2}` || calls != 2 {
		t.Fatalf("expired key was not a new authorized request: calls=%d response=%s err=%v", calls, fresh, err)
	}
}
