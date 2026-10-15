package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewruntime"
)

func privateTestCatalog(t *testing.T, channels []string) (string, reviewruntime.Catalog) {
	t.Helper()
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider.token"), []byte("synthetic-private-operator-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"version": 1, "capabilities": []any{map[string]any{
		"id": "local-cli", "account": "included-account", "organization": "test-org", "channels": channels,
		"git_executable": executable, "cli_executable": executable, "github_credential_file": "provider.token",
		"authentication_environment": map[string]string{"HOME": filepath.Join(dir, "cli-home")},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := reviewruntime.LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, catalog
}

func TestReviewExecutionCompositionIsExplicit(t *testing.T) {
	for _, mode := range []string{"disabled", "cli", "unsupported-hosted"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "private")
			store, err := core.Open(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			catalog, err := reviewruntime.LoadCatalog("")
			if err != nil {
				t.Fatal(err)
			}
			if mode != "disabled" {
				channels := []string{"cli"}
				if mode == "unsupported-hosted" {
					channels = []string{"cli", "hosted"}
				}
				_, catalog = privateTestCatalog(t, channels)
			}
			engine, drivers, err := buildReviewExecution(store, dir, catalog)
			if mode == "unsupported-hosted" {
				if err == nil || engine != nil || drivers != nil || !strings.Contains(err.Error(), "CLI execution only") {
					t.Fatal("unsupported channel silently became runnable", err)
				}
			} else if err != nil || engine == nil || len(drivers) != map[string]int{"disabled": 0, "cli": 1}[mode] {
				t.Fatal("runtime composition failed", err)
			}
			for _, name := range []string{"review-captures", "review-workspaces"} {
				info, err := os.Stat(filepath.Join(dir, name))
				if mode == "cli" {
					if err != nil || !info.IsDir() {
						t.Fatal("explicit native storage missing", err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatal("disabled or rejected runtime created native execution storage", err)
				}
			}
		})
	}
}

func TestDaemonCatalogFailurePrecedesDatabaseCreation(t *testing.T) {
	dir := t.TempDir()
	catalog := filepath.Join(dir, "private-invalid-catalog.json")
	if err := os.WriteFile(catalog, []byte(`{"version":1,"capabilities":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	stdout := &output{ready: make(chan struct{})}
	stderr := &output{ready: make(chan struct{})}
	dataDir := filepath.Join(dir, "uncreated-data")
	if code := run(context.Background(), []string{"--data-dir", dataDir, "--listen", "127.0.0.1:0", "--review-catalog", catalog}, stdout, stderr); code != 3 {
		t.Fatal("invalid catalog started service", code)
	}
	if stdout.String() != "" || strings.Contains(stderr.String(), catalog) || strings.Contains(stderr.String(), dir) {
		t.Fatal("startup exposed private configuration or announced readiness")
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatal("invalid operator catalog created a database", err)
	}
}

func TestDaemonExplicitCLICatalogStartsAndStopsWithoutProviderWork(t *testing.T) {
	path, _ := privateTestCatalog(t, []string{"cli"})
	dir := filepath.Join(t.TempDir(), "private")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout := &output{ready: make(chan struct{})}
	stderr := &output{ready: make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--data-dir", dir, "--listen", "127.0.0.1:0", "--review-catalog", path}, stdout, stderr)
	}()
	select {
	case <-stdout.ready:
	case code := <-done:
		t.Fatalf("native startup failed: %d %s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatal("native startup deadline")
	}
	var started map[string]string
	if err := json.Unmarshal([]byte(stdout.String()), &started); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + started["address"] + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("explicit runtime not ready", response.StatusCode)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 || stderr.String() != "" {
			t.Fatalf("native shutdown: %d %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native shutdown deadline")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "review-captures"))
	if err != nil || len(entries) != 0 {
		t.Fatal("idle startup created a provider capture", err)
	}
}
