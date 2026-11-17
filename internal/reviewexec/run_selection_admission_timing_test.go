package reviewexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestSelectionAdmissionTiming(t *testing.T) {
	now := time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }
	cases := []struct {
		name, created, queued string
		phases                time.Duration
		emitted, known        bool
		queueMS, acceptedMS   int64
	}{
		{"fast", stamp(time.Second), stamp(time.Second), time.Second, false, true, 1000, 1000},
		{"queue boundary", stamp(20 * time.Second), stamp(10 * time.Second), time.Second, true, true, 10000, 20000},
		{"requeued old acceptance", stamp(time.Hour), stamp(time.Second), time.Second, false, true, 1000, 3600000},
		{"retry preserves acceptance", stamp(time.Hour), stamp(11 * time.Second), time.Second, true, true, 11000, 3600000},
		{"unknown creation", "invalid", stamp(time.Hour), 11 * time.Second, true, false, 0, 0},
		{"unknown queue", stamp(time.Hour), "invalid", 11 * time.Second, true, false, 0, 0},
		{"future queue", stamp(time.Hour), stamp(-time.Second), 11 * time.Second, true, false, 0, 0},
		{"inverted", stamp(time.Second), stamp(time.Hour), 11 * time.Second, true, false, 0, 0},
		{"invalid fast", "invalid", "invalid", time.Second, false, false, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out selectionTimingBuffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
			defer slog.SetDefault(old)
			r := Run{ID: "selection", ProjectID: "project", Channel: "cli", CreatedAt: tc.created, UpdatedAt: tc.queued}
			logSlowRunSelection(t.Context(), r, runChoice{}, 2, now, tc.phases, 0)
			got, err := selectionTimingLog(out.snapshot(), "selection", tc.emitted)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.emitted {
				return
			}
			if got["admission_timing_known"] != tc.known {
				t.Fatal(got)
			}
			q, qok := got["queued_to_claim_ms"]
			a, aok := got["accepted_to_claim_ms"]
			if qok != tc.known || aok != tc.known {
				t.Fatal(got)
			}
			if tc.known && (q != float64(tc.queueMS) || a != float64(tc.acceptedMS)) {
				t.Fatal(got)
			}
			if got["duration_ms"] != float64(tc.phases.Milliseconds()) {
				t.Fatal("phase duration changed", got)
			}
		})
	}
}

func TestSelectionAdmissionTimingUsesPreclaimQueuedTimestamp(t *testing.T) {
	f := selectionFixture(t)
	receipt, _ := selectionEnqueue(t, f, "admission-log", "cli", nil)
	now := time.Now().UTC()
	if _, err := f.store.DB().Exec(`UPDATE review_selections SET created_at=?,updated_at=? WHERE id=?`, now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(-20*time.Second).Format(time.RFC3339Nano), receipt.ID); err != nil {
		t.Fatal(err)
	}
	var out selectionTimingBuffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	slog.Info(`{"event":"adoption_read_cost","reads":1}`)
	selectionNext(t, f, &refreshTestReader{})
	gotRun := selectionRead(t, f, receipt.ID)
	if gotRun.OperationID == "" {
		t.Fatal("expected ordinary child", gotRun)
	}
	got, err := selectionTimingLog(out.snapshot(), receipt.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got["admission_timing_known"] != true || got["queued_to_claim_ms"].(float64) < 20000 || got["accepted_to_claim_ms"].(float64) < 60000 {
		t.Fatal(got)
	}
}

// Capture remains safe if unrelated background telemetry writes while the test
// takes its snapshot. Every complete JSONL record is validated; only the exact
// selection event supplies assertions, never merely the first or last record.
type selectionTimingBuffer struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *selectionTimingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}
func (b *selectionTimingBuffer) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.data.Bytes())
}

func selectionTimingLog(data []byte, id string, required bool) (map[string]any, error) {
	var found map[string]any
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil || row == nil {
			return nil, fmt.Errorf("invalid log record %d", i+1)
		}
		if row["msg"] != "Review selection was slow" || row["selection_id"] != id {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("duplicate timing record for %s", id)
		}
		found = row
	}
	if (found != nil) != required {
		return nil, fmt.Errorf("timing record presence for %s: got %t want %t", id, found != nil, required)
	}
	return found, nil
}

func TestSelectionTimingLogJSONL(t *testing.T) {
	target := `{"msg":"Review selection was slow","selection_id":"wanted","queued_to_claim_ms":20000}`
	other := `{"msg":"{\"event\":\"adoption_read_cost\"}"}`
	foreign := `{"msg":"Review selection was slow","selection_id":"different","queued_to_claim_ms":1}`
	cases := []struct {
		name, data        string
		required, wantErr bool
	}{
		{"unrelated before and after", other + "\n" + foreign + "\n" + target + "\n" + other, true, false},
		{"missing target", other + "\n" + foreign, true, true},
		{"duplicate target", target + "\n" + target, true, true},
		{"malformed before", "{broken\n" + target, true, true},
		{"malformed after", target + "\n{broken", true, true},
		{"null record", "null\n" + target, true, true},
		{"expected absence", other + "\n" + foreign, false, false},
		{"unexpected event", target, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectionTimingLog([]byte(tc.data), "wanted", tc.required)
			if (err != nil) != tc.wantErr {
				t.Fatal(got, err)
			}
			if err == nil && tc.required && got["queued_to_claim_ms"] != float64(20000) {
				t.Fatal(got)
			}
		})
	}
}
