package cli

import (
	"strings"
	"testing"
)

func TestReviewOperationStatusParseCompactAndDiagnostics(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	for _, args := range [][]string{{id}, {id, "--wait-seconds", "900"}, {"--diagnostics", id, "--wait-seconds=50"}} {
		v, ok := parseReviewOperationStatus(args)
		if !ok || v.ID != id {
			t.Fatal(args, v, ok)
		}
		if args[0] == "--diagnostics" && (!v.Diagnostics || v.WaitSeconds != 50) {
			t.Fatal("diagnostic output view lost", v)
		}
	}
	for _, args := range [][]string{{}, {"bad"}, {id, id}, {id, "--wait-seconds", "901"}, {id, "--wait-seconds", "01"}, {id, "--wait-seconds", "-1"}, {id, "--wait-seconds"}, {id, "--diagnostics=true"}, {id, "--diagnostics", "--diagnostics"}, {id, "--cursor", "x"}, {id, "--wait-seconds=1", "--wait-seconds=2"}} {
		if _, ok := parseReviewOperationStatus(args); ok {
			t.Fatal("invalid arguments accepted", args)
		}
	}
}

func TestReviewOperationStatusDispatchPreservesReadOptions(t *testing.T) {
	id := "review_operation_" + strings.Repeat("a", 32)
	for _, tc := range []struct {
		args        []string
		wait        int
		diagnostics bool
	}{
		{[]string{id}, 0, false},
		{[]string{id, "--wait-seconds", "900"}, 900, false},
		{[]string{"--wait-seconds=0", id, "--diagnostics"}, 0, true},
		{[]string{"--diagnostics", id, "--wait-seconds=50"}, 50, true},
	} {
		call, ok := parseInvocation(append([]string{"reviews", "operation"}, tc.args...))
		if !ok || call.reviewOperationStatus == nil {
			t.Fatal("operation did not reach its dedicated parser", tc.args, call, ok)
		}
		v := call.reviewOperationStatus
		if v.ID != id || v.WaitSeconds != tc.wait || v.Diagnostics != tc.diagnostics || call.waitSeconds != tc.wait {
			t.Fatal("operation view or request budget changed", tc.args, call, v)
		}
		if call.mutation != nil || len(call.position) != 0 {
			t.Fatal("operation read became a mutation or generic selector", call)
		}
	}
	for _, args := range [][]string{
		{}, {"bad"}, {id, id}, {id, "--wait-seconds=901"}, {id, "--wait-seconds=01"},
		{id, "--wait-seconds=-1"}, {id, "--wait-seconds"},
		{id, "--wait-seconds=1", "--wait-seconds=2"},
		{id, "--diagnostics=true"}, {id, "--diagnostics", "--diagnostics"},
		{id, "--limit=5"}, {id, "--cursor=x"}, {id, "--pr=1"},
		{id, "--snapshot=review_snapshot_" + strings.Repeat("a", 32)},
		{id, "--project=other"}, {id, "--worker=other"}, {id, "--key=write"},
	} {
		if _, ok := parseInvocation(append([]string{"reviews", "operation"}, args...)); ok {
			t.Fatal("operation accepted invalid or unrelated options", args)
		}
	}
}

func TestReviewGenericReadsKeepTheirOwnFlags(t *testing.T) {
	suffix := strings.Repeat("a", 32)
	snapshot := "review_snapshot_" + suffix
	for _, args := range [][]string{
		{"queue", "--limit=7", "--cursor=next"},
		{"pull", "1", "--snapshot", snapshot},
		{"runs", "1", "--limit=7", "--cursor=next", "--snapshot", snapshot},
		{"attempts", "1", "--limit=7", "--cursor=next", "--snapshot", snapshot},
		{"run", "review_run_" + suffix, "--snapshot", snapshot},
		{"evidence", "review_evidence_" + suffix},
		{"adjudication", "review_evidence_" + suffix},
		{"assess", "review_evidence_" + suffix},
		{"source-operation", "review_refresh_" + suffix},
		{"route", "review_route_" + suffix},
		{"routes", "--pr=1", "--limit=7", "--cursor=next"},
		{"publication", "review_publication_" + suffix},
		{"execution"},
	} {
		t.Run(args[0], func(t *testing.T) {
			full := append([]string{"reviews"}, args...)
			call, ok := parseInvocation(full)
			if !ok || call.mutation != nil || call.reviewOperationStatus != nil || call.waitSeconds != 0 {
				t.Fatal("generic read dispatch changed", full, call, ok)
			}
			for _, arg := range args {
				if arg == "--limit=7" && call.limit != 7 || arg == "--cursor=next" && call.cursor != "next" || arg == "--pr=1" && call.pullNumber != 1 || arg == "--snapshot" && call.snapshot != snapshot {
					t.Fatal("generic read lost its option", full, arg, call)
				}
			}
			for _, flag := range []string{"--wait-seconds=0", "--wait-seconds=900", "--diagnostics", "--project=other", "--worker=other", "--key=write"} {
				if _, ok := parseInvocation(append(append([]string(nil), full...), flag)); ok {
					t.Fatal("generic read accepted operation or authority option", full, flag)
				}
			}
		})
	}
}
