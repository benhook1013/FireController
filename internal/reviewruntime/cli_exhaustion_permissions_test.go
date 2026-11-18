package reviewruntime

import (
	"errors"
	"github.com/benhook1013/FireController/internal/core"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCLIExhaustionRestrictiveModeIsUnavailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics")
	}
	root := t.TempDir()
	source, err := NewCLIExhaustionStore(filepath.Join(root, "cli"), root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(source.root, 0700); err != nil {
		t.Fatal(err)
	}
	id := "review_attempt_01234567890123456789012345678901"
	path := filepath.Join(source.root, id+".json")
	if err = os.WriteFile(path, []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0000, 0200, 0400, 0600, 0644} {
		if err = os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, found, err := source.LoadCLIExhaustion(t.Context(), id)
		want := "capture_unavailable"
		if mode == 0600 || mode == 0644 {
			want = "validation_failed"
		}
		var failure *core.Error
		if found || !errors.As(err, &failure) || failure.Code != want {
			t.Fatalf("mode %o: found=%v err=%v want=%s", mode, found, err, want)
		}
	}
}
