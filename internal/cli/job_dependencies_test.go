package cli_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
)

func TestJobDependenciesCLITransport(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "dependency-cli")
	worker := f.worker(t, project, "Builder")
	type request struct {
		method, path, query, key, worker string
		body                             map[string]any
	}
	requests := make(chan request, 2)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			f.handler.ServeHTTP(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests <- request{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Idempotency-Key"), r.Header.Get("X-FC-Worker"), body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job-target","revision":8,"outcome":"updated"}`))
	}))
	defer service.Close()
	profiles := mutationProfiles(t, f, service.URL, map[string][2]string{"builder": {project, worker}})
	var maxFlags, maxIDs []string
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("job-%02d", i)
		maxFlags = append(maxFlags, "--blocked-by", id)
		maxIDs = append(maxIDs, id)
	}
	maxJSON, err := json.Marshal(maxIDs)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		flags []string
		body  string
	}{
		{"omitted", []string{"--next", "Continue"}, `{"expected_revision":7,"progress":{"next":"Continue"}}`},
		{"replace", []string{"--blocked-by", "job-first", "--blocked-by=job-second"}, `{"expected_revision":7,"blocked_by":["job-first","job-second"]}`},
		{"clear", []string{"--blocked-by", ""}, `{"expected_revision":7,"blocked_by":[]}`},
		{"clear-assigned", []string{"--blocked-by="}, `{"expected_revision":7,"blocked_by":[]}`},
		{"literal", []string{"--blocked-by", "job-first,job-second", "--blocked-by", "@jobs.json"}, `{"expected_revision":7,"blocked_by":["job-first,job-second","@jobs.json"]}`},
		{"maximum", maxFlags, `{"expected_revision":7,"blocked_by":` + string(maxJSON) + `}`},
		{"combined", []string{"--blocked-by", "job-first", "--evidence", "proof:one", "--status", "blocked"}, `{"expected_revision":7,"blocked_by":["job-first"],"status":"blocked","progress":{"evidence":["proof:one"]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"jobs", "update", "job-target", "--expect-revision", "7", "--key", "dependency-key"}, test.flags...)
			code, out, diagnostic := mutationInvoke(t, profiles, "builder", args...)
			if code != cli.ExitSuccess || diagnostic != "" || !strings.Contains(out, `"revision":8`) {
				t.Fatalf("CLI failed: %d %s %s", code, out, diagnostic)
			}
			var got request
			select {
			case got = <-requests:
			default:
				t.Fatal("CLI succeeded without sending the update")
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(test.body), &want); err != nil {
				t.Fatal(err)
			}
			if got.method != http.MethodPatch || got.path != "/api/v1/projects/"+project+"/jobs/job-target" || got.query != "" || got.key != "dependency-key" || got.worker != worker || !reflect.DeepEqual(got.body, want) {
				t.Fatalf("unexpected transport: %+v; want body %s", got, test.body)
			}
			if len(requests) != 0 {
				t.Fatal("unexpected extra operation")
			}
		})
	}
}

func TestJobDependenciesCLIInvalidBeforeCredentials(t *testing.T) {
	base := []string{"jobs", "update", "job-target", "--expect-revision", "7", "--key", "dependency-key"}
	cases := [][]string{
		{"--blocked-by"},
		{"--blocked-by", "", "--blocked-by", "job-first"},
		{"--blocked-by", "job-first", "--blocked-by", ""},
		{"--blocked-by", "", "--blocked-by", ""},
		{"--blocked-by", " job-first"},
		{"--blocked-by", "job/first"},
		{"--blocked-by", "bad\x00"},
		{"--blocked-by", "bad\xff"},
		{"--blocked-by", strings.Repeat("x", 129)},
		{"--blocked-by", "job-first", "--expect-revision", "8"},
		{"--blocked-by", "job-first", "--key", "other-key"},
	}
	var tooMany []string
	for i := 0; i < 21; i++ {
		tooMany = append(tooMany, "--blocked-by", fmt.Sprintf("job-%02d", i))
	}
	cases = append(cases, tooMany)
	for _, flags := range cases {
		args := append(append([]string{}, base...), flags...)
		code, out, diagnostic := run(append([]string{"--profile", "absent", "--profiles-file", "/missing/profiles.json"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "invalid_arguments") {
			t.Fatalf("invalid dependency input reached credentials: %q: %d %s", flags, code, diagnostic)
		}
	}
	for _, args := range [][]string{
		{"jobs", "update", "job-target", "--blocked-by", "job-first", "--key", "key"},
		{"jobs", "update", "job-target", "--blocked-by", "job-first", "--expect-revision", "7"},
		{"jobs", "update", "job-target", "--blocked-by", "job-first", "--expect-revision", "0", "--key", "key"},
		{"jobs", "update", "job-target", "--blocked-by", "job-first", "--expect-revision", "9223372036854775807", "--key", "key"},
	} {
		code, out, diagnostic := run(append([]string{"--profile", "absent", "--profiles-file", "/missing/profiles.json"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(diagnostic, "invalid_arguments") {
			t.Fatalf("missing/invalid mutation guard reached credentials: %q: %d %s", args, code, diagnostic)
		}
	}
}

func TestJobDependenciesCLIOfflineHelp(t *testing.T) {
	code, out, diagnostic := run([]string{"--profile", "absent", "--profiles-file", "/missing/profiles.json", "jobs", "update", "--help"}, nil)
	if code != cli.ExitSuccess || diagnostic != "" {
		t.Fatalf("help accessed credentials: %d %s", code, diagnostic)
	}
	var help struct {
		Optional  []string `json:"optional"`
		Semantics []string `json:"semantics"`
	}
	if err := json.Unmarshal([]byte(out), &help); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(help.Optional, " ") + " " + strings.Join(help.Semantics, " ")
	for _, want := range []string{"--blocked-by JOB_ID (repeat up to 20)", "literal job ID", "omission preserves", `--blocked-by ""`, "mixed with other values are invalid", "do not change job status"} {
		if !strings.Contains(text, want) {
			t.Errorf("help missing %q", want)
		}
	}
}
