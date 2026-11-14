package client_test

import (
	"github.com/benhook1013/FireController/internal/client"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReviewSelectionDecisionHistoryReadContract(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid", true: "foreign"}[bad], func(t *testing.T) {
			value := client.ReviewSelectionDecisionHistory{ProjectID: "project-one", Channel: "cli", RetentionDays: 7, MaximumEntries: 256, Items: []client.ReviewSelectionDecision{{Sequence: 4, Phase: "automatic_plan", Outcome: "no_new_intent", Candidates: []client.ReviewSelectionCandidateDecision{}, FirstSeenAt: "2026-11-14T00:00:00Z", LastSeenAt: "2026-11-14T00:01:00Z", Occurrences: 2}}}
			if bad {
				value.ProjectID = "foreign"
			}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				calls++
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/lanes/cli/decisions" || r.URL.Query().Get("before") != "5" || r.URL.Query().Get("limit") != "2" {
					t.Error("wrong scoped history request")
				}
				write(w, 200, value)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			_, err := c.ReviewSelectionDecisionHistory(t.Context(), "cli", 5, 2)
			if bad {
				checkError(t, err, "invalid_response")
			} else if err != nil {
				t.Fatal(err)
			}
			if _, err = c.ReviewSelectionDecisionHistory(t.Context(), "cli", 0, 101); err == nil || calls != 1 {
				t.Fatal("unbounded read reached server")
			}
		})
	}
}
