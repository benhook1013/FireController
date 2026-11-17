package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func privateTextFixture(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "PRIVATE_TEXT.md")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrivateTextFilesPreserveBytesAndReplayAcrossInlineForms(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "cli-private-text")
	sender, recipient := f.worker(t, project, "Builder"), f.worker(t, project, "Overseer")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, sender}})
	providers := f.resolverHit.Load()
	brief := "  # PRIVATE brief\n\nExact π text\t\n"
	briefFile := privateTextFixture(t, brief)
	created := mutationSuccess(t, profiles, "builder", "jobs", "create", "--title", "Job", "--brief-file", briefFile, "--key", "create-file")
	replayed := mutationSuccess(t, profiles, "builder", "jobs", "create", "--title", "Job", "--brief", brief, "--key", "create-file")
	if fmt.Sprint(created) != fmt.Sprint(replayed) {
		t.Fatal("file/inline create changed request identity")
	}
	job := created["id"].(string)
	body := "  # PRIVATE message\n\nExact π text\t\n--help\n"
	sent := mutationSuccess(t, profiles, "builder", "inbox", "send", "--to", recipient, "--body-file", privateTextFixture(t, body), "--key", "message-file")
	again := mutationSuccess(t, profiles, "builder", "inbox", "send", "--to", recipient, "--body", body, "--key", "message-file")
	if fmt.Sprint(sent) != fmt.Sprint(again) {
		t.Fatal("file/inline send changed request identity")
	}
	thread := f.call(t, "GET", "/api/v1/projects/"+project+"/conversations/"+sent["conversation_id"].(string), f.admin, "", nil)
	items := thread["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["body"] != body {
		t.Fatal("message bytes changed or replay duplicated")
	}
	progress, next, blocker := "  Working π\n", "\nNext step\t\n", "  Waiting\n"
	updatedBrief := brief + "\nMore details\n"
	fileArgs := []string{"jobs", "update", job, "--expect-revision", "1", "--key", "edit-file", "--brief-file", privateTextFixture(t, updatedBrief), "--progress-file", privateTextFixture(t, progress), "--next-file", privateTextFixture(t, next), "--blocker-file", privateTextFixture(t, blocker)}
	edit := mutationSuccess(t, profiles, "builder", fileArgs...)
	inlineArgs := []string{"jobs", "update", job, "--expect-revision", "1", "--key", "edit-file", "--brief", updatedBrief, "--progress", progress, "--next", next, "--blocker", blocker}
	replay := mutationSuccess(t, profiles, "builder", inlineArgs...)
	if fmt.Sprint(edit) != fmt.Sprint(replay) {
		t.Fatal("file/inline patch changed request identity")
	}
	code, out, err := mutationInvoke(t, profiles, "builder", "jobs", "read", job)
	var current client.Job
	if code != cli.ExitSuccess || err != "" || json.Unmarshal([]byte(out), &current) != nil || current.Brief != updatedBrief || current.Progress.Summary != progress || current.Progress.Next != next || current.Progress.Blocker != blocker {
		t.Fatalf("file patch lost bytes: %d %s %s", code, out, err)
	}
	empty := privateTextFixture(t, "")
	mutationSuccess(t, profiles, "builder", "jobs", "update", job, "--expect-revision", "2", "--key", "clear-files", "--brief-file", empty, "--progress-file", empty, "--next-file", empty, "--blocker-file", empty)
	state := f.call(t, "GET", "/api/v1/projects/"+project+"/jobs/"+job, f.admin, "", nil)
	p := state["progress"].(map[string]any)
	if state["brief"] != "" || p["summary"] != "" || p["next"] != "" || p["blocker"] != "" {
		t.Fatal("empty files did not clear")
	}
	blank := mutationSuccess(t, profiles, "builder", "jobs", "create", "--title", "Empty brief", "--brief-file", empty, "--key", "empty-create")
	if blank["outcome"] != "created" {
		t.Fatal("empty brief create refused")
	}
	if f.resolverHit.Load() != providers {
		t.Fatal("text-file operations performed provider work")
	}
}

func TestPrivateTextFilesRejectInvalidInputBeforeProfileAccess(t *testing.T) {
	invalidUTF8 := privateTextFixture(t, string([]byte{0xff}))
	empty := privateTextFixture(t, "")
	whitespace := privateTextFixture(t, " \n\t")
	missing := filepath.Join(t.TempDir(), "PRIVATE_MISSING.md")
	cases := [][]string{
		{"inbox", "send", "--to", "worker-1", "--body-file", empty, "--key", "k"},
		{"inbox", "send", "--to", "worker-1", "--body-file", whitespace, "--key", "k"},
		{"inbox", "send", "--to", "worker-1", "--body-file", invalidUTF8, "--key", "k"},
		{"jobs", "create", "--title", "Job", "--brief-file", invalidUTF8, "--key", "k"},
		{"jobs", "create", "--title", "Job", "--brief-file", missing, "--key", "k"},
		{"jobs", "create", "--title", "Job", "--brief-file=", "--key", "k"},
		{"inbox", "send", "--to", "worker-1", "--body", "inline", "--body-file", missing, "--key", "k"},
		{"jobs", "create", "--title", "Job", "--brief", "", "--brief-file", missing, "--key", "k"},
	}
	for _, flag := range []string{"brief", "progress", "next", "blocker"} {
		cases = append(cases, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "k", "--" + flag, "", "--" + flag + "-file", missing})
		cases = append(cases, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "k", "--" + flag + "-file", invalidUTF8})
		cases = append(cases, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "k", "--" + flag + "-file", missing, "--" + flag + "-file", missing})
	}
	for _, args := range cases {
		t.Run(strings.Join(args[:2], "-")+fmt.Sprint(len(args)), func(t *testing.T) {
			code, out, err := run(append([]string{"--profiles-file", "/MISSING_PRIVATE_PROFILE"}, args...), nil)
			if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") || strings.Contains(err, "PRIVATE_") {
				t.Fatalf("invalid file leaked/reached profile: %d %s %s", code, out, err)
			}
		})
	}
}

func TestPrivateTextFileBoundsAndFileTypes(t *testing.T) {
	for _, spec := range []struct {
		flag  string
		limit int
	}{{"body", 16 << 10}, {"brief", 32 << 10}, {"progress", 8 << 10}, {"next", 8 << 10}, {"blocker", 4 << 10}} {
		for _, size := range []int{spec.limit, spec.limit + 1} {
			file := privateTextFixture(t, strings.Repeat("x", size))
			var args []string
			switch spec.flag {
			case "body":
				args = []string{"inbox", "send", "--to", "worker-1", "--key", "k", "--body-file", file}
			case "brief":
				args = []string{"jobs", "create", "--title", "Job", "--key", "k", "--brief-file", file}
			default:
				args = []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "k", "--" + spec.flag + "-file", file}
			}
			code, _, err := run(args, nil)
			want := cli.ExitConfig
			if size > spec.limit {
				want = cli.ExitUsage
			}
			if code != want {
				t.Fatalf("%s size %d: %d %s", spec.flag, size, code, err)
			}
		}
	}
	// An empty file must not consume budget while the remaining inline fields
	// exactly fill the combined limit.
	code, _, err := run([]string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "k", "--progress", strings.Repeat("p", 8<<10), "--next", strings.Repeat("n", 8<<10), "--blocker-file", privateTextFixture(t, "")}, nil)
	if code != cli.ExitConfig {
		t.Fatalf("valid empty file falsely consumed combined budget: %d %s", code, err)
	}
	// Partial progress still obeys the combined 16 KiB validator before connection.
	failure(t, []string{"jobs", "update", "job-1", "--expect-revision", "1", "--key", "k", "--progress-file", privateTextFixture(t, strings.Repeat("p", 8<<10)), "--next-file", privateTextFixture(t, strings.Repeat("n", 8<<10)), "--blocker-file", privateTextFixture(t, "b")}, nil, cli.ExitUsage, "invalid_arguments")
	good := privateTextFixture(t, "Private text")
	paths := []string{t.TempDir()}
	if runtime.GOOS != "windows" {
		public := privateTextFixture(t, "PRIVATE_TEXT")
		if err := os.Chmod(public, 0644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, public)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(good, link); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, link, "/dev/null")
		if command, err := exec.LookPath("mkfifo"); err == nil {
			fifo := filepath.Join(t.TempDir(), "fifo")
			if err := exec.Command(command, fifo).Run(); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, fifo)
		}
	}
	for _, path := range paths {
		failure(t, []string{"--profiles-file", "/missing", "jobs", "create", "--title", "Job", "--key", "k", "--brief-file", path}, nil, cli.ExitUsage, "invalid_arguments")
	}
}

func TestPrivateTextFileOperationHelpIsOfflineAndDiscoverable(t *testing.T) {
	for _, operation := range []string{"jobs create", "jobs update", "inbox send"} {
		for _, args := range [][]string{append(strings.Fields(operation), "--help"), append([]string{"help"}, strings.Fields(operation)...)} {
			code, out, err := run(append([]string{"--profiles-file", "/missing/private-profiles"}, args...), nil)
			if code != cli.ExitSuccess || err != "" || len(out) > 4096 || !strings.Contains(out, "-file PATH") || !strings.Contains(out, "UTF-8") || strings.Contains(out, "/missing/private-profiles") {
				t.Fatalf("operation help unavailable or unbounded: %s %d %s %s", operation, code, out, err)
			}
		}
	}
}
