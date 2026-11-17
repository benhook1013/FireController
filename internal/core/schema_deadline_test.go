package core

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSchemaMigrationAdmissionOutlivesOrdinaryWriteBudget(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	// One real Store admission slot models startup maintenance already in
	// progress. No expensive SQL or external database lock is required.
	s.admission <- struct{}{}
	var once sync.Once
	release := func() { once.Do(func() { <-s.admission }) }
	defer release()
	migration := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		migration <- s.EnsureSchema(ctx, "deadline_probe", []string{
			"CREATE TABLE schema_deadline_probe(value TEXT NOT NULL)",
			"INSERT INTO schema_deadline_probe VALUES('committed')",
		})
	}()
	<-started

	type outcome struct {
		err     error
		elapsed time.Duration
	}
	ordinary := make(chan outcome, 2)
	entered := make(chan struct{}, 2)
	go func() {
		start := time.Now()
		err := s.BackgroundWrite(ctx, func(context.Context, *sql.Tx) error {
			entered <- struct{}{}
			return nil
		})
		ordinary <- outcome{err, time.Since(start)}
	}()
	go func() {
		start := time.Now()
		_, err := s.Mutate(ctx, Scope{}, Mutation{Key: "deadline-probe", Operation: "probe", Target: "probe"}, func(context.Context, *sql.Tx) (any, error) {
			entered <- struct{}{}
			return nil, nil
		})
		ordinary <- outcome{err, time.Since(start)}
	}()
	for range 2 {
		select {
		case result := <-ordinary:
			var failure *Error
			if !errors.As(result.err, &failure) || failure.Code != "overloaded" {
				t.Fatalf("queued ordinary write did not retain its deadline: %v", result.err)
			}
			if result.elapsed < 1500*time.Millisecond || result.elapsed > 3500*time.Millisecond {
				t.Fatalf("ordinary write deadline changed from two seconds: %v", result.elapsed)
			}
		case <-time.After(3500 * time.Millisecond):
			t.Fatal("ordinary write was given the startup migration budget")
		}
	}
	select {
	case <-entered:
		t.Fatal("an ordinary callback ran without admission")
	default:
	}
	select {
	case err := <-migration:
		t.Fatalf("startup migration exhausted the ordinary write budget: %v", err)
	default:
	}
	release()
	select {
	case err := <-migration:
		if err != nil {
			t.Fatalf("startup migration did not commit after admission: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("startup migration did not release admission and complete")
	}
	var value, version string
	if err := s.DB().QueryRow("SELECT value FROM schema_deadline_probe").Scan(&value); err != nil || value != "committed" {
		t.Fatalf("migration SQL was not committed: %q, %v", value, err)
	}
	if err := s.DB().QueryRow("SELECT value FROM metadata WHERE key='schema:deadline_probe'").Scan(&version); err != nil || version != "1" {
		t.Fatalf("migration version was not committed atomically: %q, %v", version, err)
	}
}

func TestSchemaMigrationParentCancellationReleasesAdmission(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			s, err := Open(context.Background(), filepath.Join(t.TempDir(), "private"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			// Occupy the single real writer connection, so EnsureSchema takes
			// admission and waits without publishing any DDL or version metadata.
			conn, err := s.writer.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
			}
			defer cancel()
			var timer *time.Timer
			if !deadline {
				timer = time.AfterFunc(50*time.Millisecond, cancel)
				defer timer.Stop()
			}
			statements := []string{"CREATE TABLE canceled_schema_probe(value TEXT)", "INSERT INTO canceled_schema_probe VALUES('retained')"}
			start := time.Now()
			err = s.EnsureSchema(ctx, "canceled_probe", statements)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "overloaded" || ctx.Err() == nil {
				t.Fatalf("parent cancellation did not win: %v", err)
			}
			if time.Since(start) > time.Second {
				t.Fatal("startup migration ignored its earlier parent cancellation")
			}
			var published int
			if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='canceled_schema_probe'").Scan(&published); err != nil || published != 0 {
				t.Fatalf("canceled migration published DDL: %d, %v", published, err)
			}
			if err := s.DB().QueryRow("SELECT count(*) FROM metadata WHERE key='schema:canceled_probe'").Scan(&published); err != nil || published != 0 {
				t.Fatalf("canceled migration published its version: %d, %v", published, err)
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			retry, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := s.EnsureSchema(retry, "canceled_probe", statements); err != nil {
				t.Fatalf("canceled migration retained admission or blocked a safe retry: %v", err)
			}
			var value, version string
			if err := s.DB().QueryRow("SELECT value FROM canceled_schema_probe").Scan(&value); err != nil || value != "retained" {
				t.Fatalf("retry did not commit its SQL: %q, %v", value, err)
			}
			if err := s.DB().QueryRow("SELECT value FROM metadata WHERE key='schema:canceled_probe'").Scan(&version); err != nil || version != "1" {
				t.Fatalf("retry did not commit its version: %q, %v", version, err)
			}
		})
	}
}
