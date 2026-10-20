package cli_test

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestJobSummaryCLIExplicitReadWriteFileAndClear(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "summary-cli")
	worker := f.worker(t, project, "Builder")
	job := f.call(t, "POST", "/api/v1/projects/"+project+"/jobs", f.admin, worker, map[string]any{"title": "Private title", "brief": "Private standing instructions"})["id"].(string)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	profile := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, err := mutationInvoke(t, profile, "builder", args...)
		if code != cli.ExitSuccess || err != "" {
			t.Fatalf("summaryCLI%d %s %s", code, out, err)
		}
		return out
	}
	var state client.JobShareableSummary
	if json.Unmarshal([]byte(invoke("jobs", "summary", job)), &state) != nil || state.Revision != 0 || state.Title != "" || state.Body != "" {
		t.Fatal(state)
	}
	file := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(file, []byte("Authored **Markdown** 雪\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"jobs", "summary-set", job, "--expect-revision", "1", "--expect-summary-revision", "0", "--title", "Explicit title", "--body-file", file, "--key", "summary-once"}
	var receipt, replay client.JobSummaryReceipt
	out := invoke(args...)
	if strings.Contains(out, "Markdown") || json.Unmarshal([]byte(out), &receipt) != nil || receipt.Revision != 1 {
		t.Fatal(out)
	}
	if json.Unmarshal([]byte(invoke(args...)), &replay) != nil || replay != receipt {
		t.Fatal("summary replay changed")
	}
	if json.Unmarshal([]byte(invoke("jobs", "summary", job)), &state) != nil || state.Title != "Explicit title" || state.Body != "Authored **Markdown** 雪\n" {
		t.Fatal(state)
	}
	invoke("jobs", "summary-set", job, "--expect-revision", "1", "--expect-summary-revision", "1", "--title=", "--body=", "--key", "clear")
	if json.Unmarshal([]byte(invoke("jobs", "summary", job)), &state) != nil || state.Revision != 2 || state.Title != "" || state.Body != "" {
		t.Fatal(state)
	}
	code, out, diagnostic := mutationInvoke(t, profile, "builder", "jobs", "summary-set", job, "--expect-revision", "1", "--expect-summary-revision", "1", "--title", "Stale", "--body", "Body", "--key", "stale")
	if code != cli.ExitConflict || out != "" || !strings.Contains(diagnostic, "summary_revision_conflict") {
		t.Fatal(code, out, diagnostic)
	}
	if f.resolverHit.Load() != 1 {
		t.Fatal("summary performed provider work")
	}
}

func TestJobSummaryCLIInvalidArgumentsBeforeProfile(t *testing.T) {
	base := []string{"jobs", "summary-set", "job-one", "--expect-revision", "1", "--expect-summary-revision", "0", "--title", "Title", "--body", "Body", "--key", "key"}
	cases := [][]string{{"jobs", "summary"}, {"jobs", "summary", "../escape"}, {"jobs", "summary", "job-one", "--title", "x"}, append(append([]string{}, base...), "--key", "other"), {"jobs", "summary-set", "job-one", "--expect-revision", "1", "--expect-summary-revision", "0", "--title", "Title", "--key", "key"}}
	for _, pair := range [][2]string{{"1", "0"}, {"0", "-1"}, {"0", "9223372036854775807"}} {
		v := append([]string{}, base...)
		if pair[0] == "1" {
			v[4] = "0"
		} else {
			v[6] = pair[1]
		}
		cases = append(cases, v)
	}
	tooBig := append([]string{}, base...)
	tooBig[10] = strings.Repeat("雪", 667)
	cases = append(cases, tooBig)
	for _, args := range cases {
		code, out, err := mutationInvoke(t, "missing-summary-profiles", "builder", args...)
		if code != cli.ExitUsage || out != "" || !strings.Contains(err, "invalid_arguments") {
			t.Fatal(args, code, out, err)
		}
	}
}
