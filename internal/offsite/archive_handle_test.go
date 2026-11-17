package offsite

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Replace the path on the first actual stream read, after the archive has
// already been opened and checked. This exercises the handoff deterministically
// without a concurrent filesystem race or a production test hook.
type archivePathReplacement struct {
	context.Context
	archive, replacement string
	replaced             bool
	err                  error
}

func (c *archivePathReplacement) Err() error {
	if !c.replaced {
		var pcs [32]uintptr
		n := runtime.Callers(2, pcs[:])
		frames := runtime.CallersFrames(pcs[:n])
		stream := false
		for {
			frame, more := frames.Next()
			stream = stream || strings.HasSuffix(frame.Function, "/offsite.(*contextReader).Read")
			if !more {
				break
			}
		}
		if stream {
			c.replaced = true
			c.err = os.Rename(c.replacement, c.archive)
		}
	}
	return c.Context.Err()
}

func TestOffsiteUnpackExtractsHashedDescriptorAfterPathReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("replacing an open file requires POSIX rename semantics")
	}
	f := newFixture(t)
	bundle := f.bundle(t)
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive.tar.gz")
	receipt, err := pack(t.Context(), bundle.Path, archive, f.c)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement.tar.gz")
	const unverified = "unverified replacement archive"
	if err := os.WriteFile(replacement, []byte(unverified), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := &archivePathReplacement{Context: t.Context(), archive: archive, replacement: replacement}
	destination := t.TempDir()
	got, err := unpack(ctx, archive, destination, receipt, f.c)
	if !ctx.replaced || ctx.err != nil {
		t.Fatal("archive path was not replaced at the real read boundary", ctx.replaced, ctx.err)
	}
	if err != nil || got.ID != bundle.ID || got.EventSequence != bundle.EventSequence {
		t.Fatal("unpack did not extract the original receipt-bound descriptor", got, err)
	}
	bytes, err := os.ReadFile(archive)
	if err != nil || string(bytes) != unverified {
		t.Fatal("unpack modified or removed the replacement pathname", err)
	}
	// A separate request sees and refuses the replacement, rather than reusing
	// the original descriptor or treating its earlier validation as path proof.
	_, err = unpack(t.Context(), archive, t.TempDir(), receipt, f.c)
	wantCode(t, err, "offsite_archive_invalid")
}
