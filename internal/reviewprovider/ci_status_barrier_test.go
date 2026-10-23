package reviewprovider

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestAssessRequiredCIAppBoundStatusIsAdditionalBarrier(t *testing.T) {
	for _, native := range []string{"success", "pending", "failure", "missing", "wrong_app", "unknown_origin", "missing_attempt", "missing_job", "superseded", "ambiguous", "unsupported_event", "changed_job"} {
		for _, status := range []string{"success", "pending", "failure", "error", "future"} {
			t.Run(native+"/"+status, func(t *testing.T) {
				f := requiredCIFixture()
				want := "unproven"
				switch native {
				case "success":
					want = "satisfied"
				case "pending":
					setCIJobState(&f, "in_progress", "")
					want = "pending"
				case "failure":
					setCIJobState(&f, "completed", "failure")
					want = "failed"
				case "missing":
					f.Inventory.Checks = []CICheckRun{}
				case "wrong_app":
					f.Inventory.Checks[0].AppID++
				case "unknown_origin":
					f.Inventory.Checks[0].SuiteID = 999
				case "missing_attempt":
					f.Attempts = []ActionsAttempt{}
				case "missing_job":
					f.Attempts[0].Jobs = []ActionsJob{}
				case "superseded":
					newest := f.Actions.Runs[0]
					newest.ID, newest.CheckSuiteID, newest.RunNumber, newest.RunAttempt = 81, 91, 13, 1
					f.Actions.Runs = append(f.Actions.Runs, newest)
				case "ambiguous":
					c := f.Inventory.Checks[0]
					c.ID++
					f.Inventory.Checks = append(f.Inventory.Checks, c)
					j := f.Attempts[0].Jobs[0]
					j.ID++
					j.CheckRunID = c.ID
					f.Attempts[0].Jobs = append(f.Attempts[0].Jobs, j)
				case "unsupported_event":
					f.Actions.Runs[0].Event = "workflow_dispatch"
					f.Attempts[0].Event = "workflow_dispatch"
				case "changed_job":
					f.Inventory.Checks[0].Conclusion = "failure"
				}
				statusWant := map[string]string{"success": "satisfied", "pending": "pending", "failure": "failed", "error": "failed", "future": "unproven"}[status]
				if ciAssessmentRank(statusWant) > ciAssessmentRank(want) {
					want = statusWant
				}
				addCIStatus(&f, 3, status)
				// The later array entry has larger IDs and clocks; provider ordering owns
				// the selected status and may not be replaced by a convenient green value.
				addCIStatus(&f, 999, "success")
				f.Inventory.Statuses[1].UpdatedAt = f.Inventory.Statuses[1].UpdatedAt.Add(time.Hour)
				before := fixtureJSON(t, f)
				got, err := f.assess()
				if err != nil || got.State != want || len(got.Checks) != 1 || got.Checks[0].State != want || got.Checks[0].StatusID != 3 {
					t.Fatal("wrong independent barrier", got, err, want)
				}
				if native == "success" && (got.Checks[0].CheckRunID != 300 || got.Checks[0].RunID != 80 || got.Checks[0].RunAttempt != 2) {
					t.Fatal("native proof lost")
				}
				if string(before) != string(fixtureJSON(t, f)) {
					t.Fatal("facts rewritten")
				}
			})
		}
	}
}

func TestAssessRequiredCIAppBoundStatusAloneCannotSatisfy(t *testing.T) {
	for _, status := range []string{"success", "pending", "failure"} {
		t.Run(status, func(t *testing.T) {
			f := requiredCIFixture()
			f.Inventory.Checks = []CICheckRun{}
			f.Actions.Runs = []ActionsHeadRun{}
			f.Attempts = []ActionsAttempt{}
			addCIStatus(&f, 3, status)
			got, err := f.assess()
			if err != nil || got.State != "unproven" || got.Checks[0].CheckRunID != 0 || got.Checks[0].Reason != "ci_required_check_missing" {
				t.Fatal("status substituted for App identity", got, err)
			}
			f.Authority.Checks[0].AppID = 0
			got, err = f.assess()
			want := map[string]string{"success": "satisfied", "pending": "pending", "failure": "failed"}[status]
			if err != nil || got.State != want {
				t.Fatal("unbound status-only regressed", got, err)
			}
		})
	}
}

func TestRetainedCIAssessmentAcceptsOnlyExactLegacyOriginHold(t *testing.T) {
	for _, status := range []string{"success", "pending", "failure", "future"} {
		t.Run(status, func(t *testing.T) {
			f := requiredCIFixture()
			addCIStatus(&f, 3, status)
			legacy, err := assessRequiredCI(f.Authority, f.Inventory, f.Actions, f.Attempts, true)
			if err != nil || legacy.State != "unproven" || legacy.Checks[0].Reason != "ci_status_origin_unproven" {
				t.Fatal("legacy fixture", legacy, err)
			}
			legacyBytes := fixtureJSON(t, legacy)
			current, err := f.assess()
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []RequiredCIAssessment{current, legacy} {
				if !ValidRetainedRequiredCIAssessment(f.Authority, f.Inventory, f.Actions, f.Attempts, value) {
					t.Fatal("valid exact assessment refused")
				}
			}
			if string(legacyBytes) != string(fixtureJSON(t, legacy)) {
				t.Fatal("old retained assessment rewritten")
			}
			for _, tamper := range []string{"status_id", "check_id", "run_id", "app", "repository", "head", "reason", "aggregate", "missing_entry", "other_hold"} {
				t.Run(tamper, func(t *testing.T) {
					var changed RequiredCIAssessment
					if json.Unmarshal(legacyBytes, &changed) != nil {
						t.Fatal("fixture")
					}
					switch tamper {
					case "status_id":
						changed.Checks[0].StatusID++
					case "check_id":
						changed.Checks[0].CheckRunID++
					case "run_id":
						changed.Checks[0].RunID++
					case "app":
						changed.Checks[0].AppID++
					case "repository":
						changed.Repository = "foreign/repo"
					case "head":
						changed.HeadSHA = f.Authority.Checks[0].Context
					case "reason":
						changed.Checks[0].Reason = "ci_attempt_unproven"
					case "aggregate":
						changed.State = "satisfied"
						changed.Reason = ""
					case "missing_entry":
						changed.Checks = []RequiredCIAssessmentCheck{}
					case "other_hold":
						changed.Reason = "ci_authority_unproven"
					}
					if ValidRetainedRequiredCIAssessment(f.Authority, f.Inventory, f.Actions, f.Attempts, changed) {
						t.Fatal("tampered legacy accepted", tamper)
					}
				})
			}
			malformed := f
			malformed.Inventory.Repository = "foreign/repo"
			if ValidRetainedRequiredCIAssessment(malformed.Authority, malformed.Inventory, malformed.Actions, malformed.Attempts, legacy) {
				t.Fatal("malformed facts accepted")
			}
			if !reflect.DeepEqual(f.Authority.Checks, []RequiredCheck{{Context: "build", AppID: 400}}) {
				t.Fatal("validator changed authority")
			}
		})
	}
}
