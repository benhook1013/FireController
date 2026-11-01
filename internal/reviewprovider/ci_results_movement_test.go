package reviewprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCIResultsRecollectsOnlyCountMovement(t *testing.T) {
	for _, mode := range []string{"suite-settles", "checks-settle", "checks-keep-moving", "overlap-settles", "overlap-keeps-moving", "duplicate-page-drift", "malformed", "malformed-drift", "foreign-drift", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var origin string
			var starts, suites, checks atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case ciSuitesPath:
					n := suites.Add(1)
					total := 1
					if mode == "suite-settles" && n == 2 {
						total = 2
					}
					_ = json.NewEncoder(w).Encode(ciSuiteCountFixture(total))
				case ciChecksPath:
					checks.Add(1)
					if mode == "malformed" {
						_, _ = w.Write([]byte(`{"check_runs":null}`))
						return
					}
					page := r.URL.Query().Get("page")
					if page == "1" {
						starts.Add(1)
					}
					if mode == "suite-settles" {
						// Only the replacement inventory may escape to callers.
						_ = json.NewEncoder(w).Encode(ciEnvelope([]map[string]any{ciCheckFixture(starts.Load())}, 1))
						return
					}
					total, id := 2, int64(1)
					if page == "1" {
						w.Header().Set("Link", fmt.Sprintf("<%s%s?filter=all&page=2&per_page=100>; rel=next", origin, ciChecksPath))
					} else {
						id = 2
						if starts.Load() == 1 || mode == "checks-keep-moving" || mode == "overlap-keeps-moving" {
							total = 3
						}
						if mode == "canceled" {
							cancel()
						}
					}
					if page == "2" && total == 3 && (mode == "overlap-settles" || mode == "overlap-keeps-moving") {
						id = 1
					}
					row := ciCheckFixture(id)
					if page == "2" && mode == "malformed-drift" {
						row["head_sha"] = "invalid"
					}
					if page == "2" && mode == "foreign-drift" {
						w.Header().Set("Link", "<https://foreign.invalid/check-runs?page=3>; rel=next")
					}
					rows := []map[string]any{row}
					if page == "2" && mode == "duplicate-page-drift" {
						rows = append(rows, row)
					}
					_ = json.NewEncoder(w).Encode(ciEnvelope(rows, total))
				case ciStatusesPath:
					_, _ = w.Write([]byte(`[]`))
				default:
					t.Error("unexpected selector", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			origin = server.URL
			got, err := testGitHub(server, "").CIResults(ctx, "owner/repo", ciHead)
			switch mode {
			case "suite-settles":
				if err != nil || starts.Load() != 2 || suites.Load() != 4 || len(got.Checks) != 1 || got.Checks[0].ID != 2 {
					t.Fatal("mixed or lost replacement inventory", got, err, starts.Load(), suites.Load())
				}
			case "checks-settle", "overlap-settles":
				if err != nil || starts.Load() != 2 || checks.Load() != 4 || len(got.Checks) != 2 || got.HeadSHA != ciHead {
					t.Fatal("same-head movement did not recover", got, err, starts.Load(), checks.Load())
				}
			default:
				assertNoCIInventory(t, got, err)
				want := int64(2)
				if mode == "checks-keep-moving" || mode == "overlap-keeps-moving" {
					want = 4
					if !hasCode(err, "provider_observation_changed") {
						t.Fatal("lost bounded movement diagnostic", err)
					}
				} else if mode == "malformed" {
					want = 1
				}
				if checks.Load() != want {
					t.Fatal("unexpected recollection", checks.Load(), want)
				}
			}
		})
	}
}

func TestCIObservationRecoversMovingHeadInventory(t *testing.T) {
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
		if hits > 2 {
			return response, nil
		}
		response.Body.Close()
		rows := f.checks(candidateHead, 1)
		rows[0]["id"] = int64(9999) // Cross-page overlap must not participate in final current-attempt proof.
		total := 2
		if hits == 1 {
			link := *r.URL
			q := link.Query()
			q.Set("page", "2")
			link.RawQuery = q.Encode()
			response.Header.Set("Link", "<"+link.String()+">; rel=next")
		} else {
			total = 3
		}
		body, _ := json.Marshal(ciEnvelope(rows, total))
		response.Body = io.NopCloser(strings.NewReader(string(body)))
		response.ContentLength = int64(len(body))
		return response, nil
	})
	got, err := f.g.ObserveRequiredCI(t.Context(), ciObservationTarget())
	if err != nil || got.State != "satisfied" || got.Selection != "head" || got.Results == nil || len(got.Results.Checks) != 1 || got.Results.Checks[0].ID != 1070 || hits != 4 {
		t.Fatal("same-comparison recovery bypassed final CI proof", got.State, got.Selection, hits, err)
	}
}
