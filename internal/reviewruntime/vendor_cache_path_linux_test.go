//go:build linux

package reviewruntime

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestVendorCachePathBinding(t *testing.T) {
	for _, mode := range []string{"good", "wrong-workspace", "existing-workspace", "duplicate-key", "session-link", "metadata-link", "metadata-hardlink", "oversize", "missing-marker", "public-session", "ancestor-link", "traversal"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			data := filepath.Join(root, "work")
			git, err := exec.LookPath("git")
			if err != nil {
				t.Skip("git unavailable")
			}
			manager, err := reviewworkspace.New(data, git)
			if err != nil {
				t.Fatal(err)
			}
			attempt := "review_attempt_01234567890123456789012345678901"
			expected := filepath.Join(data, "workspaces", attempt)
			rel := "0123abcd/4567abcd/reviews/1700000000000"
			session := filepath.Join(home, ".coderabbit", "reviews", rel)
			if err = os.MkdirAll(session, 0700); err != nil {
				t.Fatal(err)
			}
			meta, _ := json.Marshal(map[string]string{"workingDirectory": expected})
			if err = os.WriteFile(filepath.Join(session, "git.json"), meta, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(session, ".session-complete-v2"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "wrong-workspace":
				_ = os.WriteFile(filepath.Join(session, "git.json"), []byte(`{"workingDirectory":"/other"}`), 0600)
			case "existing-workspace":
				_ = os.Mkdir(expected, 0700)
			case "duplicate-key":
				_ = os.WriteFile(filepath.Join(session, "git.json"), []byte(`{"workingDirectory":"/other","workingDirectory":`+string(mustJSON(expected))+`}`), 0600)
			case "session-link":
				_ = os.Rename(session, session+"-original")
				_ = os.Symlink(session+"-original", session)
			case "metadata-link":
				_ = os.Rename(filepath.Join(session, "git.json"), filepath.Join(session, "original"))
				_ = os.Symlink("original", filepath.Join(session, "git.json"))
			case "metadata-hardlink":
				_ = os.Link(filepath.Join(session, "git.json"), filepath.Join(session, "copy"))
			case "oversize":
				_ = os.WriteFile(filepath.Join(session, "git.json"), make([]byte, 65537), 0600)
			case "missing-marker":
				_ = os.Remove(filepath.Join(session, ".session-complete-v2"))
			case "public-session":
				_ = os.Chmod(session, 0777)
			case "ancestor-link":
				_ = os.Rename(home, home+"-original")
				_ = os.Symlink(home+"-original", home)
			case "traversal":
				rel = "../" + rel
			}
			got, err := observeVendorCachePath(home, rel, attempt, manager)
			if mode == "good" {
				if err != nil || got.workspace != expected || len(got.metadataSHA256) != 64 || got.sessionInode == 0 {
					t.Fatal(got, err)
				}
			} else if err == nil {
				t.Fatal("invalid binding accepted", mode, got)
			}
		})
	}
}
func mustJSON(s string) []byte { b, _ := json.Marshal(s); return b }
