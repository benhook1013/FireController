package offsite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/backuphealth"
)

// ReadStatusAndWriteHealth projects the canonical local transfer ledger into a
// separate, redacted report. No provider, rclone credential or source bundle is
// read. A failed read leaves the previous report untouched so it becomes stale.
func ReadStatusAndWriteHealth(ctx context.Context, configPath, output, label string, maxAge time.Duration) (Report, error) {
	if ctx == nil || ctx.Err() != nil {
		return Report{}, failure("offsite_cancelled")
	}
	if backuphealth.ValidateFiles([]string{output}) != nil || !backuphealth.ValidLabel(label) || maxAge < time.Minute || maxAge > 30*24*time.Hour || maxAge%time.Second != 0 {
		return Report{}, failure("offsite_configuration_invalid")
	}
	c, err := loadConfig(configPath)
	if err != nil {
		return Report{}, err
	}
	// The report is a separate operator output, never an input/configuration,
	// transfer ledger, spool or source backup member, including directory aliases.
	resolved := resolvePrefix(output)
	for _, input := range []string{configPath, c.RcloneConfig, filepath.Join(c.StateDir, "offsite.json")} {
		if resolved == resolvePrefix(input) {
			return Report{}, failure("offsite_health_unavailable")
		}
		left, a := os.Stat(output)
		right, b := os.Stat(input)
		if a == nil && b == nil && os.SameFile(left, right) {
			return Report{}, failure("offsite_health_unavailable")
		}
	}
	for _, root := range []string{c.StateDir, c.SourceRoot} {
		base := resolvePrefix(root)
		if resolved == base || strings.HasPrefix(resolved, base+string(filepath.Separator)) {
			return Report{}, failure("offsite_health_unavailable")
		}
	}
	s, err := readState(c)
	if err != nil {
		return Report{}, err
	}
	timeout, _ := time.ParseDuration(c.Timeout) // loadConfig validated the bound.
	// Round a fractional second upward so reporting never shortens the deadline.
	timeoutSeconds := int64((timeout + time.Second - 1) / time.Second)
	m := backuphealth.Mirror{Version: 1, InstanceID: c.InstanceID, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Label: label, MaxAgeSeconds: int64(maxAge / time.Second), AttemptTimeoutSeconds: timeoutSeconds, RetentionWarning: s.RetentionReason != ""}
	if s.LastSuccess != nil {
		m.LastSnapshotAt = s.LastSuccess.SnapshotFinishedAt
	}
	if s.LastAttempt != nil {
		attempt := s.LastAttempt
		m.AttemptState = attempt.State
		m.LastAttemptAt = attempt.FinishedAt
		if attempt.State == "running" {
			if attempt.FinishedAt != "" || attempt.Reason != "" {
				return Report{}, failure("offsite_state_invalid")
			}
			m.LastAttemptAt = attempt.StartedAt
		} else {
			started, e := time.Parse(time.RFC3339Nano, attempt.StartedAt)
			finished, f := time.Parse(time.RFC3339Nano, attempt.FinishedAt)
			// These are independent wall-clock stamps; a clock correction can
			// legitimately put completion before start without changing outcome.
			if e != nil || f != nil || started.IsZero() || finished.IsZero() || attempt.StartedAt != started.UTC().Format(time.RFC3339Nano) {
				return Report{}, failure("offsite_state_invalid")
			}
			if attempt.State == "failed" || attempt.State == "interrupted" {
				m.AttemptReason = "last_attempt_failed"
			} else if attempt.Reason != "" {
				return Report{}, failure("offsite_state_invalid")
			}
		}
	}
	// Receipts retain snapshot time, not an independent remote verification
	// timestamp. In particular, an unchanged run does no remote verification.
	// Validate retained timestamps and shape independently of a backward wall
	// clock step. This copy is only for diagnosis; a future-dated report is never
	// published and the original ledger and previous report remain untouched.
	observed, _ := time.Parse(time.RFC3339Nano, m.ObservedAt)
	latest := observed
	for _, value := range []string{m.LastSnapshotAt, m.LastAttemptAt} {
		if value == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, value)
		if err != nil || at.IsZero() || value != at.UTC().Format(time.RFC3339Nano) {
			return Report{}, failure("offsite_state_invalid")
		}
		if at.After(latest) {
			latest = at
		}
	}
	validation := m
	validation.ObservedAt = latest.UTC().Format(time.RFC3339Nano)
	if !validation.Valid() {
		return Report{}, failure("offsite_state_invalid")
	}
	if latest.After(observed) {
		return Report{}, failure("offsite_health_clock_ahead")
	}
	if ctx.Err() != nil {
		return Report{}, failure("offsite_cancelled")
	}
	if err := backuphealth.WriteMirror(output, m); err != nil {
		return Report{}, failure("offsite_health_unavailable")
	}
	return Report{Version: 1, LastAttempt: s.LastAttempt, LastSuccess: s.LastSuccess, Pending: s.Pending, Retained: len(s.Owned), RetentionReason: s.RetentionReason}, nil
}
