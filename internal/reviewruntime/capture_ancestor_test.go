package reviewruntime

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprocess"
)

func TestCaptureAncestorAliasCanonicalRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix ancestor alias/mode fixture")
	}
	for _, existing := range []bool{false, true} {
		name := "multiple-missing-components"
		if existing {
			name = "existing-root"
		}
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			real := filepath.Join(parent, "real")
			if err := os.Mkdir(real, 0755); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(parent, "alias")
			if err := os.Symlink(real, alias); err != nil {
				t.Fatal(err)
			}
			suffix := filepath.Join("missing-one", "missing-two", "captures")
			if existing {
				suffix = "captures"
				if err := os.Mkdir(filepath.Join(real, suffix), 0700); err != nil {
					t.Fatal(err)
				}
			}
			requested := filepath.Join(alias, suffix)
			store, err := NewCaptureStore(requested)
			if err != nil {
				t.Fatal("valid ancestor alias was rejected", err)
			}
			canonical, err := filepath.EvalSymlinks(requested)
			if err != nil || store.root != canonical {
				t.Fatal("capture root was not canonicalized", err)
			}
			if !existing {
				for _, path := range []string{"missing-one", filepath.Join("missing-one", "missing-two"), suffix} {
					info, err := os.Lstat(filepath.Join(real, path))
					if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Mode()&os.ModeSymlink != 0 {
						t.Fatal("new component was not private", path, err)
					}
				}
			}
			at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			input := CaptureRecord{Capture: reviewprocess.Capture{Stdout: []byte("literal <capture> 雪\x00"), Stderr: []byte("retained diagnostic"), ExitCode: 1, Started: true}, StartedAt: at, FinishedAt: at.Add(time.Second)}
			ref, err := store.Save(t.Context(), "attempt-alias", input)
			if err != nil {
				t.Fatal(err)
			}
			actual, readRef, err := store.Read(t.Context(), "attempt-alias")
			if err != nil || !reflect.DeepEqual(actual, input) || ref != readRef {
				t.Fatal("canonical alias roundtrip changed capture", err)
			}
			info, err := os.Lstat(filepath.Join(canonical, "attempt-alias.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("capture privacy changed", err)
			}
		})
	}
}
func TestCaptureAncestorAliasRetargetCannotRedirectStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix ancestor alias fixture")
	}
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	for _, root := range []string{first, second} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(first, alias); err != nil {
		t.Fatal(err)
	}
	store, err := NewCaptureStore(filepath.Join(alias, "captures"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	input := CaptureRecord{Capture: reviewprocess.Capture{Stdout: []byte("original"), Stderr: []byte{}, Started: true}, StartedAt: at, FinishedAt: at.Add(time.Second)}
	ref, err := store.Save(t.Context(), "existing", input)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewCaptureStore(filepath.Join(second, "captures"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Save(t.Context(), "existing", CaptureRecord{Capture: reviewprocess.Capture{Stdout: []byte("redirected"), Started: true}, StartedAt: at, FinishedAt: at.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(second, alias); err != nil {
		t.Fatal(err)
	}
	actual, readRef, err := store.Read(t.Context(), "existing")
	if err != nil || !reflect.DeepEqual(actual, input) || ref != readRef {
		t.Fatal("alias retarget redirected retained read", err)
	}
	if _, err = store.Save(t.Context(), "new", input); err != nil {
		t.Fatal("alias retarget broke canonical save", err)
	}
	if _, err = os.Lstat(filepath.Join(first, "captures", "new.json")); err != nil {
		t.Fatal("new capture did not stay pinned", err)
	}
	if _, err = os.Lstat(filepath.Join(second, "captures", "new.json")); !os.IsNotExist(err) {
		t.Fatal("retarget received a capture")
	}
}
func TestCaptureAncestorRootRemainsStrict(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix symlink/mode fixture")
	}
	for _, kind := range []string{"root-symlink", "dangling-root", "dangling-parent", "root-file", "wrong-root-mode"} {
		t.Run(kind, func(t *testing.T) {
			parent := t.TempDir()
			real := filepath.Join(parent, "real")
			if err := os.Mkdir(real, 0755); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(parent, "alias")
			if err := os.Symlink(real, alias); err != nil {
				t.Fatal(err)
			}
			requested := filepath.Join(alias, "captures")
			target := filepath.Join(real, "target")
			switch kind {
			case "root-symlink":
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, requested); err != nil {
					t.Fatal(err)
				}
			case "dangling-root":
				if err := os.Symlink(target, requested); err != nil {
					t.Fatal(err)
				}
			case "dangling-parent":
				if err := os.Symlink(target, filepath.Join(real, "dangling")); err != nil {
					t.Fatal(err)
				}
				requested = filepath.Join(alias, "dangling", "missing", "captures")
			case "root-file":
				if err := os.WriteFile(requested, []byte("existing file"), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-root-mode":
				if err := os.Mkdir(requested, 0755); err != nil {
					t.Fatal(err)
				}
			}
			_, err := NewCaptureStore(requested)
			requireCaptureError(t, err, "configuration_invalid")
			if kind == "dangling-root" || kind == "dangling-parent" {
				if _, err = os.Lstat(target); !os.IsNotExist(err) {
					t.Fatal("invalid alias created target components")
				}
			}
			if kind == "wrong-root-mode" {
				info, err := os.Stat(requested)
				if err != nil || info.Mode().Perm() != 0755 {
					t.Fatal("invalid root permissions changed")
				}
			}
			if kind == "root-file" {
				b, err := os.ReadFile(requested)
				if err != nil || string(b) != "existing file" {
					t.Fatal("existing root file changed")
				}
			}
		})
	}
}
