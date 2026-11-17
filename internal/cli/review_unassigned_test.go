package cli

import "testing"

func TestRouteUnassignedExplicitChoice(t *testing.T) {
	base := []string{"review_evidence_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--key", "unassigned-test", "--expect-revision", "0", "--finding", "0", "--reason", "Needs later owner"}
	for _, test := range []struct {
		args   []string
		valid  bool
		target int64
	}{
		{[]string{"--unassigned"}, true, 0}, {[]string{"--target-pr", "4"}, true, 4},
		{nil, false, 0}, {[]string{"--target-pr", "0"}, false, 0},
		{[]string{"--unassigned", "--target-pr", "4"}, false, 0},
		{[]string{"--unassigned", "--unassigned"}, false, 0}, {[]string{"--unassigned=true"}, false, 0},
	} {
		call, ok := parseReviewControl("route-create", append(append([]string{}, base...), test.args...))
		if ok != test.valid {
			t.Fatalf("%v: valid=%v", test.args, ok)
		}
		if ok && call.reviewRoute.TargetPullNumber != test.target {
			t.Fatal("wrong target")
		}
	}
}
