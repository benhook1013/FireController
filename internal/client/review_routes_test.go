package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

const routeClientID = "review_route_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func sampleRouteDetail() client.ReviewRouteDetail {
	source := sampleControlEvidence()
	stamp := source.RecordedAt
	return client.ReviewRouteDetail{Route: client.ReviewRoute{ID: routeClientID, ProjectID: "project-one", RepositoryID: source.Anchor.RepositoryID, SourceEvidenceID: source.ID, Finding: 0, SourcePullNumber: source.Anchor.PullNumber, SourceHeadSHA: source.Anchor.CandidateSHA, TargetPullNumber: 4, State: "active", Revision: 1, Disposition: "unadjudicated", RoutingReason: "Captured finding belongs to this receiver.", RoutingActorID: "source-actor", CreatedAt: stamp, UpdatedAt: stamp}, SourceAnchor: source.Anchor, SourceFinding: source.Capture.Findings[0]}
}

// Controlled response fixtures validate the client, not domain authority.
func TestReviewRoutesClientExplicitDetailAndPagedRead(t *testing.T) {
	detail := sampleRouteDetail()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Method != "GET" || r.Header.Get("X-FC-Instance") != "instance-one" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Idempotency-Key") != "" {
			t.Error("route read lost binding/changed effects")
		}
		if strings.HasSuffix(r.URL.Path, "/"+routeClientID) {
			write(w, 200, detail)
			return
		}
		if r.URL.Path != "/api/v1/projects/project-one/reviews/routes" || r.URL.Query().Get("pull_number") != "4" || r.URL.Query().Get("limit") != "1" {
			t.Error("route list lost receiver/page")
		}
		item := client.ReviewFindingRouteSummary{ID: routeClientID, Revision: 1, SourceEvidenceID: detail.Route.SourceEvidenceID, Finding: 0, SourcePullNumber: 2, TargetPullNumber: 4, Disposition: "unadjudicated", CreatedAt: detail.Route.CreatedAt}
		page := client.ReviewRoutePage{ProjectID: "project-one", PullNumber: 4, Items: []client.ReviewFindingRouteSummary{item}}
		if r.URL.Query().Get("cursor") == "" {
			page.Cursor = "opaque-project-bound-cursor"
			page.HasMore = true
		} else {
			if r.URL.Query().Get("cursor") != "opaque-project-bound-cursor" {
				t.Error("cursor changed")
			}
			page.Items = []client.ReviewFindingRouteSummary{}
		}
		write(w, 200, page)
	}))
	defer server.Close()
	c := newClient(t, server)
	got, err := c.ReviewFindingRoute(t.Context(), routeClientID)
	if err != nil || got.SourceFinding.Detail != detail.SourceFinding.Detail || got.Route.RoutingReason != detail.Route.RoutingReason {
		t.Fatal("explicit original source not preserved", got, err)
	}
	first, err := c.ReviewFindingRoutes(t.Context(), 4, client.PageOptions{Limit: 1})
	if err != nil || !first.HasMore || len(first.Items) != 1 || calls.Load() != 3 {
		t.Fatal("route read paged implicitly", first, err, calls.Load())
	}
	last, err := c.ReviewFindingRoutes(t.Context(), 4, client.PageOptions{Limit: 1, Cursor: first.Cursor})
	if err != nil || last.HasMore || last.Cursor != "" || len(last.Items) != 0 || calls.Load() != 4 {
		t.Fatal("explicit route cursor failed", last, err)
	}
}

func TestReviewRoutesClientRejectsWrongSourceOrReceiverProof(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*client.ReviewRouteDetail)
	}{
		{"project", func(v *client.ReviewRouteDetail) { v.Route.ProjectID = "other-project" }},
		{"id", func(v *client.ReviewRouteDetail) { v.Route.ID = "review_route_" + strings.Repeat("b", 32) }},
		{"repository", func(v *client.ReviewRouteDetail) { v.SourceAnchor.RepositoryID++ }},
		{"source-pull", func(v *client.ReviewRouteDetail) { v.SourceAnchor.PullNumber = 3 }},
		{"source-head", func(v *client.ReviewRouteDetail) { v.SourceAnchor.CandidateSHA = strings.Repeat("c", 40) }},
		{"finding", func(v *client.ReviewRouteDetail) { v.SourceFinding.Finding = 1 }},
		{"receiver-source", func(v *client.ReviewRouteDetail) { v.Route.TargetPullNumber = 2 }},
		{"source-text-control", func(v *client.ReviewRouteDetail) { v.SourceFinding.Detail = "bad\x00text" }},
		{"source-location", func(v *client.ReviewRouteDetail) { v.SourceFinding.StartLine = 12; v.SourceFinding.EndLine = 3 }},
		{"unadjudicated-proof", func(v *client.ReviewRouteDetail) { v.Route.FixSHA = strings.Repeat("f", 40) }},
		{"decision-without-actor", func(v *client.ReviewRouteDetail) {
			v.Route.Disposition = "accepted"
			v.Route.Reason = "Receiver confirms"
			v.Route.DecidedAt = v.Route.UpdatedAt
		}},
		{"orphan-proof", func(v *client.ReviewRouteDetail) { v.Route.ProofNote = "orphan" }},
		{"malformed-time", func(v *client.ReviewRouteDetail) { v.Route.CreatedAt = "not-a-time" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			v := sampleRouteDetail()
			test.edit(&v)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, v)
			}))
			defer server.Close()
			c := newClient(t, server)
			_, err := c.ReviewFindingRoute(t.Context(), routeClientID)
			checkError(t, err, "invalid_response")
		})
	}
	for _, state := range []string{"active", "superseded"} {
		t.Run("valid-"+state, func(t *testing.T) {
			v := sampleRouteDetail()
			v.Route.State = state
			v.Route.Revision = 2
			v.Route.Disposition = "rejected"
			v.Route.Reason = "Verified receiver rejection"
			v.Route.ActorID = "receiver"
			v.Route.DecidedAt = v.Route.UpdatedAt
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, v)
			}))
			defer server.Close()
			c := newClient(t, server)
			got, err := c.ReviewFindingRoute(t.Context(), routeClientID)
			if err != nil || got.Route.State != state {
				t.Fatal("retained superseded state unreadable", err)
			}
		})
	}
}

func TestReviewRoutesClientMutationReceiptsAndUnknownNoRetry(t *testing.T) {
	var posts atomic.Int64
	var malformed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		posts.Add(1)
		if r.Method != "POST" || r.Header.Get("Idempotency-Key") != "retained-route-key" || r.Header.Get("X-FC-Worker") != "worker-one" {
			t.Error("route mutation unbound/key changed")
		}
		var wire map[string]any
		if json.NewDecoder(r.Body).Decode(&wire) != nil {
			t.Error("invalid body")
		}
		if malformed.Load() {
			write(w, 200, map[string]any{"id": routeClientID})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/routes") {
			if wire["finding"] != float64(0) || wire["target_pull_number"] != float64(4) || wire["expected_route_revision"] != float64(2) {
				t.Error("route guard/index lost")
			}
			write(w, 200, client.ReviewRouteReceipt{ID: routeClientID, State: "routed", Revision: 1, SourceRevision: 4})
			return
		}
		state := "adjudicated"
		revision := int64(3)
		if strings.HasSuffix(r.URL.Path, "/fixes") {
			state = "fix_recorded"
			revision = 4
			if wire["expected_sha"] != "" || wire["proof_note"] != "Exact receiver proof" {
				t.Error("receiver proof assertion lost")
			}
		}
		write(w, 200, client.ReviewControlReceipt{ID: routeClientID, State: state, Revision: revision})
	}))
	defer server.Close()
	c := newClient(t, server)
	request := client.ReviewRouteRequest{ExpectedRevision: 3, Finding: 0, TargetPullNumber: 4, Reason: "Exact source routing reason", ExpectedRouteRevision: 2}
	got, err := c.CreateReviewFindingRoute(t.Context(), "retained-route-key", controlEvidenceID, request)
	if err != nil || got.SourceRevision != 4 {
		t.Fatal(got, err)
	}
	if _, err = c.DecideReviewFindingRoute(t.Context(), "retained-route-key", routeClientID, client.ReviewRouteDecisionRequest{ExpectedRevision: 2, Disposition: "accepted", Reason: "Receiver confirms"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.FixReviewFindingRoute(t.Context(), "retained-route-key", routeClientID, client.ReviewRouteFixRequest{ExpectedRevision: 3, ExpectedSHA: "", FixSHA: strings.Repeat("f", 40), ProofNote: "Exact receiver proof"}); err != nil {
		t.Fatal(err)
	}
	malformed.Store(true)
	_, err = c.CreateReviewFindingRoute(t.Context(), "retained-route-key", controlEvidenceID, request)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || posts.Load() != 4 {
		t.Fatal("bad committed receipt retried or lost uncertainty", err, posts.Load())
	}
	before := posts.Load()
	for _, f := range []func() error{
		func() error {
			_, e := c.CreateReviewFindingRoute(context.Background(), "key", controlEvidenceID, client.ReviewRouteRequest{Finding: 100, TargetPullNumber: 4, Reason: "bad"})
			return e
		},
		func() error {
			_, e := c.DecideReviewFindingRoute(context.Background(), "key", routeClientID, client.ReviewRouteDecisionRequest{ExpectedRevision: 1, Disposition: "routed", Reason: "bad"})
			return e
		},
		func() error {
			_, e := c.FixReviewFindingRoute(context.Background(), "key", routeClientID, client.ReviewRouteFixRequest{ExpectedRevision: 1, ExpectedSHA: "bad", FixSHA: strings.Repeat("f", 40), ProofNote: "bad"})
			return e
		},
	} {
		if f() == nil {
			t.Fatal("invalid route mutation accepted")
		}
	}
	if posts.Load() != before {
		t.Fatal("invalid routes performed HTTP")
	}
}

func TestReviewRoutesClientEscapedFullDetailWithinBound(t *testing.T) {
	v := sampleRouteDetail()
	v.SourceFinding.Detail = strings.Repeat("<", 16<<10)
	v.Route.RoutingReason = strings.Repeat("<", 2048)
	v.Route.Disposition = "accepted"
	v.Route.Revision = 3
	v.Route.Reason = strings.Repeat("<", 2048)
	v.Route.ActorID = "receiver"
	v.Route.DecidedAt = v.Route.UpdatedAt
	v.Route.FixSHA = strings.Repeat("f", 40)
	v.Route.ProofNote = strings.Repeat("<", 2048)
	v.Route.FixedAt = v.Route.UpdatedAt
	encoded, _ := json.Marshal(v)
	if len(encoded) <= 64<<10 || len(encoded) > 256<<10 {
		t.Fatal("fixture does not exercise valid escaped detail budget", len(encoded))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		write(w, 200, v)
	}))
	defer server.Close()
	c := newClient(t, server)
	got, err := c.ReviewFindingRoute(t.Context(), routeClientID)
	if err != nil || got.SourceFinding.Detail != v.SourceFinding.Detail || got.Route.ProofNote != v.Route.ProofNote {
		t.Fatal("valid full route detail was truncated/refused", err)
	}
}

func TestReviewRoutesClientRejectsMalformedDetailAndPages(t *testing.T) {
	for _, mode := range []string{"missing", "null", "alias", "unknown", "wrong-receiver-page", "duplicate-page"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				v := sampleRouteDetail()
				data, _ := json.Marshal(v)
				var fields map[string]any
				_ = json.Unmarshal(data, &fields)
				switch mode {
				case "missing":
					delete(fields, "source_anchor")
				case "null":
					fields["source_finding"] = nil
				case "alias":
					fields["Source_anchor"] = fields["source_anchor"]
					delete(fields, "source_anchor")
				case "unknown":
					fields["provider_refresh"] = true
				default:
					item := client.ReviewFindingRouteSummary{ID: routeClientID, Revision: 1, SourceEvidenceID: controlEvidenceID, Finding: 0, SourcePullNumber: 2, TargetPullNumber: 4, Disposition: "unadjudicated", CreatedAt: v.Route.CreatedAt}
					page := client.ReviewRoutePage{ProjectID: "project-one", PullNumber: 4, Items: []client.ReviewFindingRouteSummary{item}}
					if mode == "wrong-receiver-page" {
						page.Items[0].TargetPullNumber = 6
					} else {
						page.Items = append(page.Items, item)
					}
					write(w, 200, page)
					return
				}
				write(w, 200, fields)
			}))
			defer server.Close()
			c := newClient(t, server)
			var err error
			if strings.HasSuffix(mode, "page") {
				_, err = c.ReviewFindingRoutes(t.Context(), 4, client.PageOptions{})
			} else {
				_, err = c.ReviewFindingRoute(t.Context(), routeClientID)
			}
			checkError(t, err, "invalid_response")
		})
	}
}

func TestReviewRoutesClientBrowserReceiverProofStaysSeparate(t *testing.T) {
	f := newReviewClientFixture(t)
	f.importSnapshot(t)
	runs, err := f.client.ReviewRuns(t.Context(), 1, client.ReviewRunOptions{})
	if err != nil || len(runs.Items) != 1 {
		t.Fatal("base run", err)
	}
	original, err := f.client.ReviewRun(t.Context(), runs.Items[0].ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"native-receiver-fixed", "imported-no-route", "native-no-route", "native-nonrouted-with-route", "bad-receiver-proof"} {
		t.Run(mode, func(t *testing.T) {
			value := original
			value.Findings = append([]client.ReviewFinding{}, original.Findings...)
			value.Run.Source = "native-executor"
			value.Run.SourceRunID = controlEvidenceID
			value.Run.Accepted = 0
			value.Run.Routed = 1
			value.Run.Fixed = 0
			finding := &value.Findings[0]
			finding.Disposition = "routed"
			finding.FixSHA = ""
			finding.ProofNote = ""
			finding.ResolvedAt = ""
			finding.Route = &client.ReviewFindingRoute{ID: routeClientID, TargetPullNumber: 4, Revision: 3, State: "active", Disposition: "accepted", Reason: "Receiver confirms", Actor: "receiver", DecidedAt: original.Run.FinishedAt, FixSHA: strings.Repeat("f", 40), ProofNote: "Receiver proof", FixedAt: original.Run.FinishedAt}
			switch mode {
			case "imported-no-route":
				value.Run.Source = "historical-import"
				finding.Route = nil
			case "native-no-route":
				finding.Route = nil
			case "native-nonrouted-with-route":
				finding.Disposition = "accepted"
			case "bad-receiver-proof":
				finding.Route.Disposition = "rejected"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, value)
			}))
			defer server.Close()
			c := newClient(t, server)
			got, err := c.ReviewRun(t.Context(), value.Run.ID, "")
			if mode == "native-receiver-fixed" || mode == "imported-no-route" {
				if err != nil || got.Run.Fixed != 0 || got.Run.Routed != 1 || got.Findings[0].FixSHA != "" {
					t.Fatal("receiver proof replaced source counts/proof", err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		})
	}
}

func TestReviewRoutesRetainedClockRollbackKeepsIndividualTimeValidation(t *testing.T) {
	for _, bad := range []string{"", "created", "updated", "decided", "fixed"} {
		t.Run(bad, func(t *testing.T) {
			value := sampleRouteDetail()
			value.Route.Revision = 3
			value.Route.Disposition = "accepted"
			value.Route.Reason = "Receiver accepts"
			value.Route.ActorID = "receiver"
			value.Route.CreatedAt = "2099-01-03T00:00:00Z"
			value.Route.DecidedAt = "2099-01-02T00:00:00Z"
			value.Route.FixedAt = "2099-01-01T00:00:00Z"
			value.Route.UpdatedAt = "2026-01-01T00:00:00Z"
			value.Route.FixSHA = strings.Repeat("f", 40)
			value.Route.ProofNote = "Exact retained fix proof"
			switch bad {
			case "created":
				value.Route.CreatedAt = "bad"
			case "updated":
				value.Route.UpdatedAt = "bad"
			case "decided":
				value.Route.DecidedAt = "bad"
			case "fixed":
				value.Route.FixedAt = "bad"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				write(w, 200, value)
			}))
			defer server.Close()
			c := newClient(t, server)
			got, err := c.ReviewFindingRoute(t.Context(), routeClientID)
			if bad != "" {
				checkError(t, err, "invalid_response")
			} else if err != nil || got.Route != value.Route {
				t.Fatal("clock rollback erased retained route", got, err)
			}
		})
	}
}
