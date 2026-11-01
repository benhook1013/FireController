package reviewprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCIResultsSkippedSyntheticStartsPreserveMixedInventory(t *testing.T) {
	rows := []map[string]any{ciCheckFixture(1)}
	for i := int64(2); i <= 90; i++ {
		check := ciCheckFixture(i)
		check["name"], check["conclusion"] = "optional skipped "+fmt.Sprint(i), "skipped"
		check["started_at"], check["completed_at"] = "2026-01-01T00:16:00Z", "2026-01-01T00:00:37Z"
		rows = append(rows, check)
	}
	g, _ := ciFixtureServer(t, ciJSON(ciEnvelope(rows, len(rows))), `[]`)
	got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
	if err != nil || len(got.Checks) != len(rows) {
		t.Fatal("skipped results concealed the required check inventory", len(got.Checks), err)
	}
	if got.Checks[0].Conclusion != "success" || got.Checks[1].Conclusion != "skipped" || !got.Checks[1].CompletedAt.Before(got.Checks[1].StartedAt) {
		t.Fatal("provider timestamps or distinct conclusions were rewritten")
	}
}

func TestCurrentActionsAttemptSkippedSyntheticStartRemainsExactJob(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := actionsDefaultResponse(r)
		if r.URL.Path == actionsJobsPath {
			skipped := actionsJobFixture(71)
			skipped["conclusion"], skipped["started_at"], skipped["completed_at"] = "skipped", "2026-01-01T00:16:00Z", "2026-01-01T00:00:37Z"
			body = actionsEnvelope("jobs", []map[string]any{actionsJobFixture(70), skipped}, 2)
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()
	got, err := testGitHub(server, "synthetic-secret").CurrentActionsAttempt(t.Context(), "owner/repo", actionsHead, 40)
	if err != nil || got.State != "complete" || len(got.Jobs) != 2 || got.Jobs[1].CheckRunID != 1071 || !got.Jobs[1].CompletedAt.Before(got.Jobs[1].StartedAt) {
		t.Fatal("skipped job lost current attempt or original timestamps", got, err)
	}
}

func TestAssessRequiredCIMixedSkippedSyntheticTimesDoNotHideRequiredOutcome(t *testing.T) {
	for _, conclusion := range []string{"success", "failure", "skipped"} {
		t.Run(conclusion, func(t *testing.T) {
			f := requiredCIFixture()
			setCIJobState(&f, "completed", conclusion)
			if conclusion == "skipped" {
				f.Inventory.Checks[0].CompletedAt = f.Inventory.Checks[0].StartedAt.Add(-time.Second)
				f.Attempts[0].Jobs[0].CompletedAt = f.Attempts[0].Jobs[0].StartedAt.Add(-time.Second)
			}
			for i := int64(1); i <= 99; i++ {
				check := f.Inventory.Checks[0]
				check.ID, check.Name, check.Conclusion = 300+i, "optional skipped "+fmt.Sprint(i), "skipped"
				check.CompletedAt = check.StartedAt.Add(-15 * time.Minute)
				f.Inventory.Checks = append(f.Inventory.Checks, check)
				job := f.Attempts[0].Jobs[0]
				job.ID, job.CheckRunID, job.Name, job.Conclusion = 500+i, check.ID, check.Name, "skipped"
				job.CompletedAt = job.StartedAt.Add(-15 * time.Minute)
				f.Attempts[0].Jobs = append(f.Attempts[0].Jobs, job)
			}
			before := string(fixtureJSON(t, f))
			got, err := f.assess()
			want := "satisfied"
			if conclusion == "failure" {
				want = "failed"
			}
			if err != nil || got.State != want || len(got.Checks) != 1 || got.Checks[0].CheckRunID != 300 || got.Checks[0].RunAttempt != 2 {
				t.Fatal("optional skipped inventory changed required current-job outcome", got, err)
			}
			if string(fixtureJSON(t, f)) != before {
				t.Fatal("assessment rewrote original facts")
			}
		})
	}
	for _, conclusion := range []string{"success", "failure", "neutral", "cancelled", "future"} {
		f := requiredCIFixture()
		setCIJobState(&f, "completed", conclusion)
		f.Inventory.Checks[0].CompletedAt = f.Inventory.Checks[0].StartedAt.Add(-time.Second)
		if _, err := f.assess(); err == nil {
			t.Fatal("non-skipped reversed execution interval accepted", conclusion)
		}
	}
}
