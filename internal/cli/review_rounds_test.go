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
	if read("cli").Configured {
		t.Fatal("implicit round grant")
	}
	args := []string{"reviews", "rounds", "grant", "--pr", "2", "--channel", "cli", "--expected-revision", "0", "--max", "8", "--key", "round-grant"}
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
	if !state.Configured || state.Revision != 1 || state.Epoch != 1 || state.Minimum != 0 || state.Maximum != 8 || state.Taper != 3 {
		t.Fatal("bad grant state", state)
	}
	closeArgs := []string{"reviews", "rounds", "close", "--pr", "2", "--channel", "cli", "--expected-revision", "1", "--key", "round-close"}
	drop.Store(true)
	code, out, stderr := mutationInvoke(t, path, "builder", closeArgs...)
	if code == 0 || out != "" || !strings.Contains(stderr, `"outcome_unknown":true`) {
		t.Fatal("lost committed close not recoverable", code, stderr)
	}
	closed := roundSuccess(t, path, "builder", closeArgs...)
	if closed["outcome"] != "closed" || closed["revision"] != float64(2) || !read("cli").Closed {
		t.Fatal("close recovery failed")
	}
	if read("hosted").Configured {
		t.Fatal("channel leaked")
	}
	exact := roundSuccess(t, path, "builder", "reviews", "rounds", "grant", "--pr", "2", "--channel", "hosted", "--expected-revision", "0", "--exact", "2", "--key", "hosted-exact")
	if exact["epoch"] != float64(1) {
		t.Fatal(exact)
	}
	hosted := read("hosted")
	if hosted.Minimum != 2 || hosted.Maximum != 2 || hosted.Taper != 1 {
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
