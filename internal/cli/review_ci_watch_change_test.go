package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestCIWatchChangeCLIMapsBaselineByPRBeforeOrAfterTargets(t *testing.T) {
	head, base, key := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 64)
	for _, args := range [][]string{
		{"--after-observation", "4:" + key, "--target", "2:" + head + ":" + base, "--target", "4:" + head, "--wait-seconds", "0"},
		{"--target=2:" + head + ":" + base, "--target=4:" + head, "--after-observation=4:" + key, "--wait-seconds=0"},
	} {
		got, ok := parseCIWatch(args)
		want := []client.CIWatchTarget{{PullNumber: 2, HeadSHA: head, BaseSHA: base}, {PullNumber: 4, HeadSHA: head, AfterObservationKey: key}}
		if !ok || got.seconds != 0 || !reflect.DeepEqual(got.targets, want) {
			t.Fatal("baseline order changed source pins or selected the wrong PR", got, ok)
		}
	}
}

func TestCIWatchChangeCLIRejectsInvalidBaselineBeforeProfile(t *testing.T) {
	head, key := strings.Repeat("a", 40), strings.Repeat("c", 64)
	for _, extra := range [][]string{
		{"--after-observation", "2:"}, {"--after-observation", "2:short"}, {"--after-observation", "2:" + strings.Repeat("A", 64)}, {"--after-observation", "2:" + strings.Repeat("g", 64)},
		{"--after-observation", "3:" + key}, {"--after-observation", "02:" + key}, {"--after-observation", "2:" + key + ":extra"},
		{"--after-observation", "2:" + key, "--after-observation", "2:" + key}, {"--after-observation"},
	} {
		args := []string{"--profiles-file", "/unavailable", "--profile", "missing", "reviews", "ci-wait", "--target", "2:" + head}
		args = append(args, extra...)
		var out, diag bytes.Buffer
		if code := RunContext(t.Context(), args, nil, &out, &diag); code != ExitUsage || out.Len() != 0 || !strings.Contains(diag.String(), "invalid_arguments") {
			t.Fatal("invalid baseline reached credentials or HTTP", extra, code, out.String(), diag.String())
		}
	}
	var out, diag bytes.Buffer
	if code := RunContext(t.Context(), []string{"reviews", "ci-wait", "--help"}, nil, &out, &diag); code != ExitSuccess || diag.Len() != 0 || !strings.Contains(out.String(), "--after-observation") || !strings.Contains(out.String(), "passed or failed") || !strings.Contains(out.String(), "pending checks alone") {
		t.Fatal("offline help does not expose passed/failed rearm and terminal-wait limits", code, out.String(), diag.String())
	}
}
