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
