package reviewprovider

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Exercise the real collector, including both result reads, numbered Actions
// jobs and the pure assessment. Optional cancelled jobs must not conceal the
// required result, and a required cancellation must never become passing CI.
func TestObserveRequiredCICancelledTimesPreserveInventoryAndRequiredOutcome(t *testing.T) {
	for _, conclusion := range []string{"success", "failure", "skipped", "cancelled"} {
		t.Run(conclusion, func(t *testing.T) {
			var controlCounts map[string]int
			for _, reversed := range []bool{false, true} {
				f := newCIObservationFixture(t, "fallback")
				underlying := f.g.httpClient.Transport
				if underlying == nil {
					underlying = http.DefaultTransport
				}
				start, end := "2026-01-01T05:05:23Z", "2026-01-01T05:05:22Z"
				if !reversed {
					end = "2026-01-01T05:05:24Z"
				}
				f.g.httpClient.Transport = ciFailureStageTransport(func(r *http.Request) (*http.Response, error) {
					response, err := underlying.RoundTrip(r)
					isChecks := strings.HasSuffix(r.URL.Path, "/commits/"+candidateHead+"/check-runs")
					isJobs := r.URL.Path == actionsJobsPath
					if err != nil || !isChecks && !isJobs {
						return response, err
					}
					var body map[string]any
					if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					key := "check_runs"
					if isJobs {
						key = "jobs"
					}
					rows := body[key].([]any)
					required := rows[0].(map[string]any)
					required["conclusion"] = conclusion
					if conclusion == "skipped" || conclusion == "cancelled" {
						required["started_at"], required["completed_at"] = start, end
					}
					for i, optionalConclusion := range []string{"skipped", "cancelled"} {
						row := ciCheckFixture(int64(1071 + i))
						row["check_suite"] = map[string]any{"id": 40}
						if isJobs {
							row = actionsJobFixture(int64(71 + i))
						}
						row["name"], row["head_sha"], row["conclusion"] = "optional "+optionalConclusion, candidateHead, optionalConclusion
						row["started_at"], row["completed_at"] = start, end
						rows = append(rows, row)
					}
					body[key], body["total_count"] = rows, len(rows)
					encoded, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					response.Body = io.NopCloser(strings.NewReader(string(encoded)))
					response.ContentLength = int64(len(encoded))
					return response, nil
				})
				got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
				want := "satisfied"
				if conclusion == "failure" || conclusion == "cancelled" {
					want = "failed"
				}
				if err != nil || got.State != want || got.Assessment.State != want || got.Selection != "head" || got.SelectedCommitSHA != candidateHead || got.Results == nil || len(got.Results.Checks) != 3 || len(got.Attempts) != 1 || len(got.Attempts[0].Jobs) != 3 || len(got.Assessment.Checks) != 1 {
					t.Fatal("terminal timestamps concealed or changed the required outcome", got, err)
				}
				required := got.Assessment.Checks[0]
				if required.Context != "build" || required.AppID != 10 || required.CheckRunID != 1070 || required.RunID != 50 || required.RunAttempt != 2 || required.State != want || got.Results.Checks[0].Conclusion != conclusion || got.Attempts[0].Jobs[0].Conclusion != conclusion {
					t.Fatal("required identity or conclusion changed", required)
				}
				for i, optionalConclusion := range []string{"skipped", "cancelled"} {
					check, job := got.Results.Checks[i+1], got.Attempts[0].Jobs[i+1]
					if check.Conclusion != optionalConclusion || job.Conclusion != optionalConclusion || check.ID != job.CheckRunID || check.StartedAt.Format(time.RFC3339) != start || check.CompletedAt.Format(time.RFC3339) != end || job.StartedAt != check.StartedAt || job.CompletedAt != check.CompletedAt {
						t.Fatal("original optional facts were dropped or rewritten", check, job)
					}
				}
				_, counts := f.snapshot()
				if !reversed {
					controlCounts = counts
				} else if !reflect.DeepEqual(counts, controlCounts) {
					t.Fatal("terminal timestamp handling changed provider requests", counts, controlCounts)
				}
			}
		})
	}
}

func TestAssessRequiredCICancelledTimesDoNotRelaxCurrentJobIdentity(t *testing.T) {
	for name, change := range map[string]func(*ciAssessmentFixture){
		"missing current job":  func(f *ciAssessmentFixture) { f.Attempts[0].Jobs = []ActionsJob{} },
		"wrong check identity": func(f *ciAssessmentFixture) { f.Attempts[0].Jobs[0].CheckRunID++ },
		"wrong required App":   func(f *ciAssessmentFixture) { f.Inventory.Checks[0].AppID++ },
		"changed conclusion": func(f *ciAssessmentFixture) {
			f.Attempts[0].Jobs[0].Conclusion = "success"
			f.Attempts[0].Jobs[0].CompletedAt = f.Attempts[0].Jobs[0].StartedAt.Add(time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := requiredCIFixture()
			setCIJobState(&f, "completed", "cancelled")
			f.Inventory.Checks[0].CompletedAt = f.Inventory.Checks[0].StartedAt.Add(-time.Second)
			f.Attempts[0].Jobs[0].CompletedAt = f.Inventory.Checks[0].CompletedAt
			change(&f)
			before := string(fixtureJSON(t, f))
			got, err := f.assess()
			if err != nil || got.State != "unproven" || len(got.Checks) != 1 || string(fixtureJSON(t, f)) != before {
				t.Fatal("cancelled timestamps bypassed current-job proof or rewrote facts", got, err)
			}
		})
	}
}
