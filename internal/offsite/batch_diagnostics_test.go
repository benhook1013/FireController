package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestBatchDiagnosticsBoundedAndPrivate(t *testing.T) {
	_, b, cs, ts := batchFixture(t)
	ts[1].(*memoryTransport).corruptDownload = true
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	got, err := pushBatch(t.Context(), b, cs, ts)
	if err == nil || len(got.Destinations) != 3 || got.Destinations[0].Reason != "" || got.Destinations[1].Reason == "" || got.Destinations[2].Reason != "" {
		t.Fatal("diagnostics changed independent outcomes", got, err)
	}
	phases := map[string]int{}
	totals := 0
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) != nil {
			t.Fatal(line)
		}
		if row["msg"] == "Offsite batch phase outcome" {
			phases[row["phase"].(string)]++
			if row["duration_ms"].(float64) < 0 {
				t.Fatal(row)
			}
		}
		if row["msg"] == "Offsite batch transport totals" {
			totals++
			if row["calls"].(float64) < 0 || row["failures"].(float64) < 0 {
				t.Fatal(row)
			}
		}
	}
	if phases["destination"] != 3 || phases["source_inspection"] != 1 || phases["archive_pack"] != 1 || phases["archive_validation"] != 1 || totals != 12 {
		t.Fatal(phases, totals)
	}
	for _, c := range cs {
		for _, secret := range []string{c.SourceRoot, c.StateDir, c.Remote, c.RcloneConfig, c.InstanceID} {
			if secret != "" && strings.Contains(logs.String(), secret) {
				t.Fatal("private selector in diagnostic")
			}
		}
	}
}

func TestBatchTimingPreservesTransportErrorAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tr := &batchTimedTransport{transport: memory()}
	_, err := tr.read(ctx, "PRIVATE_REMOTE_MISSING_RECEIPT")
	if err != notFound || tr.timings[0].calls != 1 || tr.timings[0].failures != 1 {
		t.Fatal(err, tr.timings)
	}
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)
	finish := beginBatchPhase(ctx, 2, "destination")
	finish(err)
	tr.report(ctx, 2)
	if strings.Contains(logs.String(), "PRIVATE_REMOTE") || !strings.Contains(logs.String(), `"cancelled":true`) {
		t.Fatal(logs.String())
	}
}
