package offsite

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOffsiteUploadAbsenceExitIsTransportFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX transport fixture")
	}
	for _, code := range []int{3, 4} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newFixture(t)
			f.bundle(t)
			script := fmt.Sprintf("#!/bin/sh\nprintf 'PRIVATE_UPLOAD_ERROR' >&2\nexit %d\n", code)
			if err := os.WriteFile(f.c.RcloneBinary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.c.RcloneConfig, []byte("synthetic configuration"), 0600); err != nil {
				t.Fatal(err)
			}
			tr, err := newTransport(f.c, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			err = tr.put(t.Context(), filepath.Join(t.TempDir(), "upload"), "cloud:fixture/archive.tar.gz")
			wantCode(t, err, "offsite_transport_failed")
			if errors.Is(err, notFound) || strings.Contains(err.Error(), "PRIVATE_UPLOAD_ERROR") {
				t.Fatal("upload error retained absence meaning or provider text", err)
			}
			if _, err = tr.read(t.Context(), "cloud:fixture/receipt.json"); !errors.Is(err, notFound) {
				t.Fatal("missing remote read changed meaning", err)
			}
			if err = tr.get(t.Context(), "cloud:fixture/archive.tar.gz", filepath.Join(t.TempDir(), "download")); !errors.Is(err, notFound) {
				t.Fatal("missing download changed meaning", err)
			}
			if err = tr.remove(t.Context(), "cloud:fixture/archive.tar.gz"); err != nil {
				t.Fatal("already absent deletion stopped succeeding", err)
			}
			_, err = push(t.Context(), f.c, nil)
			wantCode(t, err, "offsite_transport_failed")
			s := state(t, f.c)
			if s.LastAttempt == nil || s.LastAttempt.State != "failed" || s.LastAttempt.Reason != "offsite_transport_failed" || s.Pending == nil || s.LastSuccess != nil || len(s.Owned) != 0 {
				t.Fatal("upload failure lost safe diagnostic or original pending work", s)
			}
		})
	}
}
