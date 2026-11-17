package backup

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func TestManagedRestartPreservesTerminalRetentionFacts(t *testing.T) {
	for _, test := range []struct {
		name, state, retention string
		after                  bool
		wantInterrupted        bool
	}{
		{"failed creation previous retention", "failed", "complete", false, false},
		{"failed creation no retention", "failed", "", false, false},
		{"successful settled retention", "complete", "complete", true, false},
		{"failed settled retention", "complete", "failed", true, false},
		{"pending retention with old result", "complete", "complete", false, true},
		{"pending retention without result", "complete", "", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, m := newManagedFixture(t, 1)
			now := time.Now().UTC().Add(-time.Minute)
			var prior *RetentionStatus
			if test.retention != "" {
				stamp := now.Add(-time.Minute)
				if test.after {
					stamp = now.Add(time.Second)
				}
				prior = &RetentionStatus{AttemptedAt: managedStamp(stamp), State: test.retention}
				if test.retention == "failed" {
					prior.Reason = "backup_retention_failed"
				}
			}
			if err := m.update(t.Context(), func(r *managedRecord, _ *sql.Tx) error {
				r.Status.State = "running"
				r.Status.NextAttemptAt = ""
				r.Status.LastAttempt = &AttemptStatus{StartedAt: managedStamp(now.Add(-time.Minute)), FinishedAt: managedStamp(now), State: test.state, SourceInstanceID: f.store.InstanceID()}
				if test.state == "failed" {
					r.Status.LastAttempt.Reason = "backup_unavailable"
				}
				r.Status.Retention = prior
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := NewManager(f.store, f.dir, m.config); err != nil {
				t.Fatal(err)
			}
			got, err := ReadStatus(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantInterrupted {
				if got.Retention == nil || got.Retention.Reason != "backup_retention_interrupted" {
					t.Fatal("missing genuine pending retention interruption")
				}
			} else if !reflect.DeepEqual(got.Retention, prior) {
				t.Fatal("restart invented or overwrote retention facts")
			}
		})
	}
}

func TestManagedAttemptSettlesStatusWithTerminalOutcome(t *testing.T) {
	f, m := newManagedFixture(t, 1)
	if err := m.attempt(t.Context()); err != nil {
		t.Fatal(err)
	}
	first, err := ReadStatus(t.Context(), f.dir)
	if err != nil || first.State != "scheduled" || first.NextAttemptAt == "" || first.Retention == nil || first.Retention.State != "complete" {
		t.Fatal("completed retention did not atomically settle worker state", err)
	}
	// Returning before Run's outer settlement is the real crash boundary. Clock
	// rollback must not turn a committed terminal result into an interruption.
	m.now = func() time.Time { return time.Now().UTC().Add(-time.Hour) }
	m.create = func(context.Context, *core.Store, string, string) (createdBundle, error) {
		return createdBundle{}, errors.New("synthetic creation failure")
	}
	if err := m.attempt(t.Context()); err == nil {
		t.Fatal("creation failure disappeared")
	}
	failed, err := ReadStatus(t.Context(), f.dir)
	if err != nil || failed.State != "scheduled" || failed.LastAttempt.State != "failed" || !reflect.DeepEqual(first.Retention, failed.Retention) {
		t.Fatal("failed creation damaged retained outcome", err)
	}
	if _, err := NewManager(f.store, f.dir, m.config); err != nil {
		t.Fatal(err)
	}
	restarted, err := ReadStatus(t.Context(), f.dir)
	if err != nil || !reflect.DeepEqual(failed.Retention, restarted.Retention) {
		t.Fatal("restart changed settled retention after clock rollback", err)
	}
}
