package cli_test

import (
	"encoding/json"
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

func TestPrivateTextFileAccessDiagnosticBeforeProfileAndNetwork(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "file-access-diagnostic")
	worker := f.worker(t, project, "Builder")
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	missingProfiles := filepath.Join(t.TempDir(), "PRIVATE_MISSING_PROFILE.json")
	for _, spec := range []struct {
		flag string
		args []string
	}{
		{"--brief-file", []string{"jobs", "create", "--title", "Job", "--key", "PRIVATE_ORIGINAL_KEY"}},
		{"--brief-file", []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "PRIVATE_ORIGINAL_KEY"}},
		{"--progress-file", []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "PRIVATE_ORIGINAL_KEY"}},
		{"--next-file", []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "PRIVATE_ORIGINAL_KEY"}},
		{"--blocker-file", []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "PRIVATE_ORIGINAL_KEY"}},
		{"--body-file", []string{"inbox", "send", "--to", "worker-1", "--key", "PRIVATE_ORIGINAL_KEY"}},
	} {
		for _, kind := range []string{"missing", "unreadable"} {
			t.Run(spec.args[0]+"-"+spec.args[1]+"/"+spec.flag+"/"+kind, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "PRIVATE_INPUT.md")
				const body = "PRIVATE_INPUT_CONTENT π\n"
				if kind == "unreadable" {
					if runtime.GOOS == "windows" {
						t.Skip("POSIX unreadable-file fixture")
					}
					if err := os.WriteFile(path, []byte(body), 0000); err != nil {
						t.Fatal(err)
					}
					if _, err := os.ReadFile(path); err == nil {
						t.Skip("this identity can read mode-zero files")
					}
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
						t.Fatalf("file access failure lost local usage diagnostic: %d %s %s", code, out, stderr)
					}
					cause := "does not exist"
					if kind == "unreadable" {
						cause = "could not be read"
					}
					if !strings.Contains(result.Error.Message, spec.flag) || !strings.Contains(result.Error.Message, cause) || !strings.Contains(result.Error.Action, "original --key") || !strings.Contains(result.Error.Action, "No request was sent") {
						t.Fatalf("file diagnostic omits the flag, cause or safe retry: %s", stderr)
					}
					for _, private := range []string{path, profileFile, body, "PRIVATE_", "permission denied", "no such file"} {
						if strings.Contains(stderr, private) {
							t.Fatal("file diagnostic disclosed private input or raw OS error")
						}
					}
				}
				if kind == "missing" {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatal("local rejection created its missing input")
					}
				} else {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0 {
						t.Fatal("local rejection changed input permissions")
					}
					if err := os.Chmod(path, 0600); err != nil {
						t.Fatal(err)
					}
					data, err := os.ReadFile(path)
					if err != nil || string(data) != body {
						t.Fatal("local rejection changed input contents")
					}
				}
			})
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("local file failures sent %d requests", requests.Load())
	}
}
