//go:build linux

package buildinfo

import (
	"path/filepath"
	"runtime/debug"
	"syscall"
	"testing"
)

func TestLaunchTargetRefusesFIFOBeforeMetadataOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fcmcp")
	if err := syscall.Mkfifo(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, ok := launchTarget(path, func(string) (*debug.BuildInfo, error) { t.Fatal("FIFO reached metadata opener"); return nil, nil }); ok {
		t.Fatal("FIFO became launch target metadata")
	}
}
