package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestBackgroundWriteLogsSanitizedStorageFailure(t *testing.T) {
	store, _, _ := streamFixture(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	err := store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT * FROM private_missing_fixture_table")
		return err
	})
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "storage_unavailable" {
		t.Fatalf("expected unchanged public storage error, got %v", err)
	}
	var observation struct {
		Message     string `json:"msg"`
		Stage       string `json:"stage"`
		Cause       string `json:"cause"`
		Code        int    `json:"sqlite_code"`
		PrimaryCode int    `json:"sqlite_primary_code"`
	}
	if err := json.Unmarshal(logs.Bytes(), &observation); err != nil {
		t.Fatal(err)
	}
	if observation.Message != "background_write_failed" || observation.Stage != "apply" ||
		observation.Cause != "sqlite" || observation.Code != 1 || observation.PrimaryCode != 1 {
		t.Fatalf("unexpected storage diagnostic %+v", observation)
	}
	if strings.Contains(logs.String(), "private_missing_fixture_table") || strings.Contains(err.Error(), "private_missing_fixture_table") {
		t.Fatal("database details leaked")
	}

	logs.Reset()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = store.BackgroundWrite(ctx, func(context.Context, *sql.Tx) error { return nil })
	if !errors.As(err, &failure) || failure.Code != "overloaded" || logs.Len() != 0 {
		t.Fatalf("ordinary cancellation should retain overload semantics without a storage failure log, got %v", err)
	}
}
