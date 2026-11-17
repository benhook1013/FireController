package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGitHubWebhookSecretPrivateFile(t *testing.T) {
	if value, err := loadGitHubWebhookSecret(""); err != nil || value != nil {
		t.Fatal("disabled secret was read", err)
	}
	root := t.TempDir()
	path := filepath.Join(root, "secret")
	for _, tc := range []struct {
		body  string
		valid bool
	}{{strings.Repeat("a", 32), true}, {strings.Repeat("b", 64) + "\n", true}, {strings.Repeat("a", 31), false}, {strings.Repeat("a", 4097), false}, {strings.Repeat("a", 32) + "\nother", false}, {strings.Repeat("a", 32) + " ", false}} {
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		value, err := loadGitHubWebhookSecret(path)
		if (err == nil) != tc.valid || tc.valid && string(value) != strings.TrimSuffix(tc.body, "\n") {
			t.Fatal("secret boundary mismatch", len(tc.body), err)
		}
	}
	if _, err := loadGitHubWebhookSecret("relative-secret"); err == nil {
		t.Fatal("relative secret accepted")
	}
	if _, err := loadGitHubWebhookSecret(root); err == nil {
		t.Fatal("directory accepted")
	}
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(path, []byte(strings.Repeat("a", 32)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadGitHubWebhookSecret(path); err == nil {
			t.Fatal("public secret accepted")
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "secret-link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := loadGitHubWebhookSecret(link); err == nil {
			t.Fatal("symbolic link accepted")
		}
	}
}
