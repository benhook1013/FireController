package reviewprovider

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCIResultsTimeOrderIssuePreservesValidationAndErrorCode(t *testing.T) {
	for _, mode := range []string{"success", "failure", "skipped", "cancelled", "malformed time", "invalid identity", "cancelled malformed start", "cancelled malformed end", "cancelled missing end", "cancelled invalid identity", "nonterminal cancelled"} {
		t.Run(mode, func(t *testing.T) {
			row := ciCheckFixture(1)
			row["started_at"], row["completed_at"] = "2026-01-01T01:00:00Z", "2026-01-01T00:00:00Z"
			want := "check_run_time_order"
			switch mode {
			case "failure", "skipped", "cancelled":
				row["conclusion"] = mode
			case "malformed time":
				row["completed_at"], want = "PRIVATE not a timestamp", ""
			case "invalid identity":
				row["id"], want = 0, ""
			case "cancelled malformed start":
				row["conclusion"], row["started_at"], want = "cancelled", "PRIVATE not a timestamp", ""
			case "cancelled malformed end":
				row["conclusion"], row["completed_at"], want = "cancelled", "PRIVATE not a timestamp", ""
			case "cancelled missing end":
				row["conclusion"], row["completed_at"], want = "cancelled", nil, ""
			case "cancelled invalid identity":
				row["conclusion"], row["id"], want = "cancelled", 0, ""
			case "nonterminal cancelled":
				row["status"], row["conclusion"], want = "in_progress", "cancelled", ""
			}
			g, calls := ciFixtureServer(t, ciJSON(ciEnvelope([]map[string]any{row}, 1)), `[]`)
			got, err := g.CIResults(t.Context(), "owner/repo", ciHead)
			if mode == "skipped" || mode == "cancelled" {
				if err != nil || len(got.Checks) != 1 || got.Checks[0].Conclusion != mode || !got.Checks[0].CompletedAt.Before(got.Checks[0].StartedAt) || calls.Load() != 4 {
					t.Fatal("supported terminal interval changed", got, err, calls.Load())
				}
				return
			}
			assertNoCIInventory(t, got, err)
			var cause *Error
			if !errors.As(err, &cause) || cause.Code != "provider_invalid_response" || CIResultIssue(err) != want || calls.Load() != 2 || strings.Contains(err.Error(), "2026-") {
				t.Fatal("invalid inventory changed classification, leaked data or retried", err, CIResultIssue(err), calls.Load())
			}
		})
	}
}

func TestCICollectionTimeOrderIssueKeepsExactStageAndNoCredit(t *testing.T) {
	f := newCIObservationFixture(t, "fallback")
	underlying := f.g.httpClient.Transport
	if underlying == nil {
		underlying = http.DefaultTransport
	}
	hits := 0
	f.g.httpClient.Transport = ciFailureStageTransport(func(r *http.Request) (*http.Response, error) {
		response, err := underlying.RoundTrip(r)
		if err != nil || !strings.HasSuffix(r.URL.Path, "/commits/"+candidateHead+"/check-runs") {
			return response, err
		}
		hits++
		var body map[string]any
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		row := body["check_runs"].([]any)[0].(map[string]any)
		row["started_at"], row["completed_at"] = "2026-01-01T01:00:00Z", "2026-01-01T00:00:00Z"
		encoded, _ := json.Marshal(body)
		response.Body = io.NopCloser(strings.NewReader(string(encoded)))
		response.ContentLength = int64(len(encoded))
		return response, nil
	})
	got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
	var cause *Error
	if !errors.As(err, &cause) || cause.Code != "provider_invalid_response" || CICollectionStage(err) != "head_results" || CIResultIssue(err) != "check_run_time_order" || got.State != "unproven" || got.Results != nil || got.Assessment.State != "" || hits != 1 {
		t.Fatal("known issue lost stage, credited partial facts or recollected", got.State, err, hits)
	}
}

func TestCIResultIssueRejectsUnrecognizedText(t *testing.T) {
	for _, issue := range []string{"", "PRIVATE endpoint field", "https://provider.invalid/PRIVATE", "check_run_time_order_invented"} {
		err := &ciResultIssueError{issue: issue, cause: &Error{Code: "provider_invalid_response", Message: "PRIVATE"}}
		if CIResultIssue(err) != "" || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("unrecognized result detail escaped")
		}
	}
}
