package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestCIWatchCLIExactTargetsAndLocalHelp(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	args := []string{"reviews", "ci-wait", "--target", "2:" + head + ":" + base, "--target=4:" + head, "--wait-seconds", "900"}
	call, ok := parseInvocation(args)
	if !ok || call.ciWatch == nil || call.ciWatch.seconds != 900 || len(call.ciWatch.targets) != 2 || call.ciWatch.targets[0].BaseSHA != base || call.ciWatch.targets[1].BaseSHA != "" || call.mutation != nil {
		t.Fatal("exact read target parsing changed", call, ok)
	}
	if got, ok := parseCIWatch([]string{"--target", "2:" + head}); !ok || got.seconds != 300 {
		t.Fatal("default wait changed", got, ok)
	}
	if got, ok := parseCIWatch([]string{"--target", "2:" + head, "--wait-seconds", "0"}); !ok || got.seconds != 0 {
		t.Fatal("immediate result refused", got, ok)
	}
	for _, args := range [][]string{nil, {"--target", "2:short"}, {"--target", "2:" + head + ":"}, {"--target", "02:" + head}, {"--target", "2:" + head, "--target", "2:" + head}, {"--target", "2:" + head, "--key", "private"}, {"--target", "2:" + head, "--wait-seconds", "901"}, {"--target", "2:" + head, "--wait-seconds", "0", "--wait-seconds", "1"}} {
		var out, diag bytes.Buffer
		input := append([]string{"--profile", "missing", "--profiles-file", "/unavailable", "reviews", "ci-wait"}, args...)
		if code := RunContext(t.Context(), input, nil, &out, &diag); code != ExitUsage || out.Len() != 0 || !strings.Contains(diag.String(), "invalid_arguments") {
			t.Fatal("invalid read reached profile or service", args, code, out.String(), diag.String())
		}
	}
	var out, diag bytes.Buffer
	if code := RunContext(t.Context(), []string{"--profiles-file", "/unavailable", "reviews", "ci-wait", "--help"}, nil, &out, &diag); code != ExitSuccess || diag.Len() != 0 || !strings.Contains(out.String(), "default 300") || !strings.Contains(out.String(), "0-900") || !strings.Contains(out.String(), "default merge branch") || !strings.Contains(out.String(), "all_targets_passed") || !strings.Contains(out.String(), "wake condition") {
		t.Fatal("offline CI watch help unavailable", code, out.String(), diag.String())
	}
}
