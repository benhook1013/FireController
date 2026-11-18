package reviewprocess

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureProvesExitDespiteInvalidOutputOrTimeout(t *testing.T) {
	for _, script := range []string{"echo invalid; exit 0", "exit 7", "sleep 30"} {
		t.Run(script, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "process")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			r := &Runner{executable: path, now: time.Now}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			result := r.run(ctx, dir, nil, 1024, 1024)
			if !result.capture.Started || !result.capture.Exited {
				t.Fatalf("exit not proven: %+v", result.capture)
			}
		})
	}
	r := &Runner{executable: "/missing/executable", now: time.Now}
	result := r.run(context.Background(), t.TempDir(), nil, 1024, 1024)
	if result.capture.Exited || result.capture.Started {
		t.Fatal("failed start claims exit")
	}
}
