// Package backuphealth contains redacted backup reporting contracts. It never
// opens a database, discovers backup files, or contacts a storage provider.
package backuphealth

import (
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const MaxFiles = 8
const MaxMirrorBytes = 8 << 10
const ReporterMaxAge = 5 * time.Minute

var instancePattern = regexp.MustCompile(`^instance_[0-9a-f]{32}$`)
var reasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,79}$`)

type Row struct {
	Label         string `json:"label,omitempty"`
	State         string `json:"state"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type Snapshot struct {
	Version   int    `json:"version"`
	CheckedAt string `json:"checked_at"`
	Local     Row    `json:"local"`
	Offsite   []Row  `json:"offsite"`
}

// Mirror is an operator-authored projection of retained transfer facts. ObservedAt
// means the local ledger was read; it never means remote bytes were reverified.
type Mirror struct {
	Version          int    `json:"version"`
	InstanceID       string `json:"instance_id"`
	ObservedAt       string `json:"observed_at"`
	Label            string `json:"label"`
	MaxAgeSeconds    int64  `json:"max_age_seconds"`
	LastSnapshotAt   string `json:"last_snapshot_at,omitempty"`
	LastAttemptAt    string `json:"last_attempt_at,omitempty"`
	AttemptState     string `json:"attempt_state,omitempty"`
	AttemptReason    string `json:"attempt_reason,omitempty"`
	RetentionWarning bool   `json:"retention_warning,omitempty"`
}

func ValidLabel(label string) bool {
	return label != "" && strings.TrimSpace(label) == label && utf8.ValidString(label) && len(label) <= 80 && strings.IndexFunc(label, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

func timestamp(value string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, value)
	return t, err == nil && !t.IsZero() && value == t.UTC().Format(time.RFC3339Nano)
}

func (m Mirror) Valid() bool {
	observed, ok := timestamp(m.ObservedAt)
	if m.Version != 1 || !instancePattern.MatchString(m.InstanceID) || !ok || !ValidLabel(m.Label) || m.MaxAgeSeconds < 60 || m.MaxAgeSeconds > int64((30*24*time.Hour)/time.Second) {
		return false
	}
	for _, value := range []string{m.LastSnapshotAt, m.LastAttemptAt} {
		if value != "" {
			t, valid := timestamp(value)
			if !valid || t.After(observed) {
				return false
			}
		}
	}

	switch m.AttemptState {
	case "":
		if m.LastAttemptAt != "" || m.AttemptReason != "" {
			return false
		}
	case "running", "complete", "unchanged", "failed", "interrupted":
		if m.LastAttemptAt == "" {
			return false
		}
	default:
		return false
	}
	if m.AttemptReason != "" && (!reasonPattern.MatchString(m.AttemptReason) || m.AttemptState != "failed" && m.AttemptState != "interrupted") {
		return false
	}
	return true
}

func Unknown() Row {
	return Row{Label: "Offsite backup", State: "unknown", Reason: "report_unavailable"}
}

func Offsite(m Mirror, instance string, now time.Time) Row {
	if !m.Valid() || m.InstanceID != instance {
		return Unknown()
	}
	observed, _ := timestamp(m.ObservedAt)
	if observed.After(now) {
		return Unknown()
	}
	r := Row{Label: m.Label, State: "unknown", LastSuccessAt: m.LastSnapshotAt, LastAttemptAt: m.LastAttemptAt}
	if now.Sub(observed) > ReporterMaxAge {
		r.Reason = "report_stale"
		return r
	}
	if m.AttemptState == "failed" || m.AttemptState == "interrupted" {
		r.State, r.Reason = "failed", "last_attempt_failed"
		return r
	}
	if m.RetentionWarning {
		r.State, r.Reason = "failed", "retention_failed"
		return r
	}
	if m.AttemptState == "running" {
		attempt, _ := timestamp(m.LastAttemptAt)
		if now.Sub(attempt) <= ReporterMaxAge {
			r.State = "running"
		} else {
			r.Reason = "attempt_stale"
		}
		return r
	}
	if m.LastSnapshotAt == "" {
		r.Reason = "no_completed_backup"
		return r
	}
	snapshot, _ := timestamp(m.LastSnapshotAt)
	if now.Sub(snapshot) > time.Duration(m.MaxAgeSeconds)*time.Second {
		r.State = "overdue"
	} else {
		r.State = "healthy"
	}
	return r
}

// LocalRecord is the redacted portion of a managed singleton needed to derive
// health. Private paths and operator configurations do not enter this package.
type LocalRecord struct {
	Configured       bool
	State            string
	LastSnapshotAt   string
	SuccessInstance  string
	AttemptState     string
	LastAttemptAt    string
	AttemptInstance  string
	RetentionWarning bool
	Interval         time.Duration
	Timeout          time.Duration
}

func Local(record LocalRecord, instance string, now time.Time) Row {
	r := Row{State: "unknown"}
	if record.LastSnapshotAt != "" && record.SuccessInstance == instance {
		snapshot, valid := timestamp(record.LastSnapshotAt)
		if !valid || snapshot.After(now) {
			r.Reason = "invalid_backup_time"
			return r
		}
		r.LastSuccessAt = record.LastSnapshotAt
	}
	if !record.Configured {
		r.State = "unconfigured"
		return r
	}
	if record.LastAttemptAt != "" && record.AttemptInstance == instance {
		attempt, valid := timestamp(record.LastAttemptAt)
		if !valid || attempt.After(now) {
			r.Reason = "invalid_attempt_time"
			return r
		}
		r.LastAttemptAt = record.LastAttemptAt
	}
	if record.State == "held" {
		r.State, r.Reason = "failed", "backup_held"
		return r
	}
	if record.AttemptInstance == instance && (record.AttemptState == "failed" || record.AttemptState == "interrupted") {
		r.State, r.Reason = "failed", "last_attempt_failed"
		return r
	}
	if record.RetentionWarning {
		r.State, r.Reason = "failed", "retention_failed"
		return r
	}
	if record.State == "running" {
		attempt, valid := timestamp(r.LastAttemptAt)
		if valid && record.Timeout > 0 && now.Sub(attempt) <= record.Timeout+ReporterMaxAge {
			r.State = "running"
		} else {
			r.Reason = "attempt_stale"
		}
		return r
	}
	if r.LastSuccessAt == "" {
		r.Reason = "no_completed_backup"
		return r
	}
	if record.Interval <= 0 || record.Timeout <= 0 {
		r.Reason = "configuration_unavailable"
		return r
	}
	snapshot, _ := timestamp(r.LastSuccessAt)
	if now.Sub(snapshot) > record.Interval+record.Timeout+ReporterMaxAge {
		r.State = "overdue"
	} else {
		r.State = "healthy"
	}
	return r
}
