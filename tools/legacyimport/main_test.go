package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func readDiagnostic(t *testing.T, data []byte) string {
	t.Helper()
	var result struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &result) != nil || result.Error.Code == "" || result.Error.Message == "" {
		t.Fatal("missing safe structured diagnostic")
	}
	return result.Error.Code
}

func TestInvalidArgumentsDoNotRevealPrivateInput(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "private-source-name.sqlite")
	cases := [][]string{
		nil,
		{"apply", "--snapshot", secretPath},
		{"inspect", "--snapshot", secretPath, "--snapshot", secretPath},
		{"inspect", "--snapshot", secretPath, "--unknown=private-value"},
		{"inspect", "--snapshot", secretPath, "extra-private-value"},
		{"inspect", "--snapshot", "relative-private-source.sqlite"},
		{"inspect", "--snapshot", secretPath, "--timeout", "0s"},
		{"inspect", "--snapshot", secretPath, "--timeout", "6m"},
		{"inspect", "--snapshot", secretPath, "--timeout", "private-value"},
		{"inspect", "--snapshot", secretPath, "--max-cell-bytes", "0"},
		{"inspect", "--snapshot", secretPath, "--max-total-rows", "-1"},
	}
	for i, args := range cases {
		t.Run("case-"+strconv.Itoa(i), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), args, &stdout, &stderr); code != 2 {
				t.Fatalf("exit=%d", code)
			}
			if stdout.Len() != 0 || readDiagnostic(t, stderr.Bytes()) != "arguments_invalid" || bytes.Contains(stderr.Bytes(), []byte("private")) {
				t.Fatal("argument rejection exposed private input or emitted a success result")
			}
			if _, err := os.Lstat(secretPath); !os.IsNotExist(err) {
				t.Fatal("argument rejection created a source file")
			}
		})
	}
}

func TestInvalidSnapshotIsReadOnlyAndDiagnosticsAreSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-source-name.sqlite")
	original := []byte("private-database-content-is-not-a-sqlite-header")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"inspect", "--snapshot", path}, &stdout, &stderr); code != 3 {
		t.Fatalf("exit=%d", code)
	}
	if stdout.Len() != 0 || readDiagnostic(t, stderr.Bytes()) == "" || bytes.Contains(stderr.Bytes(), []byte("private")) {
		t.Fatal("invalid snapshot leaked path or content")
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, current) {
		t.Fatal("source changed")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !os.IsNotExist(err) {
			t.Fatal("inspection created a sidecar")
		}
	}
}

func TestCanceledInspectionHasNoSuccessOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"inspect", "--snapshot", filepath.Join(t.TempDir(), "absent.sqlite")}, &stdout, &stderr); code != 130 {
		t.Fatalf("exit=%d", code)
	}
	if stdout.Len() != 0 || readDiagnostic(t, stderr.Bytes()) != "inspection_canceled" {
		t.Fatal("cancellation did not preserve its diagnostic boundary")
	}
}
