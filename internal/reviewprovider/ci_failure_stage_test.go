package reviewprovider

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type ciFailureStageTransport func(*http.Request) (*http.Response, error)

func (f ciFailureStageTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCICollectionFailureReportsExactSafeStage(t *testing.T) {
	for _, tc := range []struct {
		name, mode, path, stage string
		nth                     int
	}{
		{"authority", "merge", "/branches/main/protection", "authority", 1},
		{"candidate", "merge", candidatePullPath, "candidate", 1},
		{"merge-results", "merge", "/commits/" + candidateMerge + "/check-runs", "merge_results", 1},
		{"head-results", "fallback", "/commits/" + candidateHead + "/check-runs", "head_results", 1},
		{"attempt", "merge", "/actions/runs/50/attempts/2/jobs", "attempt", 1},
		{"authority-recheck", "merge", "/branches/main/protection", "authority_recheck", 2},
		{"candidate-recheck", "merge", candidatePullPath, "candidate_recheck", 3},
		{"results-recheck", "merge", "/commits/" + candidateMerge + "/check-runs", "results_recheck", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCIObservationFixture(t, tc.mode)
			underlying := f.g.httpClient.Transport
			if underlying == nil {
				underlying = http.DefaultTransport
			}
			hits := 0
			f.g.httpClient.Transport = ciFailureStageTransport(func(r *http.Request) (*http.Response, error) {
				response, err := underlying.RoundTrip(r)
				if err != nil {
					return response, err
				}
				if strings.HasSuffix(r.URL.Path, tc.path) {
					hits++
					if hits == tc.nth {
						response.Body.Close()
						response.Body = io.NopCloser(strings.NewReader("PRIVATE_PROVIDER_BODY"))
						response.ContentLength = int64(len("PRIVATE_PROVIDER_BODY"))
					}
				}
				return response, nil
			})
			got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
			var cause *Error
			if err == nil || !errors.As(err, &cause) || cause.Code != "provider_invalid_response" || CICollectionStage(err) != tc.stage || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatalf("stage=%q expected=%q hits=%d err=%v", CICollectionStage(err), tc.stage, hits, err)
			}
			if got.State == "satisfied" || got.State == "not_required" || hits != tc.nth {
				t.Fatal("failure credited or retried", got.State, hits)
			}
		})
	}
}

func TestCICollectionStageRejectsUnrecognizedText(t *testing.T) {
	for _, stage := range []string{"", "PRIVATE url credential", "https://provider.example/private", "attempt_recheck_invented"} {
		err := &ciCollectionError{stage: stage, cause: &Error{Code: "provider_invalid_response", Message: "PRIVATE"}}
		if CICollectionStage(err) != "" || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("unsafe stage")
		}
	}
}
