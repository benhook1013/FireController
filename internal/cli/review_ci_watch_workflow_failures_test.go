package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestCIWatchWorkflowFailuresCLIMapsRepeatedSelectionByPR(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	for _, args := range [][]string{
		{"--failure-workflow", "4:302", "--target", "2:" + head + ":" + base, "--failure-workflow", "2:301", "--target", "4:" + head, "--failure-workflow", "4:303"},
		{"--target=2:" + head + ":" + base, "--failure-workflow=2:301", "--target=4:" + head, "--failure-workflow=4:302", "--failure-workflow=4:303"},
	} {
		got, ok := parseCIWatch(args)
		if !ok || len(got.targets) != 2 || got.targets[0].BaseSHA != base || !reflect.DeepEqual(got.targets[0].FailureWorkflowIDs, []int64{301}) || !reflect.DeepEqual(got.targets[1].FailureWorkflowIDs, []int64{302, 303}) {
			t.Fatal("workflow selection changed target binding/order", got, ok)
		}
	}
}

func TestCIWatchWorkflowFailuresCLIInvalidSelectionBeforeProfile(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, extra := range [][]string{
		{"--failure-workflow", "2:0"}, {"--failure-workflow", "2:-1"}, {"--failure-workflow", "2:01"}, {"--failure-workflow", "2:not-an-id"}, {"--failure-workflow", "3:301"}, {"--failure-workflow", "02:301"}, {"--failure-workflow", "2:"}, {"--failure-workflow", "2:301:extra"}, {"--failure-workflow", "2:301", "--failure-workflow", "2:301"}, {"--failure-workflow"},
	} {
		args := append([]string{"--profiles-file", "/unavailable", "--profile", "missing", "reviews", "ci-wait", "--target", "2:" + head}, extra...)
		var out, diag bytes.Buffer
		if code := RunContext(t.Context(), args, nil, &out, &diag); code != ExitUsage || out.Len() != 0 || !strings.Contains(diag.String(), "invalid_arguments") {
			t.Fatal("invalid workflow selector reached credentials/HTTP", extra, code, out.String(), diag.String())
		}
	}
	var out, diag bytes.Buffer
	if code := RunContext(t.Context(), []string{"reviews", "ci-wait", "--help"}, nil, &out, &diag); code != ExitSuccess || diag.Len() != 0 || !strings.Contains(out.String(), "--failure-workflow") {
		t.Fatal("offline help omits workflow attention", code, out.String(), diag.String())
	}
}
