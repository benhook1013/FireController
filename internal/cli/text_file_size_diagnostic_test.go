package cli_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/cli"
)

func TestPrivateTextFileSizeDiagnosticReportsExactBytesBeforeProfileAccess(t *testing.T) {
	missingProfiles := filepath.Join(t.TempDir(), "PRIVATE_MISSING_PROFILE.json")
	for _, spec := range []struct {
		flag  string
		limit int
		args  []string
	}{
		{"--brief-file", 32768, []string{"jobs", "create", "--title", "Job", "--key", "size-key"}},
		{"--brief-file", 32768, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "size-key"}},
		{"--body-file", 16384, []string{"inbox", "send", "--to", "worker-1", "--key", "size-key"}},
		{"--progress-file", 8192, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "size-key"}},
		{"--next-file", 8192, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "size-key"}},
		{"--blocker-file", 4096, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "size-key"}},
	} {
		for _, extra := range []int{1, 915} {
			for _, multibyte := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s-%s/%s/%d/multibyte-%t", spec.args[0], spec.args[1], spec.flag, extra, multibyte), func(t *testing.T) {
					size := spec.limit + extra
					const marker = "PRIVATE_SIZE_CONTENT "
					body := marker + strings.Repeat("x", size-len(marker))
					if multibyte {
						body = marker + strings.Repeat("π", (size-len(marker))/2) + strings.Repeat("x", (size-len(marker))%2)
						if utf8.RuneCountInString(body) >= spec.limit {
							t.Fatal("fixture must distinguish UTF-8 bytes from character count")
						}
					}
					if !utf8.ValidString(body) || len(body) != size {
						t.Fatal("wrong byte-count fixture")
					}
					path := privateTextFixture(t, body)
					args := append(append([]string{}, spec.args...), spec.flag, path)
					code, out, stderr := mutationInvoke(t, missingProfiles, "builder", args...)
					var result struct {
						Error struct {
							Code    string `json:"code"`
							Message string `json:"message"`
							Action  string `json:"action"`
						} `json:"error"`
					}
					if code != cli.ExitUsage || out != "" || json.Unmarshal([]byte(stderr), &result) != nil || result.Error.Code != "invalid_arguments" {
						t.Fatalf("oversized input reached profile access or lost usage diagnostic: %d %s %s", code, out, stderr)
					}
					if !strings.Contains(result.Error.Message, spec.flag) || !strings.Contains(result.Error.Message, fmt.Sprintf("%d bytes", size)) || !strings.Contains(result.Error.Message, fmt.Sprintf("%d bytes", spec.limit)) || !strings.Contains(result.Error.Action, "UTF-8 bytes") || !strings.Contains(result.Error.Action, "No request was sent") {
						t.Fatalf("size diagnostic lacks exact flag/byte counts or correction: %s", stderr)
					}
					for _, private := range []string{path, missingProfiles, marker, "PRIVATE_", "size-key"} {
						if strings.Contains(stderr, private) {
							t.Fatal("size diagnostic disclosed a private path, content or key")
						}
					}
					data, err := os.ReadFile(path)
					if err != nil || string(data) != body {
						t.Fatal("size rejection changed the file", err)
					}
				})
			}
		}
	}
}

func TestPrivateTextFileSizeDiagnosticAcceptsExactUTF8ByteBoundaries(t *testing.T) {
	for _, spec := range []struct {
		flag  string
		limit int
		args  []string
	}{
		{"--brief-file", 32768, []string{"jobs", "create", "--title", "Job", "--key", "boundary-key"}},
		{"--brief-file", 32768, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "boundary-key"}},
		{"--body-file", 16384, []string{"inbox", "send", "--to", "worker-1", "--key", "boundary-key"}},
		{"--progress-file", 8192, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "boundary-key"}},
		{"--next-file", 8192, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "boundary-key"}},
		{"--blocker-file", 4096, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "boundary-key"}},
	} {
		t.Run(spec.args[0]+"-"+spec.args[1]+"/"+spec.flag, func(t *testing.T) {
			body := strings.Repeat("π", spec.limit/2)
			if len(body) != spec.limit {
				t.Fatal("wrong boundary fixture")
			}
			args := append(append([]string{}, spec.args...), spec.flag, privateTextFixture(t, body))
			code, out, stderr := run(args, nil)
			// Reaching the required-profile error proves admission succeeded
			// without reading a profile or sending a service request.
			if code != cli.ExitConfig || out != "" || !strings.Contains(stderr, "profile_required") {
				t.Fatalf("valid byte-boundary input was rejected: %d %s %s", code, out, stderr)
			}
		})
	}
}
