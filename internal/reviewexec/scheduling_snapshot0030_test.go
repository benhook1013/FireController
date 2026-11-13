package reviewexec

import (
	"errors"
	"testing"
)

type schedulingSnapshot0030Row struct {
	values []any
	err    error
}

func (r schedulingSnapshot0030Row) Scan(destinations ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, value := range r.values {
		*destinations[i].(*any) = value
	}
	return nil
}

func TestSchedulingSnapshot0030PreservesStoredTypes(t *testing.T) {
	var text string
	var revision int64
	var enabled bool
	if err := scanSchedulingSnapshot(schedulingSnapshot0030Row{values: []any{"reason", int64(1), int64(1)}}, &text, &revision, &enabled); err != nil || text != "reason" || revision != 1 || !enabled {
		t.Fatal("valid stored values refused", text, revision, enabled, err)
	}
	for _, tc := range []struct {
		name        string
		value       any
		destination any
	}{
		{"blob-string", []byte("reason"), &text},
		{"number-string", int64(1), &text},
		{"null-string", nil, &text},
		{"text-revision", "1", &revision},
		{"real-revision", float64(1), &revision},
		{"blob-revision", []byte("1"), &revision},
		{"null-revision", nil, &revision},
		{"text-enabled", "true", &enabled},
		{"real-enabled", float64(1), &enabled},
		{"invalid-enabled", int64(2), &enabled},
		{"negative-enabled", int64(-1), &enabled},
		{"null-enabled", nil, &enabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attemptCode(t, scanSchedulingSnapshot(schedulingSnapshot0030Row{values: []any{tc.value}}, tc.destination), "storage_unavailable")
		})
	}
	failure := errors.New("synthetic query failure")
	if got := scanSchedulingSnapshot(schedulingSnapshot0030Row{err: failure}, &text); got != failure {
		t.Fatal("query failure reclassified as retained data", got)
	}
}
