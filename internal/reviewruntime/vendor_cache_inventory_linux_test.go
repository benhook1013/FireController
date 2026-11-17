//go:build linux

package reviewruntime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
)

func TestVendorCacheInventoryBoundedTechnicalMetadata(t *testing.T) {
	for _, mode := range []string{"valid", "active", "foreign", "duplicate", "symlink", "canceled", "entry-limit"} {
		t.Run(mode, func(t *testing.T) {
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			manager, err := reviewworkspace.New(filepath.Join(t.TempDir(), "data"), git)
			if err != nil {
				t.Fatal(err)
			}
			attempt := "attempt_test"
			workspace, err := manager.RetiredPath(attempt)
			if err != nil {
				t.Fatal(err)
			}
			home := filepath.Join(t.TempDir(), "home")
			relative := "0123abcd/4567abcd/reviews/1700000000000"
			session := filepath.Join(home, ".coderabbit", "reviews", relative)
			if err = os.MkdirAll(session, 0700); err != nil {
				t.Fatal(err)
			}
			metadata, _ := json.Marshal(map[string]string{"workingDirectory": workspace})
			if err = os.WriteFile(filepath.Join(session, "git.json"), metadata, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(session, ".session-complete-v2"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(session, "internalState.json")
			if err = os.WriteFile(marker, []byte("never read or remove"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "active":
				if err = os.MkdirAll(workspace, 0700); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				if err = os.WriteFile(filepath.Join(session, "git.json"), []byte(`{"workingDirectory":"/other/attempt_test"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				if err = os.WriteFile(filepath.Join(session, "git.json"), []byte(`{"workingDirectory":"/x","workingDirectory":"/y"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err = os.Rename(filepath.Join(home, ".coderabbit"), filepath.Join(home, "saved")); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(filepath.Join(home, "saved"), filepath.Join(home, ".coderabbit")); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			case "entry-limit":
				dir := filepath.Join(home, ".coderabbit", "reviews")
				for i := 0; i < 4097; i++ {
					f, e := os.CreateTemp(dir, "ignored-")
					if e != nil {
						t.Fatal(e)
					}
					f.Close()
				}
			}
			got, err := inventoryVendorCache(ctx, home, manager)
			if mode == "symlink" || mode == "canceled" || mode == "entry-limit" {
				if err == nil {
					t.Fatal("unsafe inventory accepted", got)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if mode == "valid" {
					if len(got.Candidates) != 1 || got.Candidates[0] != (vendorCacheCandidate{relative, attempt}) {
						t.Fatal(got)
					}
				} else if len(got.Candidates) != 0 {
					t.Fatal("unowned candidate", got)
				}
				if mode == "duplicate" && got.Skipped != 1 {
					t.Fatal("missing refusal count", got)
				}
			}
			b, err := os.ReadFile(marker)
			if err != nil || string(b) != "never read or remove" {
				t.Fatal("payload changed", err)
			}
		})
	}
}
