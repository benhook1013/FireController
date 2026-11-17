//go:build linux

package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprocess"
)

func TestVendorCacheRetirementActualNativeCaptureAndNamedPayloads(t *testing.T) {
	for _, mode := range []string{"success", "budget", "active", "workspace-present", "tampered-capture", "wrong-workspace", "hardlink", "custody-busy", "partial-error", "partial-evidence-change"} {
		t.Run(mode, func(t *testing.T) {
			f := newCLIEvidenceFixture(t)
			a, w := f.startAdmitted(t, 2)
			capture := makeCompleteCLICapture(t, w, mustChangedFiles(t, f.manager, w))
			start, finish := time.Now().Add(-time.Second).UTC(), time.Now().UTC()
			if _, err := RetainCLI(t.Context(), f.captures, f.engine, f.manager, a, w, capture, start, finish); err != nil {
				t.Fatal(err)
			}
			if mode != "active" {
				if _, err := f.engine.Advance(t.Context(), f.project, a.ID, a.Revision, "completed", "completion", ""); err != nil {
					t.Fatal(err)
				}
			}
			// Advance only the observation clock, not the native journal or source bytes.
			now := time.Now().Add(time.Hour)
			if mode != "workspace-present" {
				if _, err := f.manager.Prune(t.Context(), func(ctx context.Context, id string) (bool, error) {
					return f.engine.WorkspaceDisposable(ctx, "runtime-capability", id, now)
				}); err != nil {
					t.Fatal(err)
				}
			}
			home := filepath.Join(t.TempDir(), "home")
			relative := "0123abcd/4567abcd/reviews/1700000000000"
			session := filepath.Join(home, ".coderabbit", "reviews", relative)
			if err := os.MkdirAll(session, 0700); err != nil {
				t.Fatal(err)
			}
			expected := w.Directory
			if mode == "wrong-workspace" {
				expected = "/other"
			}
			metadata, _ := json.Marshal(map[string]string{"workingDirectory": expected})
			write := func(name string, b []byte) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(session, name), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("git.json", metadata)
			write(".session-complete-v2", nil)
			write("incrementalDiff.v2.json", []byte("disposable diff"))
			write("internalState.json", []byte("disposable state"))
			write("unknown-retained", []byte("keep"))
			if mode == "hardlink" {
				if err := os.Link(filepath.Join(session, "internalState.json"), filepath.Join(session, "alias")); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "tampered-capture" {
				p := filepath.Join(f.captures.root, a.ID+".json")
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(p, append(b, ' '), 0600); err != nil {
					t.Fatal(err)
				}
			}
			custody := &reviewprocess.CacheCustody{}
			lease, err := reviewprocess.NewCacheHomeLease(home)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			runner, err := reviewprocess.New(f.manager, "/bin/true", map[string]string{"HOME": home}, reviewprocess.WithCacheCustody(custody), reviewprocess.WithCacheHomeLease(lease))
			if err != nil {
				t.Fatal(err)
			}
			limit := int64(1024)
			if mode == "budget" {
				limit = 1
			}
			var result vendorCacheRetirement
			run := func() error {
				var e error
				if mode == "partial-error" || mode == "partial-evidence-change" {
					result, e = retireVendorCachePayloadsObserved(t.Context(), runner, f.manager, f.engine, f.captures, "runtime-capability", relative, a.ID, limit, now, func(count int) error {
						if count != 1 {
							return nil
						}
						if mode == "partial-error" {
							return errors.New("injected post-sync interruption")
						}
						path := filepath.Join(f.captures.root, a.ID+".json")
						b, readErr := os.ReadFile(path)
						if readErr != nil {
							return readErr
						}
						return os.WriteFile(path, append(b, ' '), 0600)
					})
				} else {
					result, e = retireVendorCachePayloads(t.Context(), runner, f.manager, f.engine, f.captures, "runtime-capability", relative, a.ID, limit, now)
				}
				return e
			}
			if mode == "custody-busy" {
				err = custody.TryMaintenance(t.Context(), run)
			} else {
				err = run()
			}
			if mode == "success" {
				if err != nil || result.Files != 2 || result.LogicalBytes != int64(len("disposable diff")+len("disposable state")) {
					t.Fatal(result, err)
				}
				again, e := retireVendorCachePayloads(t.Context(), runner, f.manager, f.engine, f.captures, "runtime-capability", relative, a.ID, limit, now)
				if e != nil || again.Files != 0 {
					t.Fatal("retry", again, e)
				}
			} else if mode == "partial-error" || mode == "partial-evidence-change" {
				if err == nil || result.Files != 1 || result.LogicalBytes != int64(len("disposable diff")) {
					t.Fatal("partial progress lost", result, err)
				}
				if _, e := os.Stat(filepath.Join(session, "incrementalDiff.v2.json")); !os.IsNotExist(e) {
					t.Fatal("first payload not removed", e)
				}
				if _, e := os.Stat(filepath.Join(session, "internalState.json")); e != nil {
					t.Fatal("second payload changed", e)
				}
				again, e := retireVendorCachePayloads(t.Context(), runner, f.manager, f.engine, f.captures, "runtime-capability", relative, a.ID, limit, now)
				if mode == "partial-error" {
					if e != nil || again.Files != 1 || again.LogicalBytes != int64(len("disposable state")) {
						t.Fatal("partial retry", again, e)
					}
				} else if e == nil || again.Files != 0 {
					t.Fatal("changed evidence retry", again, e)
				}
			} else {
				if err == nil || result.Files != 0 {
					t.Fatal("unsafe removal", result, err)
				}
				for _, name := range []string{"incrementalDiff.v2.json", "internalState.json"} {
					if _, e := os.Stat(filepath.Join(session, name)); e != nil {
						t.Fatal(e)
					}
				}
			}
			for _, name := range []string{"git.json", ".session-complete-v2", "unknown-retained"} {
				if _, e := os.Stat(filepath.Join(session, name)); e != nil {
					t.Fatal("metadata changed", e)
				}
			}
			if _, e := os.Stat(filepath.Join(f.captures.root, a.ID+".json")); e != nil {
				t.Fatal("capture removed", e)
			}
		})
	}
}
