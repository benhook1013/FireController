package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestAuditNotesAndFindingBatchFileDiagnosticsStayLocal(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "audit-file-diagnostics")
	worker := f.worker(t, project, "Builder")
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	for _, spec := range []struct {
		args  []string
		flag  string
		limit int
	}{
		{[]string{"notes", "add", "--key", "PRIVATE_KEY"}, "--body-file", 16 << 10},
		{[]string{"notes", "update", "note-1", "--expect-revision", "1", "--key", "PRIVATE_KEY"}, "--body-file", 16 << 10},
		{[]string{"reviews", "decide-batch", cliEvidenceID, "--key", "PRIVATE_KEY"}, "--file", client.ReviewBatchMaxBytes},
		{[]string{"reviews", "fix-batch", cliEvidenceID, "--key", "PRIVATE_KEY"}, "--file", client.ReviewBatchMaxBytes},
	} {
		for _, cause := range []string{"missing", "permissions", "oversized", "unreadable"} {
			t.Run(spec.args[0]+"-"+spec.args[1]+"/"+cause, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "PRIVATE_INPUT")
				body := "PRIVATE_CONTENT π\n"
				mode := os.FileMode(0600)
				if cause == "oversized" {
					body = strings.Repeat("x", spec.limit+1)
				}
				if cause == "permissions" {
					if runtime.GOOS == "windows" {
						t.Skip("POSIX admission")
					}
					mode = 0644
				}
				if cause == "unreadable" {
					if runtime.GOOS == "windows" {
						t.Skip("POSIX read denial")
					}
					mode = 0000
				}
				if cause != "missing" {
					if err := os.WriteFile(path, []byte(body), mode); err != nil {
						t.Fatal(err)
					}
				}
				if cause == "unreadable" {
					if _, err := os.ReadFile(path); err == nil {
						t.Skip("current identity can read mode-zero files")
					}
				}
				args := append(append([]string{}, spec.args...), spec.flag, path)
				for _, profileFile := range []string{profiles, filepath.Join(t.TempDir(), "PRIVATE_MISSING_PROFILE")} {
					code, out, stderr := mutationInvoke(t, profileFile, "builder", args...)
					var result struct {
						Error struct {
							Code, Message, Action string
							OutcomeUnknown        bool `json:"outcome_unknown"`
						}
					}
					if code != cli.ExitUsage || out != "" || json.Unmarshal([]byte(stderr), &result) != nil || result.Error.Code != "invalid_arguments" || result.Error.OutcomeUnknown {
						t.Fatalf("local diagnostic: %d %s %s", code, out, stderr)
					}
					want := "does not exist"
					switch cause {
					case "permissions":
						want = "unsafe permissions"
					case "oversized":
						want = fmt.Sprintf("%d bytes; the limit is %d bytes", spec.limit+1, spec.limit)
					case "unreadable":
						want = "could not be read"
					}
					if !strings.Contains(result.Error.Message, spec.flag) || !strings.Contains(result.Error.Message, want) || !strings.Contains(result.Error.Action, "No request was sent") {
						t.Fatalf("missing cause/remedy: %s", stderr)
					}
					if cause == "permissions" && (!strings.Contains(result.Error.Action, "chmod 600") || !strings.Contains(result.Error.Action, "original --key")) {
						t.Fatalf("unsafe recovery guidance: %s", stderr)
					}
					for _, private := range []string{"PRIVATE_", path, profileFile, "permission denied", "no such file"} {
						if strings.Contains(stderr, private) {
							t.Fatalf("private input leaked: %s", stderr)
						}
					}
				}
				if cause != "missing" && cause != "unreadable" {
					data, err := os.ReadFile(path)
					info, statErr := os.Stat(path)
					if err != nil || statErr != nil || string(data) != body || info.Mode().Perm() != mode {
						t.Fatal("rejection changed input")
					}
				}
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("local admission sent %d requests", requests.Load())
	}
}

func TestAuditNoteFileCorrectionKeepsExactContentsAndOriginalKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX admission")
	}
	f := setupServiceFixture(t)
	project := f.project(t, "audit-note-correction")
	worker := f.worker(t, project, "Builder")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	const body = "  PRIVATE note π\n\nExact bytes\t\n"
	path := privateTextFixture(t, body)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	args := []string{"notes", "add", "--body-file", path, "--key", "retained-note-key"}
	code, out, stderr := mutationInvoke(t, profiles, "builder", args...)
	if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "chmod 600") {
		t.Fatalf("permission diagnostic: %d %s %s", code, out, stderr)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	created := mutationSuccess(t, profiles, "builder", args...)
	replay := mutationSuccess(t, profiles, "builder", args...)
	inline := mutationSuccess(t, profiles, "builder", "notes", "add", "--body", body, "--key", "retained-note-key")
	if fmt.Sprint(created) != fmt.Sprint(replay) || fmt.Sprint(created) != fmt.Sprint(inline) {
		t.Fatal("correction changed action identity")
	}
	note := f.call(t, "GET", "/api/v1/projects/"+project+"/notes/"+created["id"].(string), f.admin, "", nil)
	if note["body"] != body || note["revision"] != float64(1) {
		t.Fatal("correction lost bytes or replay duplicated mutation")
	}
}
