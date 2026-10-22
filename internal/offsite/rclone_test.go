package offsite

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOffsiteProcessOutputAndCancellationAreBoundedAndPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process-group fixture")
	}
	for _, kind := range []string{"output-limit", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			work := t.TempDir()
			script := "#!/bin/sh\nprintf 'PRIVATE_PROVIDER_TOKEN_SENTINEL' >&2\n"
			if kind == "output-limit" {
				script += "while :; do printf '%08000d' 1; done\n"
			} else {
				script += "sleep 30 &\nwait\n"
			}
			if err := os.WriteFile(f.c.RcloneBinary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.c.RcloneConfig, []byte("private configuration"), 0600); err != nil {
				t.Fatal(err)
			}
			tr, err := newTransport(f.c, work)
			if err != nil {
				t.Fatal(err)
			}
			deadline := 5 * time.Second
			if kind == "cancellation" {
				deadline = 30 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), deadline)
			defer cancel()
			started := time.Now()
			_, err = tr.read(ctx, "cloud:fixture/receipt.json")
			code := "offsite_output_limit"
			if kind == "cancellation" {
				code = "offsite_cancelled"
			}
			wantCode(t, err, code)
			if strings.Contains(err.Error(), "PRIVATE_PROVIDER_TOKEN_SENTINEL") || time.Since(started) > 3*time.Second {
				t.Fatal("unbounded process or leaked provider output", err)
			}
			entries, _ := os.ReadDir(work)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), "private-") {
					t.Fatal("unexpected provider log")
				}
			}
		})
	}
}

func TestOffsiteConfigAndStatusRejectMalformedPrivateState(t *testing.T) {
	f := newFixture(t)
	f.bundle(t)
	if _, err := push(t.Context(), f.c, memory()); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "config.json")
	if err := writeJSON(config, f.c); err != nil {
		t.Fatal(err)
	}
	report, err := ReadStatus(t.Context(), config)
	if err != nil || report.Retained != 1 || report.LastSuccess == nil {
		t.Fatal(report, err)
	}
	// No credential file or executable exists in this fixture. Status still
	// reads just the local facts; neither is required or opened.
	for _, body := range []string{`{}`, `{"version":1,"binding":"changed","owned":[]}`, `{"version":1,"version":1}`, `{"version":null}`} {
		if err := os.WriteFile(filepath.Join(f.c.StateDir, "offsite.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err = ReadStatus(t.Context(), config)
		wantCode(t, err, "offsite_state_invalid")
	}
}
