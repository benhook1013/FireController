package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func assertMutationOverloadLog(t *testing.T, err error, stage, cause string) {
	t.Helper()
	var failure *Error
	var observation *mutationOverload
	if !errors.As(err, &failure) || failure.Code != "overloaded" || failure.Status != 503 || !errors.As(err, &observation) {
		t.Fatal("original overload class or private metadata lost", err)
	}
	var output bytes.Buffer
	prior := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(prior)
	w := httptest.NewRecorder()
	WriteError(w, fmt.Errorf("PRIVATE wrapper SQL: %w", err))
	lines := strings.TrimSpace(output.String())
	var entry map[string]any
	start := strings.IndexByte(lines, '{')
	if start < 0 || json.Unmarshal([]byte(lines[start:]), &entry) != nil || len(entry) != 6 || entry["event"] != "mutation_overloaded" || entry["request_id"] != w.Header().Get("X-Request-ID") || entry["stage"] != stage || entry["cause"] != cause || strings.Contains(lines, "PRIVATE") || strings.Contains(lines, "private") {
		t.Fatal("unsafe, missing or duplicate structured overload log", lines)
	}
	elapsed, ok := entry["elapsed_ms"].(float64)
	admission, okAdmission := entry["admission_ms"].(float64)
	if !ok || !okAdmission || elapsed < 0 || admission < 0 || admission > elapsed || stage == "writer_admission" && admission < 1 {
		t.Fatal("invalid elapsed/admission timing", entry)
	}
	// The public response is byte-for-byte the original classified error. Only
	// the private wrapper produces a log, and no metadata enters JSON/headers.
	plain := httptest.NewRecorder()
	plain.Header().Set("X-Request-ID", w.Header().Get("X-Request-ID"))
	WriteError(plain, failure)
	if w.Code != plain.Code || w.Body.String() != plain.Body.String() || fmt.Sprint(w.Header()) != fmt.Sprint(plain.Header()) || strings.TrimSpace(output.String()) != lines {
		t.Fatal("instrumentation changed response or logged an ordinary refusal")
	}
}

func TestMutationOverloadAdmissionAndApplyTimingPreserveRollbackAndReplay(t *testing.T) {
	s, scope, _ := streamFixture(t)
	m := Mutation{Operation: "PRIVATE-operation", Target: "PRIVATE-target", Key: "PRIVATE-key", Request: map[string]string{"input": "PRIVATE-input"}}
	var calls int
	apply := func(ctx context.Context, tx *sql.Tx) (any, error) {
		calls++
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('mutation-diagnostic-probe','PRIVATE-body')`); err != nil {
			return nil, err
		}
		if err := AppendEvent(ctx, tx, scope, "diagnostic.probe", "PRIVATE-resource", 1, map[string]string{"value": "PRIVATE-body"}); err != nil {
			return nil, err
		}
		return map[string]string{"outcome": "saved"}, nil
	}
	// Occupy the actual writer admission token. The callback must never start.
	s.admission <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err := s.Mutate(ctx, scope, m, apply)
	cancel()
	<-s.admission
	assertMutationOverloadLog(t, err, "writer_admission", "deadline")
	if calls != 0 {
		t.Fatal("callback ran without writer admission")
	}
	ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err = s.Mutate(ctx, scope, m, func(ctx context.Context, tx *sql.Tx) (any, error) {
		value, err := apply(ctx, tx)
		if err != nil {
			return nil, err
		}
		<-ctx.Done()
		return value, ctx.Err()
	})
	cancel()
	assertMutationOverloadLog(t, err, "apply", "deadline")
	var changes int
	if err := s.DB().QueryRow(`SELECT (SELECT count(*) FROM metadata WHERE key='mutation-diagnostic-probe')+(SELECT count(*) FROM receipts)+(SELECT count(*) FROM events)`).Scan(&changes); err != nil || changes != 0 || calls != 1 {
		t.Fatal("timed-out transaction partially committed", changes, calls, err)
	}
	first, err := s.Mutate(t.Context(), scope, m, apply)
	if err != nil {
		t.Fatal("same-key recovery failed", err)
	}
	replayed, err := s.Mutate(t.Context(), scope, m, apply)
	if err != nil || !bytes.Equal(first, replayed) || calls != 2 {
		t.Fatal("exact replay changed effect or callback count", string(replayed), calls, err)
	}
	if err := s.DB().QueryRow(`SELECT (SELECT count(*) FROM metadata WHERE key='mutation-diagnostic-probe')+(SELECT count(*) FROM receipts)+(SELECT count(*) FROM events)`).Scan(&changes); err != nil || changes != 3 {
		t.Fatal("recovery/replay duplicated committed work", changes, err)
	}
}

func TestMutationOverloadKeepsProvenBusyAndUnknownCauseDistinct(t *testing.T) {
	s, scope, _ := streamFixture(t)
	for _, tc := range []struct {
		name, cause string
		failure     error
	}{
		{"busy", "sqlite_busy", storageTestCode(5)},
		{"canceled", "canceled", context.Canceled},
		{"domain", "unknown", Fail("overloaded", "A bounded domain refusal.", 503)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Mutate(t.Context(), scope, Mutation{Operation: "PRIVATE-operation", Target: "PRIVATE-target", Key: "PRIVATE-" + tc.name}, func(context.Context, *sql.Tx) (any, error) {
				return nil, tc.failure
			})
			assertMutationOverloadLog(t, err, "apply", tc.cause)
		})
	}
	for _, failure := range []error{Fail("forbidden", "Current access refused.", 403), errors.New("PRIVATE SQL token")} {
		_, err := s.Mutate(t.Context(), scope, Mutation{Operation: "probe", Target: "probe", Key: "not-overloaded"}, func(context.Context, *sql.Tx) (any, error) { return nil, failure })
		var observation *mutationOverload
		if err != failure || errors.As(err, &observation) {
			t.Fatal("non-overload was replaced or annotated", err)
		}
	}
}
