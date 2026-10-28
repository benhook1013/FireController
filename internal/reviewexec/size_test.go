package reviewexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/presentation"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/reviewsize"
)

func TestSuggestedPRSizeViewsAndEnqueueNeverAlterReviewControls(t *testing.T) {
	for _, tc := range []struct {
		name               string
		suggested, maximum int64
	}{
		{"disabled", 0, 0}, {"suggestion", 80, 0}, {"maximum", 0, 100}, {"both", 80, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := queueEnrollmentFixture(t)
			ctx := t.Context()
			admin := f.scope(t, attemptProject, f.admin, true)
			if err := presentation.EnsureSchema(ctx, f.store); err != nil {
				t.Fatal(err)
			}
			presentation.Register(f.handler.(*http.ServeMux), f.store)
			admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'size-worker','Sizer')`, attemptProject)
			policy, err := f.e.ReadPolicy(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.e.ConfigurePolicy(ctx, admin, "size-maximum", PolicyConfiguration{ExpectedRevision: &policy.Revision, RequiredChannels: policy.Policy.RequiredChannels, MaxReviewFiles: tc.maximum})
			if err != nil {
				t.Fatal(err)
			}
			p := projectionPull(2)
			p.Diff = &reviews.PullDiff{Files: 96, Additions: 100, Deletions: 10}
			for _, id := range []string{"completed-final", "completed-pending"} {
				p.Runs = append(p.Runs, reviews.RunInput{SourceRunID: id, Channel: "cli", HeadSHA: strings.Repeat("c", 40), Reviewer: "Synthetic", Scope: "full", Outcome: "completed", Attributable: true, Complete: true, Finalized: id == "completed-final", StartedAt: "2026-01-01T01:00:00Z", FinishedAt: "2026-01-01T01:01:00Z", Findings: []reviews.Finding{}})
			}
			other := projectionPull(9)
			other.Diff = p.Diff
			obs := reviews.ObservationRequest{ExpectedRevision: adjudicationRevision(0), RepositoryID: 801, Source: "size-fixture", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{p, other}}
			w := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "size-observation", encoded(obs))
			if w.Code != 201 {
				t.Fatal(w.Code, w.Body.String())
			}
			configureQueue(t, f, 0, 2)
			beforePolicy, err := f.e.ReadPolicy(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			beforeRound := roundRead(t, f, 2, "cli")
			var name, rawPolicy string
			if err = f.store.DB().QueryRow(`SELECT name FROM projects WHERE id=?`, attemptProject).Scan(&name); err != nil {
				t.Fatal(err)
			}
			if err = f.store.DB().QueryRow(`SELECT body_json FROM review_policy_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, attemptProject).Scan(&rawPolicy); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("PUT", "/api/v1/projects/"+attemptProject+"/presentation", strings.NewReader(encoded(map[string]any{"expected_revision": 0, "presentation": map[string]any{"name": name, "suggested_max_pr_files": tc.suggested}})))
			request.Header.Set("Authorization", "Bearer "+f.admin)
			request.Header.Set("X-FC-Worker", "size-worker")
			request.Header.Set("Idempotency-Key", "size-preference")
			w = httptest.NewRecorder()
			f.handler.ServeHTTP(w, request)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			assertSize := func(a *reviewsize.Assessment, completed int64) {
				t.Helper()
				if tc.suggested == 0 && tc.maximum == 0 {
					if a != nil {
						t.Fatal(a)
					}
					return
				}
				if a == nil || !reviewsize.Valid(*a, a.PullNumber) || a.Files == nil || *a.Files != 96 || a.SuggestedMaxPRFiles != tc.suggested || a.MaxReviewFiles != tc.maximum || a.AboveSuggested != (tc.suggested > 0) || a.CompletedReviews == nil || *a.CompletedReviews != completed {
					t.Fatalf("size=%+v", a)
				}
			}
			q := configuredQueue(t, f)
			assertSize(q.Items[0].ReviewSize, 2)
			var active reviews.ActiveQueueResponse
			projectionRead(t, f, "/queue/active?limit=100", &active)
			assertSize(active.Items[0].ReviewSize, 2)
			fronts, err := f.e.ReadFronts(ctx, admin)
			if err != nil {
				t.Fatal(err)
			}
			sawFront := false
			for _, front := range fronts.Channels {
				if front.PullNumber == 2 {
					sawFront = true
					assertSize(front.ReviewSize, 2)
				}
			}
			if !sawFront {
				t.Fatal("missing next-target context", fronts)
			}
			first := queueChangeHTTP(t, f, f.writer, "size-add", "add", QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 9})
			assertSize(first.ReviewSize, 0)
			replay := queueChangeHTTP(t, f, f.writer, "size-add", "add", QueueChange{ExpectedConfigurationRevision: 1, PullNumber: 9})
			if !reflect.DeepEqual(first, replay) {
				t.Fatal("enqueue replay changed", first, replay)
			}
			out, err := f.e.Enqueue(ctx, f.scope(t, attemptProject, f.writer, false), "size-review-request", Request{ExpectedConfigurationRevision: 2, PullNumber: 9, Channel: "cli", HeadSHA: p.HeadSHA})
			if err != nil {
				t.Fatal(err)
			}
			var receipt Receipt
			if json.Unmarshal(out, &receipt) != nil || receipt.State != "queued" {
				t.Fatal(string(out))
			}
			assertSize(receipt.ReviewSize, 0)
			// A successful request for a newer head must not reuse this earlier
			// observation's count or derived headroom as candidate-specific facts.
			moved := Request{ExpectedConfigurationRevision: 2, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("e", 40)}
			out, err = f.e.Enqueue(ctx, f.scope(t, attemptProject, f.writer, false), "size-new-head", moved)
			receipt = Receipt{}
			if err != nil || json.Unmarshal(out, &receipt) != nil || receipt.State != "queued" {
				t.Fatal("size context blocked a new-head request", err)
			}
			if tc.name == "disabled" {
				if receipt.ReviewSize != nil {
					t.Fatal("disabled sizing manufactured context")
				}
			} else {
				a := receipt.ReviewSize
				if a == nil || !reviewsize.Valid(*a, 2) || a.Files != nil || a.RemainingFiles != nil || a.ExcessFiles != nil || a.AboveSuggested || a.CompletedReviews == nil || *a.CompletedReviews != 2 || a.SuggestedMaxPRFiles != tc.suggested || a.MaxReviewFiles != tc.maximum {
					t.Fatalf("new candidate reused stale count or lost settings/history: %+v", a)
				}
			}
			replayed, err := f.e.Enqueue(ctx, f.scope(t, attemptProject, f.writer, false), "size-new-head", moved)
			if err != nil || string(out) != string(replayed) {
				t.Fatal("request replay replaced its original size context", err)
			}
			// The queue is still a stored observation with its own observed head.
			assertSize(configuredQueue(t, f).Items[0].ReviewSize, 2)
			afterPolicy, err := f.e.ReadPolicy(ctx, admin)
			if err != nil || !reflect.DeepEqual(beforePolicy, afterPolicy) || !reflect.DeepEqual(beforeRound, roundRead(t, f, 2, "cli")) {
				t.Fatal("advisory changed policy or earned review controls", err)
			}
			var afterRaw string
			if err = f.store.DB().QueryRow(`SELECT body_json FROM review_policy_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, attemptProject).Scan(&afterRaw); err != nil || rawPolicy != afterRaw {
				t.Fatal("existing maximum settings were rewritten", err)
			}
			if tc.name == "suggestion" {
				// An unavailable optional preference cannot turn a successful
				// enrollment into a hold or require repair before planning work.
				admissionSQL(t, f, `UPDATE project_presentations SET state_json='{"suggested_max_pr_files":"invalid"}' WHERE project_id=?`, attemptProject)
				q = configuredQueue(t, f)
				if q.Items[0].ReviewSize == nil || !q.Items[0].ReviewSize.Unavailable {
					t.Fatal("missing optional-information diagnostic")
				}
				added := queueChangeHTTP(t, f, f.writer, "size-unavailable-add", "add", QueueChange{ExpectedConfigurationRevision: 2, PullNumber: 11})
				if added.State != "configured" || added.ReviewSize == nil || !added.ReviewSize.Unavailable {
					t.Fatal("optional size failure blocked enrollment", added)
				}
			}
		})
	}
}
