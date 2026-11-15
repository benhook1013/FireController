package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestRetentionDiagnosticsCancelledAdmissionIsBounded(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	m := &Manager{}
	if err := m.retain(ctx); err == nil {
		t.Fatal("cancelled admission succeeded")
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["phase"] != "admission" || got["failed"] != true || got["cancelled"] != true || got["deadline_exceeded"] != false || got["bundles_deleted"] != float64(0) {
		t.Fatal(got)
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "phase": true, "duration_ms": true, "keepers_verified": true, "candidates_verified": true, "bundles_deleted": true, "failed": true, "deadline_exceeded": true, "cancelled": true}
	for key := range got {
		if !allowed[key] {
			t.Fatal("unbounded diagnostic key", key)
		}
	}
}
