package offsite

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOffsiteDownloadPermissionFailureIsPrivateStorageError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX synthetic rclone executable and mode fixture")
	}
	for _, kind := range []string{"missing_destination", "success", "not_found_3", "not_found_4", "transport_failure"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			work := t.TempDir()
			destination := filepath.Join(work, "PRIVATE_DESTINATION_SENTINEL archive.tar.gz")
			script := "#!/bin/sh\nprintf 'PRIVATE_PROVIDER_ERROR' >&2\n"
			switch kind {
			case "success":
				// Deliberately create a nonprivate download so get must fix it.
				script += "while [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = copyto ]; then\n    [ \"$#\" -eq 4 ] && [ \"$4\" = --ignore-times ] || exit 7\n    umask 022\n    printf 'synthetic archive' > \"$3\"\n    exit 0\n  fi\n  shift\ndone\nexit 7\n"
			case "not_found_3", "not_found_4":
				code := 3
				if kind == "not_found_4" {
					code = 4
				}
				script += fmt.Sprintf("exit %d\n", code)
			case "transport_failure":
				script += "exit 7\n"
			default:
				// A successful transport with no destination exercises the actual
				// filesystem Chmod failure without hooks or permission assumptions.
				script += "exit 0\n"
			}
			if err := os.WriteFile(f.c.RcloneBinary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.c.RcloneConfig, []byte("private configuration"), 0600); err != nil {
				t.Fatal(err)
			}
			transport, err := newTransport(f.c, work)
			if err != nil {
				t.Fatal(err)
			}
			err = transport.get(t.Context(), "cloud:fixture/archive.tar.gz", destination)
			switch kind {
			case "success":
				if err != nil {
					t.Fatal("successful download was refused", err)
				}
				body, readErr := os.ReadFile(destination)
				info, statErr := os.Stat(destination)
				if readErr != nil || statErr != nil || string(body) != "synthetic archive" || info.Mode().Perm() != 0600 {
					t.Fatal("download content or private permissions changed", readErr, statErr)
				}
				return
			case "not_found_3", "not_found_4":
				if !errors.Is(err, notFound) {
					t.Fatal("transport absence was replaced by a chmod error", err)
				}
			case "transport_failure":
				wantCode(t, err, "offsite_transport_failed")
			default:
				wantCode(t, err, "offsite_storage_unavailable")
			}
			encoded, marshalErr := json.Marshal(err)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, private := range []string{destination, work, f.c.RcloneConfig, "PRIVATE_DESTINATION_SENTINEL", "PRIVATE_PROVIDER_ERROR"} {
				if strings.Contains(err.Error(), private) || strings.Contains(string(encoded), private) {
					t.Fatal("download failure exposed private filesystem/provider details")
				}
			}
			if _, statErr := os.Lstat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("failed download unexpectedly created a destination", statErr)
			}
		})
	}
}
