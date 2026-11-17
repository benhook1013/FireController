package reviewprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSuiteQueryOptionalCostNeverChangesValidCI(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cost           any
		present, known bool
	}{{"known", 1, true, true}, {"missing", nil, false, false}, {"null", nil, true, false}, {"string", "PRIVATE_COST", true, false}, {"negative", -1, true, false}, {"zero", 0, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var request struct{ Query string }
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if calls == 1 && !strings.Contains(request.Query, "rateLimit{cost}") {
					t.Error("missing same-query cost field")
				}
				body := suiteBatchEnvelope(suiteBatchNode(ciHead, 0), suiteBatchNode(strings.Repeat("d", 40), 0))
				if tc.present {
					body["data"].(map[string]any)["rateLimit"] = map[string]any{"cost": tc.cost}
				}
				w.Header().Set("X-RateLimit-Resource", "graphql")
				json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			g := testGitHub(server, "")
			q := NewGitHubQuota()
			g.SetQuotaTelemetry(q)
			result := g.ciSuiteCountsBatch(t.Context(), "owner/repo", [2]string{ciHead, strings.Repeat("d", 40)}, [2]*actionsBudget{{}, {}})
			if result[0].Err != nil || result[1].Err != nil || result[0].Count != 0 || result[1].Count != 0 {
				t.Fatal("diagnostic changed CI", result)
			}
			if calls != 1 {
				t.Fatal("cost added provider request", calls)
			}
			row := q.rows[githubQuotaKey{"other", "suite_count_graphql", "graphql"}]
			if row.requests != 1 || row.details["suite_count_graphql"].Requests != 1 || q.completions[githubCompletionKey{"other", "suite_count_graphql"}].Completed != 1 {
				t.Fatal("physical count attribution", row, q.completions)
			}
			if tc.known {
				if row.suiteCostKnown != 1 || row.suiteCostUnknown != 0 || row.suitePointsSum != 1 || row.suitePointsMax != 1 {
					t.Fatal(row)
				}
			} else if row.suiteCostKnown != 0 || row.suiteCostUnknown != 1 || row.suitePointsSum != 0 {
				t.Fatal("unknown presented as cost", row)
			}
			_, _, _, err := g.requestBytes(t.Context(), http.MethodPost, "/graphql", nil, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			legacy := q.rows[githubQuotaKey{"other", "pull_graphql", "graphql"}]
			if calls != 2 || legacy.requests != 1 || legacy.suiteCostKnown != 0 || legacy.suiteCostUnknown != 0 {
				t.Fatal("legacy accounting changed", calls, legacy)
			}
		})
	}
}
func TestSuiteQueryCostParserBoundedAndStrict(t *testing.T) {
	for _, data := range []string{`{"data":{"rateLimit":{"cost":1,"cost":2}}}`, `{"data":{"rateLimit":{"cost":1.5}}}`, `{"data":{"rateLimit":{"cost":1000000001}}}`, strings.Repeat(" ", 64<<10) + `{"data":{"rateLimit":{"cost":1}}}`} {
		if _, known := suiteQueryCost([]byte(data)); known {
			t.Fatal("invalid diagnostic known")
		}
	}
}
