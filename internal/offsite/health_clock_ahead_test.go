package offsite

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOffsiteHealthDistinguishesClockAheadFromCorruptState(t *testing.T) {
	f := newFixture(t)
	config := filepath.Join(t.TempDir(), "config.json")
	if err := writeJSON(config, f.c); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.c.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	outputDir := t.TempDir()
	if err := os.Chmod(outputDir, 0700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(outputDir, "health.json")
	ledger := filepath.Join(f.c.StateDir, "offsite.json")
	now := time.Now().UTC()
	past := now.Add(-time.Minute).Format(time.RFC3339Nano)
	future := now.Add(time.Hour).Format(time.RFC3339Nano)
	makeState := func() Status {
		r := Receipt{Version: 1, ID: "backup_" + strings.Repeat("a", 32), InstanceID: f.c.InstanceID, SnapshotFinishedAt: past, ArchiveSHA256: strings.Repeat("b", 64), ArchiveBytes: 1024}
		return Status{Version: 1, Binding: binding(f.c), Owned: []Receipt{r}, LastSuccess: &r, LastAttempt: &Attempt{State: "complete", StartedAt: past, FinishedAt: past}}
	}
	if err := writeJSON(ledger, makeState()); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStatusAndWriteHealth(t.Context(), config, output, "Cloud", 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, code string
		change     func(*Status)
	}{
		{"future-snapshot", "offsite_health_clock_ahead", func(s *Status) {
			s.LastSuccess.SnapshotFinishedAt = future
			s.Owned[0] = *s.LastSuccess
		}},
		{"future-completed-attempt", "offsite_health_clock_ahead", func(s *Status) { s.LastAttempt.FinishedAt = future }},
		{"future-running-attempt", "offsite_health_clock_ahead", func(s *Status) {
			s.LastAttempt = &Attempt{State: "running", StartedAt: future}
		}},
		{"malformed-snapshot", "offsite_state_invalid", func(s *Status) {
			s.LastSuccess.SnapshotFinishedAt = "not a timestamp"
			s.Owned[0] = *s.LastSuccess
		}},
		{"malformed-attempt", "offsite_state_invalid", func(s *Status) { s.LastAttempt.FinishedAt = "not a timestamp" }},
		{"future-snapshot-with-malformed-attempt", "offsite_state_invalid", func(s *Status) {
			s.LastSuccess.SnapshotFinishedAt = future
			s.Owned[0] = *s.LastSuccess
			s.LastAttempt.FinishedAt = "not a timestamp"
		}},
		{"future-snapshot-with-noncanonical-attempt", "offsite_state_invalid", func(s *Status) {
			s.LastSuccess.SnapshotFinishedAt = future
			s.Owned[0] = *s.LastSuccess
			s.LastAttempt.FinishedAt = now.Format("2006-01-02T15:04:05.999999999-07:00")
		}},
		{"future-attempt-with-invalid-state", "offsite_state_invalid", func(s *Status) {
			s.LastAttempt.State = "invalid"
			s.LastAttempt.FinishedAt = future
		}},
		{"future-attempt-with-wrong-ledger-binding", "offsite_state_invalid", func(s *Status) {
			s.Binding = strings.Repeat("c", 64)
			s.LastAttempt.FinishedAt = future
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := makeState()
			tc.change(&s)
			if err := writeJSON(ledger, s); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ReadStatusAndWriteHealth(t.Context(), config, output, "Cloud", 3*time.Hour)
			wantCode(t, err, tc.code)
			if tc.code == "offsite_health_clock_ahead" {
				if _, err := ReadStatus(t.Context(), config); err != nil {
					t.Fatal("valid retained ledger became unreadable after clock rollback", err)
				}
			}
			after, err := os.ReadFile(ledger)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("health diagnosis changed retained ledger evidence", err)
			}
			current, err := os.ReadFile(output)
			if err != nil || !bytes.Equal(previous, current) {
				t.Fatal("refused health projection replaced the prior report", err)
			}
		})
	}
}
