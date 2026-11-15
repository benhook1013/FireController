package backup

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHashPrivateSequentialBufferPreservesChecks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(dir, "first"), filepath.Join(dir, "second")}
	for i, p := range paths {
		if err := os.WriteFile(p, []byte{byte(i), 1, 2, 3}, 0600); err != nil {
			t.Fatal(err)
		}
	}
	buffer := make([]byte, 64<<10)
	for _, p := range paths {
		want, err := hashPrivate(t.Context(), p, 100)
		if err != nil {
			t.Fatal(err)
		}
		got, err := hashPrivateBuffer(t.Context(), p, 100, buffer)
		if err != nil || !reflect.DeepEqual(want, got) {
			t.Fatal(got, want, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := hashPrivateBuffer(ctx, paths[0], 100, buffer); err == nil {
		t.Fatal("cancellation accepted")
	}
	if _, err := hashPrivateBuffer(t.Context(), paths[0], 1, buffer); err == nil {
		t.Fatal("size bound ignored")
	}
	if err := os.Chmod(paths[0], 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := hashPrivateBuffer(t.Context(), paths[0], 100, buffer); err == nil {
		t.Fatal("public file accepted")
	}
}

func BenchmarkHashPrivateSequentialBuffer(b *testing.B) {
	dir := filepath.Join(b.TempDir(), "private")
	if err := os.Mkdir(dir, 0700); err != nil {
		b.Fatal(err)
	}
	p := filepath.Join(dir, "artifact")
	if err := os.WriteFile(p, make([]byte, 1024), 0600); err != nil {
		b.Fatal(err)
	}
	for _, reuse := range []bool{false, true} {
		name := "per_file"
		if reuse {
			name = "per_inspection"
		}
		b.Run(name, func(b *testing.B) {
			buffer := make([]byte, 64<<10)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				if reuse {
					_, err = hashPrivateBuffer(b.Context(), p, 2048, buffer)
				} else {
					_, err = hashPrivate(b.Context(), p, 2048)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
