package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadAdmissionOverloadSeparatesPoolFromExecution(t *testing.T) {
	s, scope, _ := streamFixture(t)
	s.DB().SetMaxOpenConns(1)
	conn, err := s.DB().Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	called := false
	callback := func(context.Context, *sql.Tx) error { called = true; return nil }
	err = s.Read(t.Context(), scope, callback)
	var failure *Error
	if !IsReadAdmissionOverload(err) || !IsReadAdmissionOverload(fmt.Errorf("wrapped: %w", err)) || !errors.As(err, &failure) || failure.Code != "overloaded" || failure.Status != 503 || called || t.Context().Err() != nil {
		t.Fatalf("pool deadline was not narrowly classified: err=%v called=%v", err, called)
	}
	// Classification is internal. HTTP retains exactly the normal safe error DTO.
	response := httptest.NewRecorder()
	WriteError(response, err)
	var wire struct {
		Error     Error  `json:"error"`
		RequestID string `json:"request_id"`
	}
	if response.Code != 503 || json.Unmarshal(response.Body.Bytes(), &wire) != nil || wire.Error.Code != failure.Code || wire.Error.Message != failure.Message || wire.Error.Details != nil || wire.RequestID == "" {
		t.Fatal("wrapper changed safe HTTP error", response.Code)
	}
	for _, tc := range []struct {
		name        string
		makeContext func() (context.Context, context.CancelFunc)
	}{
		{"cancelled", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx, cancel
		}},
		{"caller-deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(t.Context(), 10*time.Millisecond)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.makeContext()
			defer cancel()
			err := s.Read(ctx, scope, callback)
			if IsReadAdmissionOverload(err) || !errors.As(err, &failure) || failure.Code != "overloaded" || called {
				t.Fatalf("caller failure marked as admission: %v", err)
			}
		})
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	t.Run("statement-deadline", func(t *testing.T) {
		entered := false
		err := s.Read(t.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
			entered = true
			var total int64
			return tx.QueryRowContext(ctx, `WITH RECURSIVE sequence(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM sequence) SELECT sum(n) FROM sequence`).Scan(&total)
		})
		if !entered || IsReadAdmissionOverload(err) || !errors.As(err, &failure) || failure.Code != "overloaded" {
			t.Fatalf("statement deadline mislabeled: %v entered=%v", err, entered)
		}
	})
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{"domain-overload", Fail("overloaded", "Bounded wait capacity occupied.", 503)},
		{"sqlite-busy", storageTestCode(5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Read(t.Context(), scope, func(context.Context, *sql.Tx) error { return tc.failure })
			if IsReadAdmissionOverload(err) || !errors.As(err, &failure) || failure.Code != "overloaded" {
				t.Fatalf("non-admission overload mislabeled: %v", err)
			}
		})
	}
	if _, err := s.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='credential'`); err != nil {
		t.Fatal(err)
	}
	called = false
	err = s.Read(t.Context(), scope, callback)
	if IsReadAdmissionOverload(err) || !errors.As(err, &failure) || failure.Code != "unauthorized" || called {
		t.Fatalf("authorization failure mislabeled or callback invoked: %v", err)
	}
	if IsReadAdmissionOverload(nil) || IsReadAdmissionOverload(context.DeadlineExceeded) {
		t.Fatal("unclassified error admitted")
	}
}
