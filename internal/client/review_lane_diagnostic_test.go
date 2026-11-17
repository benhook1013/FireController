package client_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func sampleLaneDiagnostic() client.ReviewLaneDiagnostic {
	return client.ReviewLaneDiagnostic{ProjectID: "project-one", Channel: "cli", ConfigurationRevision: 1, EvaluatedAt: "2026-11-10T21:00:00Z", Scope: "retained_source_local_policy", SourceConfigurationRevision: 1, SourceCheckedAt: "2026-11-10T20:59:00Z", SourceComplete: true, Candidates: []client.ReviewLaneCandidateDiagnostic{{PullNumber: 2, HeadSHA: strings.Repeat("a", 40), SourceObservedAt: "2026-11-10T20:58:00Z", LocalState: "held", LocalReason: "review_evidence_pending"}, {PullNumber: 4, HeadSHA: strings.Repeat("b", 40), SourceObservedAt: "2026-11-10T20:58:00Z", ParentPullNumber: 2, LocalState: "held", LocalReason: "predecessor_unfinished", BlockingPullNumber: 2, BlockingReason: "review_evidence_pending"}}}
}
func TestReviewLaneDiagnosticBoundReadAndStrictResponses(t *testing.T) {
	cases := map[string]func(*client.ReviewLaneDiagnostic){
		"valid":           func(*client.ReviewLaneDiagnostic) {},
		"project":         func(v *client.ReviewLaneDiagnostic) { v.ProjectID = "foreign" },
		"channel":         func(v *client.ReviewLaneDiagnostic) { v.Channel = "hosted" },
		"duplicate":       func(v *client.ReviewLaneDiagnostic) { v.Candidates[1].PullNumber = 2 },
		"false-freshness": func(v *client.ReviewLaneDiagnostic) { v.SourceConfigurationRevision = 2 },
		"changed-source-ready": func(v *client.ReviewLaneDiagnostic) {
			v.SourceConfigurationRevision = 2
			v.SourceComplete = false
			v.SourceReason = "configuration_changed"
		},
		"no-source-ready": func(v *client.ReviewLaneDiagnostic) {
			v.Candidates[0].HeadSHA = ""
			v.Candidates[0].SourceObservedAt = ""
			v.Candidates[0].LocalState = "locally_ready"
			v.Candidates[0].LocalReason = ""
		},
		"missing-blocker": func(v *client.ReviewLaneDiagnostic) { v.Candidates[1].BlockingPullNumber = 0 },
		"bad-time":        func(v *client.ReviewLaneDiagnostic) { v.SourceCheckedAt = "yesterday" },
		"null-candidates": func(v *client.ReviewLaneDiagnostic) { v.Candidates = nil },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			value := sampleLaneDiagnostic()
			edit(&value)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				calls++
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/lanes/cli/diagnostics" || r.URL.RawQuery != "" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("diagnostic changed method or binding")
				}
				write(w, 200, value)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			got, err := c.ReviewLaneDiagnostic(t.Context(), "cli")
			if calls != 1 {
				t.Fatal("unexpected fanout", calls)
			}
			if name == "valid" {
				if err != nil || got.Candidates[1].BlockingPullNumber != 2 {
					t.Fatal(got, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
			if _, err = c.ReviewLaneDiagnostic(t.Context(), "manual"); err == nil || calls != 1 {
				t.Fatal("invalid channel contacted service")
			}
		})
	}
}

func TestReviewLaneDiagnosticPausedPlaceholderStrictShape(t *testing.T) {
	cases := map[string]func(*client.ReviewLaneDiagnostic){
		"valid":                 func(*client.ReviewLaneDiagnostic) {},
		"complete-frame":        func(v *client.ReviewLaneDiagnostic) { v.SourceComplete = true },
		"changed-revision":      func(v *client.ReviewLaneDiagnostic) { v.SourceConfigurationRevision++ },
		"changed-configuration": func(v *client.ReviewLaneDiagnostic) { v.SourceReason = "configuration_changed" },
		"changed-repository":    func(v *client.ReviewLaneDiagnostic) { v.SourceReason = "repository_changed" },
		"unavailable-source": func(v *client.ReviewLaneDiagnostic) {
			v.SourceConfigurationRevision = 0
			v.SourceCheckedAt = ""
			v.SourceReason = "source_unavailable"
		},
		"fabricated-time": func(v *client.ReviewLaneDiagnostic) { v.Candidates[0].SourceObservedAt = v.SourceCheckedAt },
		"unpaired-head":   func(v *client.ReviewLaneDiagnostic) { v.Candidates[0].HeadSHA = strings.Repeat("a", 40) },
		"parent":          func(v *client.ReviewLaneDiagnostic) { v.Candidates[0].ParentPullNumber = 4 },
		"blocker": func(v *client.ReviewLaneDiagnostic) {
			v.Candidates[0].BlockingPullNumber = 4
			v.Candidates[0].BlockingReason = "pull_scheduling_paused"
		},
		"other-hold":     func(v *client.ReviewLaneDiagnostic) { v.Candidates[0].LocalReason = "review_evidence_pending" },
		"missing-reason": func(v *client.ReviewLaneDiagnostic) { v.Candidates[0].LocalReason = "" },
		"complete-row": func(v *client.ReviewLaneDiagnostic) {
			v.Candidates[0].LocalState = "complete"
			v.Candidates[0].LocalReason = ""
		},
		"ready-row": func(v *client.ReviewLaneDiagnostic) {
			v.Candidates[0].LocalState = "locally_ready"
			v.Candidates[0].LocalReason = ""
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			value := sampleLaneDiagnostic()
			value.SourceComplete = false
			value.Candidates = []client.ReviewLaneCandidateDiagnostic{{PullNumber: 2, LocalState: "held", LocalReason: "pull_scheduling_paused"}}
			edit(&value)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				calls++
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/lanes/cli/diagnostics" {
					t.Error("unexpected diagnostic request")
				}
				write(w, 200, value)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			got, err := c.ReviewLaneDiagnostic(t.Context(), "cli")
			if name == "valid" {
				if err != nil || got.Candidates[0].LocalReason != "pull_scheduling_paused" {
					t.Fatal(got, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
			if calls != 1 {
				t.Fatal("unexpected fanout", calls)
			}
		})
	}
}

func TestReviewLaneDiagnosticPendingSelectionStrictShape(t *testing.T) {
	cases := map[string]func(*client.ReviewLaneDiagnostic){
		"queued":          func(v *client.ReviewLaneDiagnostic) {},
		"selecting":       func(v *client.ReviewLaneDiagnostic) { v.PendingSelection.State = "selecting" },
		"omitted":         func(v *client.ReviewLaneDiagnostic) { v.PendingSelection = nil },
		"bad-id":          func(v *client.ReviewLaneDiagnostic) { v.PendingSelection.ID = "review_operation_wrong" },
		"selected":        func(v *client.ReviewLaneDiagnostic) { v.PendingSelection.State = "selected" },
		"held":            func(v *client.ReviewLaneDiagnostic) { v.PendingSelection.State = "held" },
		"missing-created": func(v *client.ReviewLaneDiagnostic) { v.PendingSelection.CreatedAt = "" },
		"bad-updated":     func(v *client.ReviewLaneDiagnostic) { v.PendingSelection.UpdatedAt = "yesterday" },
		"reversed-times":  func(v *client.ReviewLaneDiagnostic) { v.PendingSelection.UpdatedAt = "2026-11-10T19:00:00Z" },
		"cli":             func(v *client.ReviewLaneDiagnostic) { v.Channel = "cli" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			value := sampleLaneDiagnostic()
			value.Channel = "hosted"
			value.PendingSelection = &client.ReviewLanePendingSelection{ID: "review_selection_" + strings.Repeat("a", 32), State: "queued", CreatedAt: "2026-11-10T20:00:00Z", UpdatedAt: "2026-11-10T20:01:00Z"}
			edit(&value)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/instance") {
					handshake(w)
					return
				}
				write(w, 200, value)
			}))
			defer srv.Close()
			c := newClient(t, srv)
			_, err := c.ReviewLaneDiagnostic(t.Context(), value.Channel)
			if name == "queued" || name == "selecting" || name == "omitted" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		})
	}
}
