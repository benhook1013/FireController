package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestQuarantineOperatorRejectsMissingCustodyAndRepeatedFlagsBeforeSource(t *testing.T) {
	for _, command := range []string{"backup-reconcile-empty-quarantine", "backup-reconcile-readback"} {
		t.Run(command, func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "must-not-create")
			var out, errout bytes.Buffer
			code := run(context.Background(), []string{command, "--data-dir", data}, &out, &errout)
			if code == 0 || out.Len() != 0 {
				t.Fatal(code, out.String())
			}
			if _, err := os.Lstat(data); !os.IsNotExist(err) {
				t.Fatal("source created")
			}
			out.Reset()
			errout.Reset()
			code = run(context.Background(), []string{command, "--data-dir", data, "--data-dir", data}, &out, &errout)
			if code != 2 {
				t.Fatal("duplicate flags accepted", code)
			}
		})
	}
}
