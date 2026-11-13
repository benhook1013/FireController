package cli_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewRoundsCLIInvalidBeforeCredentials(t *testing.T) {
	base := []string{"reviews", "rounds", "grant", "--pr", "2", "--channel", "cli", "--key", "round-key", "--expected-revision", "0"}
	cases := [][]string{{"reviews", "rounds"}, {"reviews", "rounds", "read", "--pr", "02", "--channel", "cli"}, {"reviews", "rounds", "read", "--pr", "2", "--channel", "other"}, {"reviews", "rounds", "read", "--pr", "2", "--channel", "cli", "--key", "forbidden"}, {"reviews", "rounds", "close", "--pr", "2", "--channel", "cli", "--expected-revision", "0"}}
	for _, suffix := range [][]string{{}, {"--max", "0"}, {"--max", "1001"}, {"--max", "03"}, {"--max", "3", "--min", "4"}, {"--exact", "0"}, {"--exact", "2", "--min", "0"}, {"--exact", "2", "--max", "2"}, {"--max", "2", "--max", "3"}, {"--max", "2", "--expected-revision", "1"}, {"--max", "2", "--worker", "other"}, {"--exact", "+2"}, {"--exact", "2", "extra"}} {
		cases = append(cases, append(append([]string{}, base...), suffix...))
	}
	taper := []string{"reviews", "rounds", "taper", "--pr", "2", "--channel", "cli", "--expected-revision", "1", "--key", "taper-key"}
	for _, suffix := range [][]string{{}, {"--taper", "0"}, {"--taper", "1001"}, {"--taper", "02"}, {"--taper", "+2"}, {"--taper", "2", "--min", "0"}, {"--taper", "2", "--max", "8"}, {"--taper", "2", "--exact", "2"}, {"--taper", "2", "--taper", "3"}} {
		cases = append(cases, append(append([]string{}, taper...), suffix...))
	}
	cases = append(cases, append(append([]string{}, base...), "--max", "8", "--taper", "2"))
	for _, args := range cases {
		code, out, stderr := run(append([]string{"--profiles-file", "/unavailable/round-profile"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("invalid round args reached profile", args, code, stderr)
		}
	}
}

func TestReviewRoundsCLIRealHTTPGrantReadCloseAndReplay(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	var calls atomic.Int64
	var drop atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == "POST" && drop.Swap(false) {
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, r)
			if rec.Code != 200 {
				t.Errorf("dropped call did not commit: %d", rec.Code)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	path := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
	read := func(channel string) client.ReviewRoundControl {
		t.Helper()
		code, out, stderr := mutationInvoke(t, path, "builder", "reviews", "rounds", "read", "--pr", "2", "--channel", channel)
		var value client.ReviewRoundControl
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &value) != nil || value.ProjectID != project || value.Channel != channel {
			t.Fatal("bad round read", code, stderr)
		}
		return value
	}
	initial := read("cli")
	if !initial.Configured || initial.Revision != 1 || initial.Epoch != 1 || initial.Minimum != 2 || initial.Maximum != 6 || initial.Taper != 2 || initial.Used != 0 || initial.Dry != 0 {
		t.Fatal("new CLI enrollment lost product defaults", initial)
	}
	initialHosted := read("hosted")
	if !initialHosted.Configured || initialHosted.Revision != 1 || initialHosted.Minimum != 2 || initialHosted.Maximum != 3 || initialHosted.Taper != 2 {
		t.Fatal("new Hosted enrollment lost product defaults", initialHosted)
	}
	args := []string{"reviews", "rounds", "grant", "--pr", "2", "--channel", "cli", "--expected-revision", "1", "--max", "8", "--key", "round-grant"}
	beforeProvider := f.resolverHit.Load()
	before := calls.Load()
	first := roundSuccess(t, path, "builder", args...)
	if len(first) != 5 || first["outcome"] != "granted" || calls.Load() != before+2 {
		t.Fatal("expanded receipt or extra requests", first)
	}
	if encodeString(first) != encodeString(roundSuccess(t, path, "builder", args...)) {
		t.Fatal("grant retry changed result")
	}
	state := read("cli")
	if !state.Configured || state.Revision != 2 || state.Epoch != 1 || state.Minimum != 0 || state.Maximum != 8 || state.Taper != 2 {
		t.Fatal("bad grant state", state)
	}
	closeArgs := []string{"reviews", "rounds", "close", "--pr", "2", "--channel", "cli", "--expected-revision", "2", "--key", "round-close"}
	drop.Store(true)
	code, out, stderr := mutationInvoke(t, path, "builder", closeArgs...)
	if code == 0 || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) {
		t.Fatal("lost committed close not recoverable", code, stderr)
	}
	closed := roundSuccess(t, path, "builder", closeArgs...)
	if closed["outcome"] != "closed" || closed["revision"] != float64(3) || !read("cli").Closed {
		t.Fatal("close recovery failed")
	}
	taperArgs := []string{"reviews", "rounds", "taper", "--pr", "2", "--channel", "cli", "--expected-revision", "3", "--taper", "1", "--key", "round-taper"}
	tapered := roundSuccess(t, path, "builder", taperArgs...)
	if tapered["outcome"] != "taper_updated" || tapered["revision"] != float64(4) || encodeString(tapered) != encodeString(roundSuccess(t, path, "builder", taperArgs...)) {
		t.Fatal("taper update or exact replay changed receipt", tapered)
	}
	current := read("cli")
	if current.Taper != 1 || !current.Closed || current.Minimum != state.Minimum || current.Maximum != state.Maximum || current.Used != state.Used || current.Dry != state.Dry || current.Epoch != state.Epoch {
		t.Fatal("taper changed closure, grants or progress", current)
	}
	noopArgs := []string{"reviews", "rounds", "taper", "--pr", "2", "--channel", "cli", "--expected-revision", "4", "--taper", "1", "--key", "round-taper-noop"}
	noop := roundSuccess(t, path, "builder", noopArgs...)
	if noop["outcome"] != "taper_updated" || noop["revision"] != float64(4) || encodeString(noop) != encodeString(roundSuccess(t, path, "builder", noopArgs...)) || read("cli") != current {
		t.Fatal("same taper changed control or exact replay", noop)
	}
	if hosted := read("hosted"); hosted != initialHosted {
		t.Fatal("CLI controls changed Hosted defaults", hosted)
	}
	exact := roundSuccess(t, path, "builder", "reviews", "rounds", "grant", "--pr", "2", "--channel", "hosted", "--expected-revision", "1", "--exact", "2", "--key", "hosted-exact")
	if exact["epoch"] != float64(1) {
		t.Fatal(exact)
	}
	hosted := read("hosted")
	if hosted.Minimum != 2 || hosted.Maximum != 2 || hosted.Taper != 2 {
		t.Fatal("exact did not bind bounds")
	}
	if f.resolverHit.Load() != beforeProvider {
		t.Fatal("round controls invoked provider")
	}
	var stderrBuffer strings.Builder
	failedOut := roundFailWriter{}
	full := append([]string{"--profiles-file", path, "--profile", "builder"}, closeArgs...)
	code = cli.RunContext(context.Background(), full, map[string]string{}, failedOut, &stderrBuffer)
	if code != cli.ExitConfig || !strings.Contains(stderrBuffer.String(), `"outcome_unknown":true`) {
		t.Fatal("failed receipt write hid recovery", code, stderrBuffer.String())
	}
}

type roundFailWriter struct{}

func (roundFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func roundSuccess(t *testing.T, path, profile string, args ...string) map[string]any {
	t.Helper()
	code, out, stderr := mutationInvoke(t, path, profile, args...)
	var receipt map[string]any
	if code != cli.ExitSuccess || stderr != "" || strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &receipt) != nil || len(receipt) != 5 {
		t.Fatal("bad round receipt", code, out, stderr)
	}
	return receipt
}

func TestReviewRoundsCLILocalHelpWithoutCredentials(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}, {"read", "--help"}, {"grant", "--help"}, {"close", "--help"}, {"taper", "--help"}} {
		full := append([]string{"--profiles-file", "/unavailable/round-profile", "reviews", "rounds"}, args...)
		code, out, stderr := run(full, map[string]string{"FC_PROFILE": "missing"})
		var help map[string]any
		if code != cli.ExitSuccess || stderr != "" || json.Unmarshal([]byte(out), &help) != nil || !strings.Contains(help["usage"].(string), "reviews rounds") {
			t.Fatal("local round help reached credentials or was invalid", args, code, stderr)
		}
	}
	for _, args := range [][]string{{"unknown", "--help"}, {"read", "--help", "--pr", "2"}, {"grant", "--help=1"}} {
		code, _, _ := run(append([]string{"reviews", "rounds"}, args...), nil)
		if code != cli.ExitUsage {
			t.Fatal("malformed help accepted", args, code)
		}
	}
}

func TestReviewRoundsCLIRevisionAliasAndDuplicateRefusal(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	path := mutationProfiles(t, f, srv.URL, map[string][2]string{"builder": {project, worker}})
	args := []string{"reviews", "rounds", "grant", "--pr", "2", "--channel", "cli", "--expect-revision", "1", "--max", "8", "--key", "canonical-revision"}
	original := roundSuccess(t, path, "builder", args...)
	alias := append([]string{}, args...)
	for i := range alias {
		if alias[i] == "--expect-revision" {
			alias[i] = "--expected-revision"
		}
	}
	if encodeString(original) != encodeString(roundSuccess(t, path, "builder", alias...)) {
		t.Fatal("revision alias changed exact replay")
	}
	for _, extra := range [][]string{{"--expected-revision", "1"}, {"--expect-revision=1"}, {"--expected-revision=2"}} {
		full := append([]string{"--profiles-file", "/unavailable/round-profile"}, args...)
		full = append(full, extra...)
		code, _, stderr := run(full, map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal("duplicate revision reached credentials", code, stderr)
		}
	}
}
