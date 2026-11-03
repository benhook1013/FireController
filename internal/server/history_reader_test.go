package server

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func historyReaderTokenFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "selected.token")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGitHubHistoryReaderExplicitCredentialAndStableFingerprint(t *testing.T) {
	const token = "synthetic-history-selected-token"
	first, fingerprint, err := GitHubHistoryReader(historyReaderTokenFile(t, token+"\n"))
	if err != nil || first == nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fingerprint) || strings.Contains(fingerprint, token) {
		t.Fatal("explicit history reader construction failed")
	}
	second, same, err := GitHubHistoryReader(historyReaderTokenFile(t, " \t"+token+"\n"))
	if err != nil || second == nil || first == second || same != fingerprint {
		t.Fatal("equivalent selected credentials did not retain stable reader identity")
	}
	_, changed, err := GitHubHistoryReader(historyReaderTokenFile(t, "synthetic-changed-history-token"))
	if err != nil || changed == fingerprint {
		t.Fatal("changed selected credential retained its prior identity")
	}
	reader, public, err := GitHubHistoryReader("")
	if err != nil || reader == nil || public == "" || public == fingerprint {
		t.Fatal("public read construction unavailable or confused with private identity")
	}
	// These tests do not run in parallel or change HOME/working directories.
	// Empty explicit selection must not discover credentials from ambient CLI
	// conventions, regardless of their content or configured directory.
	t.Setenv("GH_TOKEN", "ignored-ambient-token")
	t.Setenv("GITHUB_TOKEN", "ignored other token that is invalid as a credential")
	t.Setenv("GH_CONFIG_DIR", filepath.Join(t.TempDir(), "must-not-discover"))
	reader, stillPublic, err := GitHubHistoryReader("")
	if err != nil || reader == nil || stillPublic != public {
		t.Fatal("empty explicit selection discovered ambient credentials")
	}
	if _, boundary, err := GitHubHistoryReader(historyReaderTokenFile(t, strings.Repeat("a", 4096))); err != nil || boundary == "" {
		t.Fatal("bounded private credential refused", err)
	}
}

func TestGitHubHistoryReaderRefusesUnsafeSelectedFilesWithoutDisclosure(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "empty", "oversized", "spaces", "embedded-newline", "nonascii", "invalid-utf8", "control", "public", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			const private = "PRIVATE_HISTORY_TOKEN_CONTENT"
			path := filepath.Join(t.TempDir(), "PRIVATE_HISTORY_SELECTED_PATH")
			body := private
			switch kind {
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "empty":
				body = " \t\n"
			case "oversized":
				body = strings.Repeat("a", 4097)
			case "spaces":
				body += " other"
			case "embedded-newline":
				body += "\nother"
			case "nonascii":
				body += "雪"
			case "invalid-utf8":
				body += string([]byte{255})
			case "control":
				body += "\x00"
			case "public":
				if runtime.GOOS == "windows" {
					t.Skip("Unix permission mode is not Windows ACL proof")
				}
			case "symlink":
				target := historyReaderTokenFile(t, private)
				if err := os.Symlink(target, path); err != nil {
					t.Skip("symlink creation unavailable")
				}
			}
			if kind != "missing" && kind != "directory" && kind != "symlink" {
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "public" {
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			reader, fingerprint, err := GitHubHistoryReader(path)
			var failure *core.Error
			if reader != nil || fingerprint != "" || !errors.As(err, &failure) || failure.Code != "configuration_invalid" || strings.Contains(err.Error(), private) || strings.Contains(err.Error(), path) {
				t.Fatal("unsafe credential was adopted or disclosed")
			}
		})
	}
}
