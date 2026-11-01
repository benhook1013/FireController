package offsite

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOffsiteWorkspaceMarkerInterruptedPublicationRecovery(t *testing.T) {
	for _, phase := range []string{"empty_work", "empty_temp", "partial_temp", "complete_temp", "published_with_temp"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			if err := privateDir(f.c.StateDir, true); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(f.c.StateDir, "work")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			owner := []byte(binding(f.c))
			if phase != "empty_work" {
				content := owner
				if phase == "empty_temp" {
					content = nil
				} else if phase == "partial_temp" {
					content = owner[:len(owner)/2]
				}
				pending := filepath.Join(dir, ".owner.pending")
				if err := os.WriteFile(pending, content, 0600); err != nil {
					t.Fatal(err)
				}
				if phase == "published_with_temp" {
					if err := os.Link(pending, filepath.Join(dir, ".owner")); err != nil {
						t.Fatal(err)
					}
				}
			}
			s, err := openSession(f.c, memory())
			if err != nil {
				t.Fatal("interrupted marker prevented the next session", err)
			}
			defer s.close()
			entries, err := os.ReadDir(s.work)
			if err != nil || len(entries) != 1 || entries[0].Name() != ".owner" {
				t.Fatal("complete publication left unexpected workspace contents", entries, err)
			}
			marker := filepath.Join(s.work, ".owner")
			actual, err := privateRead(marker, 64)
			if err != nil || !bytes.Equal(actual, owner) {
				t.Fatal("published marker is not the complete connection binding", err)
			}
			info, err := os.Lstat(marker)
			if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatal("published marker is not a private regular file", err)
			}
			_, err = openSession(f.c, memory())
			wantCode(t, err, "offsite_busy")
			s.close()
			if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("owned workspace survived session cleanup", err)
			}
		})
	}
}

func TestOffsiteWorkspaceMarkerRecoveryPreservesUnknownData(t *testing.T) {
	for _, kind := range []string{"wrong_temp", "oversize_temp", "temp_symlink", "temp_directory", "extra_file", "extra_directory", "similar_name", "partial_owner", "foreign_owner", "owner_symlink"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			if err := privateDir(f.c.StateDir, true); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(f.c.StateDir, "work")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			pending := filepath.Join(dir, ".owner.pending")
			content := []byte(binding(f.c)[:16])
			if kind == "wrong_temp" {
				content = []byte("not this connection")
			} else if kind == "oversize_temp" {
				content = []byte(binding(f.c) + "x")
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("KEEP"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "temp_symlink":
				if err := os.Symlink(outside, pending); err != nil {
					t.Skip(err)
				}
			case "temp_directory":
				if err := os.Mkdir(pending, 0700); err != nil {
					t.Fatal(err)
				}
			case "similar_name":
				pending += ".other"
				fallthrough
			default:
				if err := os.WriteFile(pending, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "extra_file":
				if err := os.WriteFile(filepath.Join(dir, "KEEP"), []byte("unknown"), 0600); err != nil {
					t.Fatal(err)
				}
			case "extra_directory":
				if err := os.Mkdir(filepath.Join(dir, "KEEP"), 0700); err != nil {
					t.Fatal(err)
				}
			case "partial_owner", "foreign_owner":
				owner := content
				if kind == "foreign_owner" {
					owner = bytes.Repeat([]byte("!"), 64)
				}
				if err := os.WriteFile(filepath.Join(dir, ".owner"), owner, 0600); err != nil {
					t.Fatal(err)
				}
			case "owner_symlink":
				if err := os.Symlink(outside, filepath.Join(dir, ".owner")); err != nil {
					t.Skip(err)
				}
			}
			before, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			infos := make([]os.FileInfo, len(before))
			for i, entry := range before {
				infos[i], err = entry.Info()
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := prepareWork(f.c); err == nil {
				t.Fatal("claimed unknown or foreign workspace data")
			}
			after, err := os.ReadDir(dir)
			if err != nil || len(after) != len(before) {
				t.Fatal("refusal removed unknown entries", err)
			}
			for i, entry := range before {
				current, err := after[i].Info()
				if err != nil || entry.Name() != after[i].Name() || !os.SameFile(infos[i], current) {
					t.Fatal("refusal replaced unknown entries", err)
				}
			}
			if kind != "temp_symlink" && kind != "temp_directory" {
				actual, err := os.ReadFile(pending)
				if err != nil || !bytes.Equal(actual, content) {
					t.Fatal("refusal changed temporary marker bytes", err)
				}
			}
			if actual, err := os.ReadFile(outside); err != nil || string(actual) != "KEEP" {
				t.Fatal("refusal changed symlink target", err)
			}
		})
	}
}

func TestOffsiteWorkspaceMarkerRecoveryPreservesPendingPublication(t *testing.T) {
	f := newFixture(t)
	bundle := f.bundle(t)
	if err := privateDir(f.c.StateDir, true); err != nil {
		t.Fatal(err)
	}
	// Durable publication intent and spool are outside disposable work.
	r, err := pack(t.Context(), bundle.Path, filepath.Join(f.c.StateDir, "pending.tar.gz"), f.c)
	if err != nil {
		t.Fatal(err)
	}
	st := state(t, f.c)
	st.Pending = &r
	if err := writeJSON(filepath.Join(f.c.StateDir, "offsite.json"), st); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.c.StateDir, "work")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".owner.pending"), []byte(binding(f.c)[:1]), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.c.SourceRoot); err != nil {
		t.Fatal(err)
	}
	remote := memory()
	result, err := push(t.Context(), f.c, remote)
	if err != nil || result.State != "complete" || result.BackupID != r.ID {
		t.Fatal("marker recovery lost durable pending publication", result, err)
	}
	after := state(t, f.c)
	if after.Pending != nil || after.LastSuccess == nil || *after.LastSuccess != r || len(after.Owned) != 1 || after.Owned[0] != r || remote.deletes != 0 {
		t.Fatal("recovery changed exact publication identity", after)
	}
	for name, count := range remote.puts {
		if count != 1 {
			t.Fatal("recovery duplicated a remote publication", name, count)
		}
	}
}
