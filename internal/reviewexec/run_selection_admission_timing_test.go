package reviewexec

import (
	"bytes"
	"encoding/json"
	"log/slog"
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
			var out bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
			defer slog.SetDefault(old)
			r := Run{ID: "selection", ProjectID: "project", Channel: "cli", CreatedAt: tc.created, UpdatedAt: tc.queued}
			logSlowRunSelection(t.Context(), r, runChoice{}, 2, now, tc.phases, 0)
			if (out.Len() > 0) != tc.emitted {
				t.Fatalf("unexpected event: %s", out.String())
			}
			if !tc.emitted {
				return
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
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
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	selectionNext(t, f, &refreshTestReader{})
	gotRun := selectionRead(t, f, receipt.ID)
	if gotRun.OperationID == "" {
		t.Fatal("expected ordinary child", gotRun)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err, out.String())
	}
	if got["admission_timing_known"] != true || got["queued_to_claim_ms"].(float64) < 20000 || got["accepted_to_claim_ms"].(float64) < 60000 {
		t.Fatal(got)
	}
}
