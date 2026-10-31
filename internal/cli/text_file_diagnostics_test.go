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
)

func TestPrivateTextFilePermissionsDiagnosticBeforeProfileAndNetwork(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX private-file permission requirement")
	}
	f := setupServiceFixture(t)
	project := f.project(t, "private-file-diagnostic")
	worker := f.worker(t, project, "Builder")
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	missingProfiles := filepath.Join(t.TempDir(), "PRIVATE_MISSING_PROFILE.json")
	const body = "PRIVATE_FILE_CONTENT π\n"
	for _, spec := range []struct {
		flag string
		args []string
	}{
		{"--brief-file", []string{"jobs", "create", "--title", "Job", "--key", "original-key"}},
		{"--body-file", []string{"inbox", "send", "--to", "worker-1", "--key", "original-key"}},
		{"--brief-file", []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "original-key"}},
		{"--progress-file", []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "original-key"}},
		{"--next-file", []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "original-key"}},
		{"--blocker-file", []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "original-key"}},
	} {
		for _, mode := range []os.FileMode{0644, 0640, 0604, 0610, 0601, 0666} {
			t.Run(spec.args[0]+"-"+spec.args[1]+"-"+spec.flag+fmt.Sprintf("-%04o", mode), func(t *testing.T) {
				path := privateTextFixture(t, body)
				if err := os.Chmod(path, mode); err != nil {
					t.Fatal(err)
				}
				args := append(append([]string{}, spec.args...), spec.flag, path)
				for _, profileFile := range []string{missingProfiles, profiles} {
					code, out, stderr := mutationInvoke(t, profileFile, "builder", args...)
					var result struct {
						Error struct {
							Code    string `json:"code"`
							Message string `json:"message"`
							Action  string `json:"action"`
						} `json:"error"`
					}
					if code != cli.ExitUsage || out != "" || json.Unmarshal([]byte(stderr), &result) != nil || result.Error.Code != "invalid_arguments" {
						t.Fatalf("unexpected diagnostic: %d %s %s", code, out, stderr)
					}
					if !strings.Contains(result.Error.Message, spec.flag) || !strings.Contains(result.Error.Message, "permissions") || !strings.Contains(result.Error.Action, "0600") || !strings.Contains(result.Error.Action, "chmod 600") || !strings.Contains(result.Error.Action, "original --key") || !strings.Contains(result.Error.Action, "No request was sent") {
						t.Fatalf("permission guidance is not actionable: %s", stderr)
					}
					for _, private := range []string{path, profileFile, body, "PRIVATE_", "original-key"} {
						if strings.Contains(stderr, private) {
							t.Fatal("permission diagnostic exposed private input")
						}
					}
				}
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("rejection changed file permissions: %v", err)
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != body {
					t.Fatalf("rejection changed file contents: %v", err)
				}
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("rejected private-file input sent %d requests", requests.Load())
	}
}

func TestPrivateTextFilePermissionCorrectionPreservesOriginalKeyAndBytes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX private-file permission requirement")
	}
	f := setupServiceFixture(t)
	project := f.project(t, "private-file-correction")
	worker := f.worker(t, project, "Builder")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	created := mutationSuccess(t, profiles, "builder", "jobs", "create", "--title", "Job", "--key", "create")
	job := created["id"].(string)
	const progress = "  PRIVATE progress π\n\nExact bytes\t\n"
	path := privateTextFixture(t, progress)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	args := []string{"jobs", "update", job, "--expect-revision", "1", "--key", "original-update-key", "--progress-file", path}
	code, out, stderr := mutationInvoke(t, profiles, "builder", args...)
	if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "0600") {
		t.Fatalf("unsafe file accepted: %d %s %s", code, out, stderr)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	updated := mutationSuccess(t, profiles, "builder", args...)
	fileReplay := mutationSuccess(t, profiles, "builder", args...)
	inlineReplay := mutationSuccess(t, profiles, "builder", "jobs", "update", job, "--expect-revision", "1", "--key", "original-update-key", "--progress", progress)
	if fmt.Sprint(updated) != fmt.Sprint(fileReplay) || fmt.Sprint(updated) != fmt.Sprint(inlineReplay) || updated["revision"] != float64(2) {
		t.Fatal("permission correction changed request identity or replay")
	}
	state := f.call(t, "GET", "/api/v1/projects/"+project+"/jobs/"+job, f.admin, "", nil)
	if state["revision"] != float64(2) || state["progress"].(map[string]any)["summary"] != progress {
		t.Fatal("permission correction changed exact bytes or duplicated mutation")
	}
}
