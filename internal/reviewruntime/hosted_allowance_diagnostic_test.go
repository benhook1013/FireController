package reviewruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type diagnosticAllowanceProvider struct {
	*driverHostedProvider
	historyFailure error
	futureQuery    bool
}

func (p *diagnosticAllowanceProvider) IssueComments(ctx context.Context, repo string, number int64) ([]reviewprovider.IssueComment, error) {
	if p.historyFailure != nil {
		p.histories++
		return nil, p.historyFailure
	}
	rows, err := p.driverHostedProvider.IssueComments(ctx, repo, number)
	if p.futureQuery {
		for i := range rows {
			if rows[i].ID == 100 {
				rows[i].CreatedAt = rows[i].CreatedAt.Add(time.Hour)
				rows[i].UpdatedAt = rows[i].CreatedAt
			}
		}
	}
	return rows, err
}

func TestHostedAllowanceDriverSafeFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		failure      error
		future       bool
	}{
		{"invalid history", "hosted_allowance_history_invalid", &reviewprovider.Error{Code: "provider_invalid_response", Message: "PRIVATE response body", Status: 200}, false},
		{"unavailable history", "hosted_allowance_history_unavailable", &reviewprovider.Error{Code: "provider_unavailable", Message: "PRIVATE credential path", Status: 503}, false},
		{"unknown history", "hosted_allowance_unproven", errors.New("PRIVATE unknown body"), false},
		{"contradictory POST GET times", "hosted_allowance_capture_invalid", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHostedDriverFixture(t)
			provider := &diagnosticAllowanceProvider{driverHostedProvider: f.provider, historyFailure: tc.failure, futureQuery: tc.future}
			f.hosted.provider = provider
			raw, err := f.engine.Enqueue(t.Context(), f.scope(t, f.writer, false), "allowance-diagnostic-worker", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.headSHA})
			if err != nil {
				t.Fatal(err)
			}
			var receipt reviewexec.Receipt
			if json.Unmarshal(raw, &receipt) != nil {
				t.Fatal("invalid request receipt")
			}
			ctx, cancel := context.WithCancel(t.Context())
			done, err := f.engine.Start(ctx, map[string]reviewexec.Driver{f.capability.ID: f.hosted})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			op, terminal, readErr := f.engine.WaitOperation(t.Context(), f.scope(t, f.writer, false), receipt.ID, 3)
			cancel()
			select {
			case stopErr := <-done:
				if stopErr != nil {
					t.Fatal(stopErr)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("supervisor did not stop")
			}
			if readErr != nil || !terminal || op.State != "held" || op.Reason != tc.reason {
				t.Fatal("worker lost safe preparation cause", op.State, op.Reason, readErr)
			}
			a := reviewexec.Attempt{ID: op.AttemptID}
			if f.provider.queries != 1 || f.provider.histories != 1 || f.provider.reviews != 0 || f.provider.observations != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 {
				t.Fatal("diagnosis repeated provider work or changed credit")
			}

			query, err := f.engine.ReadHostedAllowanceQuery(t.Context(), f.project, a.ID)
			if err != nil || query.Comment == nil || query.Comment.ID != 100 || !query.Comment.CreatedAt.Equal(f.provider.comments[0].CreatedAt) {
				t.Fatal("original POST response changed", err)
			}
			if tc.future {
				if query.Comment.CreatedAt.After(time.Now().UTC()) || query.Comment.Body != query.Intent {
					t.Fatal("original POST facts normalized")
				}
				if rows, err := f.engine.ReadHostedObservations(t.Context(), f.project, a.ID); err != nil || len(rows) != 0 {
					t.Fatal("invalid future capture admitted", err)
				}
			}
			mux := http.NewServeMux()
			f.engine.Register(mux)
			for _, compact := range []bool{false, true} {
				path := "/api/v1/projects/" + f.project + "/reviews/operations/" + op.ID
				if compact {
					path += "/status"
				}
				req := httptest.NewRequest("GET", path, nil)
				req.Header.Set("Authorization", "Bearer "+f.readerToken())
				if !compact {
					req.Header.Set("X-FC-Hosted-Query", "2")
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != 200 || strings.Contains(w.Body.String(), "PRIVATE") {
					t.Fatal("unsafe diagnostic read", w.Code)
				}
				if compact {
					var v client.ReviewOperationStatus
					if err = json.Unmarshal(w.Body.Bytes(), &v); err != nil {
						t.Fatal(err)
					}
					out := client.AgentReviewOperationStatus(v)
					if v.Reason != tc.reason || out.Diagnostic == nil {
						t.Fatal("compact diagnostic lost retained cause", out)
					}
				} else {
					var v client.ReviewOperation
					if err = json.Unmarshal(w.Body.Bytes(), &v); err != nil {
						t.Fatal(err)
					}
					if v.HostedQuery == nil || v.HostedQuery.QueryID != query.ID || v.HostedQuery.QueryCommentID != 100 {
						t.Fatal("original query handle lost", v.HostedQuery)
					}
				}
			}
			for _, header := range []string{"", "1", "unknown"} {
				req := httptest.NewRequest("GET", "/api/v1/projects/"+f.project+"/reviews/operations/"+op.ID, nil)
				req.Header.Set("Authorization", "Bearer "+f.readerToken())
				req.Header.Set("X-FC-Hosted-Query", header)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				var old client.ReviewOperation
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &old) != nil || old.Reason != tc.reason || strings.Contains(w.Body.String(), "PRIVATE") {
					t.Fatal("old header changed operation or exposed source", header, w.Code)
				}
				wantQuery := header == "1" && tc.reason == "hosted_allowance_unproven"
				if (old.HostedQuery != nil) != wantQuery || strings.Contains(w.Body.String(), `"hosted_query"`) != wantQuery {
					t.Fatal("old reader received expanded reason binding or lost old handle", header, old.HostedQuery)
				}
			}
			if f.provider.queries != 1 || f.provider.histories != 1 || f.provider.reviews != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || cliCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
				t.Fatal("local held reads changed accounting or fetched provider")
			}
		})
	}
}

func TestHostedAllowanceFailureReasonFiniteSafeStages(t *testing.T) {
	for _, tc := range []struct {
		stage string
		err   error
		want  string
	}{
		{"query_post_unproven", errors.New("PRIVATE"), "hosted_allowance_query_post_unproven"},
		{"query_recording_pending", core.Fail("review_conflict", "PRIVATE", 409), "hosted_allowance_query_recording_pending"},
		{"query_capture_recording_pending", core.Fail("overloaded", "PRIVATE", 503), "hosted_allowance_capture_recording_pending"},
		{"query_capture_unavailable", core.Fail("capture_unavailable", "PRIVATE", 503), "hosted_allowance_capture_unavailable"},
		{"query_history_incomplete", context.DeadlineExceeded, "hosted_allowance_history_unavailable"},
		{"query_history_incomplete", &reviewprovider.Error{Code: "PRIVATE", Message: "PRIVATE"}, "hosted_allowance_unproven"},
		{"query_capture_unavailable", errors.New("PRIVATE"), "hosted_allowance_unproven"},
		{"PRIVATE", core.Fail("validation_failed", "PRIVATE", 400), "hosted_allowance_unproven"},
	} {
		if got := hostedAllowanceFailureReason(tc.stage, tc.err); got != tc.want {
			t.Fatal(tc.stage, got, tc.want)
		}
	}
}
