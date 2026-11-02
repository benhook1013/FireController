package reviewprovider

import (
	"testing"
	"time"
)

func addRetainedAttemptCheck(f *ciAssessmentFixture, conclusion string) {
	old := f.Inventory.Checks[0]
	old.ID = 301 // The historical ID is deliberately higher than the current ID.
	old.Status, old.Conclusion = "completed", conclusion
	old.CompletedAt = f.Inventory.ObservedAt.Add(-time.Hour)
	old.StartedAt = old.CompletedAt.Add(-time.Minute)
	f.Inventory.Checks = append(f.Inventory.Checks, old)
}

func TestAssessRequiredCISameSuiteRerunUsesExactCurrentAttempt(t *testing.T) {
	for _, test := range []struct{ currentStatus, currentConclusion, oldConclusion, want string }{
		{"completed", "success", "failure", "satisfied"},
		{"completed", "failure", "success", "failed"},
		{"queued", "", "success", "pending"},
	} {
		for _, reverse := range []bool{false, true} {
			f := requiredCIFixture()
			setCIJobState(&f, test.currentStatus, test.currentConclusion)
			addRetainedAttemptCheck(&f, test.oldConclusion)
			if reverse {
				f.Inventory.Checks[0], f.Inventory.Checks[1] = f.Inventory.Checks[1], f.Inventory.Checks[0]
			}
			before := string(fixtureJSON(t, f))
			got, err := f.assess()
			if err != nil || got.State != test.want || len(got.Checks) != 1 || got.Checks[0].CheckRunID != 300 || got.Checks[0].RunID != 80 || got.Checks[0].RunAttempt != 2 {
				t.Fatal("retained same-suite attempt replaced exact current job", test, reverse, got, err)
			}
			if string(fixtureJSON(t, f)) != before {
				t.Fatal("selection rewrote retained attempt facts")
			}
		}
	}
}

func TestAssessRequiredCISameSuiteRerunCannotGuessMissingOrAmbiguousCurrentness(t *testing.T) {
	cases := map[string]func(*ciAssessmentFixture){
		"missing current jobs":       func(f *ciAssessmentFixture) { f.Attempts[0].Jobs = []ActionsJob{} },
		"unproven current attempt":   func(f *ciAssessmentFixture) { f.Attempts[0].State = "unproven"; f.Attempts[0].Jobs = []ActionsJob{} },
		"first attempt duplicates":   func(f *ciAssessmentFixture) { f.Actions.Runs[0].RunAttempt = 1; f.Attempts[0].RunAttempt = 1 },
		"unknown historical suite":   func(f *ciAssessmentFixture) { f.Inventory.Checks[1].SuiteID = 999 },
		"unrestricted different app": func(f *ciAssessmentFixture) { f.Authority.Checks[0].AppID = 0; f.Inventory.Checks[1].AppID = 401 },
		"unmatched pending check": func(f *ciAssessmentFixture) {
			f.Inventory.Checks[1].Status = "queued"
			f.Inventory.Checks[1].Conclusion = ""
			f.Inventory.Checks[1].CompletedAt = time.Time{}
		},
		"current job changed": func(f *ciAssessmentFixture) { f.Attempts[0].Jobs[0].Conclusion = "failure" },
		"two current matching jobs": func(f *ciAssessmentFixture) {
			job := f.Attempts[0].Jobs[0]
			job.ID = 501
			job.CheckRunID = 301
			job.Conclusion = "failure"
			f.Attempts[0].Jobs = append(f.Attempts[0].Jobs, job)
		},
		"manual workflow event": func(f *ciAssessmentFixture) {
			f.Actions.Runs[0].Event = "workflow_dispatch"
			f.Attempts[0].Event = "workflow_dispatch"
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := requiredCIFixture()
			addRetainedAttemptCheck(&f, "failure")
			edit(&f)
			got, err := f.assess()
			if err != nil || got.State != "unproven" {
				t.Fatal("rerun selection weakened current evidence guard", got, err)
			}
		})
	}
}

// The parent reducer conservatively retained both same-suite attempts. Reading
// that exact original result must not re-credit it under newer collection rules.
func TestValidRetainedRequiredCISameSuiteAmbiguity(t *testing.T) {
	f := requiredCIFixture()
	addRetainedAttemptCheck(&f, "failure")
	current, err := f.assess()
	if err != nil || current.State != "satisfied" {
		t.Fatal(current, err)
	}
	original := RequiredCIAssessment{Repository: f.Inventory.Repository, HeadSHA: f.Inventory.HeadSHA, State: "unproven", Reason: "ci_required_checks_unproven", Checks: []RequiredCIAssessmentCheck{{Context: "build", AppID: 400, State: "unproven", Reason: "ci_check_ambiguous"}}}
	before := string(fixtureJSON(t, f))
	if !ValidRetainedRequiredCIAssessment(f.Authority, f.Inventory, f.Actions, f.Attempts, original) {
		t.Fatal("exact parent same-suite ambiguity rejected")
	}
	if string(fixtureJSON(t, f)) != before {
		t.Fatal("original inventory rewritten")
	}
	for _, edit := range []func(*RequiredCIAssessment){
		func(v *RequiredCIAssessment) { v.Checks[0].CheckRunID = 300 },
		func(v *RequiredCIAssessment) { v.Checks[0].Reason = "ci_unknown" },
		func(v *RequiredCIAssessment) { v.HeadSHA = f.Inventory.HeadSHA[:39] + "b" },
	} {
		changed := original
		changed.Checks = append([]RequiredCIAssessmentCheck(nil), original.Checks...)
		edit(&changed)
		if ValidRetainedRequiredCIAssessment(f.Authority, f.Inventory, f.Actions, f.Attempts, changed) {
			t.Fatal("changed parent facts accepted", changed)
		}
	}
	// Reversed skipped intervals are newly supported facts, not facts that the
	// original reducer could have admitted with its older conservative result.
	f.Inventory.Checks[1].Conclusion = "skipped"
	f.Inventory.Checks[1].StartedAt = f.Inventory.Checks[1].CompletedAt.Add(time.Second)
	if ValidRetainedRequiredCIAssessment(f.Authority, f.Inventory, f.Actions, f.Attempts, original) {
		t.Fatal("parent-only assessment accepted newly admitted timestamps")
	}
}

func TestValidRetainedRequiredCIOriginHoldWithCurrentRerunReduction(t *testing.T) {
	f := requiredCIFixture()
	addRetainedAttemptCheck(&f, "failure")
	addCIStatus(&f, 201, "success")
	current, err := f.assess()
	if err != nil || current.State != "satisfied" {
		t.Fatal(current, err)
	}
	original := current
	original.Checks = append([]RequiredCIAssessmentCheck(nil), current.Checks...)
	original.State, original.Reason = "unproven", "ci_required_checks_unproven"
	original.Checks[0].State, original.Checks[0].Reason = "unproven", "ci_status_origin_unproven"
	if !ValidRetainedRequiredCIAssessment(f.Authority, f.Inventory, f.Actions, f.Attempts, original) {
		t.Fatal("previously accepted origin-only hold rejected with retained same-suite checks")
	}
	original.Checks[0].CheckRunID = 301
	if ValidRetainedRequiredCIAssessment(f.Authority, f.Inventory, f.Actions, f.Attempts, original) {
		t.Fatal("historical check substituted for original current check")
	}
}
