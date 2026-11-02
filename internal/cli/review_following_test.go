package cli_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestPRFollowingCLIHelpValidationAndRealBoundPreferences(t *testing.T) {
	for _, name := range []string{"following", "follow", "unfollow"} {
		code, out, stderr := run([]string{"reviews", name, "--help"}, nil)
		if code != 0 || stderr != "" || !strings.Contains(out, "docs/review-notifications.md") {
			t.Fatal(code, out, stderr)
		}
	}
	for _, args := range [][]string{{"following"}, {"following", "02"}, {"following", "2", "--key", "k"}, {"follow", "2", "--key", "k"}, {"unfollow", "2", "--expect-revision", "-1", "--key", "k"}, {"follow", "2", "--expect-revision", "0", "--worker", "other", "--key", "k"}} {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing/profiles", "reviews"}, args...), map[string]string{"FC_PROFILE": "missing"})
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(code, out, stderr)
		}
	}
	f, project, worker := reviewExecutionFixture(t, true)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"builder": {project, worker}})
	invoke := func(args ...string) client.ReviewFollowing {
		t.Helper()
		code, out, stderr := mutationInvoke(t, profiles, "builder", append([]string{"reviews"}, args...)...)
		var v client.ReviewFollowing
		if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &v) != nil {
			t.Fatal(code, out, stderr)
		}
		return v
	}
	v := invoke("following", "2")
	if v.Mode != "none" || v.WorkerID != worker {
		t.Fatal(v)
	}
	muted := invoke("unfollow", "2", "--expect-revision", "0", "--key", "mute-pr")
	if muted.Mode != "muted" || muted.Revision != 1 {
		t.Fatal(muted)
	}
	if replay := invoke("unfollow", "2", "--expect-revision", "0", "--key", "mute-pr"); replay != muted {
		t.Fatal(replay)
	}
	v = invoke("follow", "2", "--expect-revision", "1", "--key", "follow-pr")
	if v.Mode != "follow" || v.Revision != 2 {
		t.Fatal(v)
	}
}
