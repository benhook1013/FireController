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
	"github.com/benhook1013/FireController/internal/reviews"
)

const subagentRunID = "review_run_0123456789abcdef0123456789abcdef"

func subagentRecord() client.SubagentRecordRequest {
	return client.SubagentRecordRequest{PullNumber: 7, HeadSHA: strings.Repeat("a", 40), Reviewer: "Sol", Scope: "Database invariants", StartedAt: "2026-10-25T01:00:00Z", FinishedAt: "2026-10-25T01:01:00Z", Findings: []client.SubagentFindingInput{{Title: "Source identity", Detail: "PRIVATE_REPORT_DETAIL", Severity: "high"}}}
}
func subagentReport() client.SubagentReport {
	return client.SubagentReport{ID: subagentRunID, ProjectID: "project-one", PullNumber: 7, Revision: 17, Run: client.ReviewRunSummary{ID: subagentRunID, Source: "worker-subagent", SourceRunID: "retained-subagent-source", PullNumber: 7, Channel: "subagent", HeadSHA: strings.Repeat("a", 40), Reviewer: "Sol", Scope: "Database invariants", Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-10-25T01:00:00Z", FinishedAt: "2026-10-25T01:01:00Z", Found: 1, CoverageLimits: []string{}}, Findings: []client.ReviewFinding{{SourceFindingID: "finding:0", Title: "Source identity", Detail: "PRIVATE_REPORT_DETAIL", Severity: "high", Disposition: "unadjudicated"}}}
}
func TestSubagentClientBoundRequestsCompactReceiptsAndStableRevision(t *testing.T) {
	var calls atomic.Int64
	request := subagentRecord()
	report := subagentReport()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			write(w, 200, map[string]string{"instance_id": "instance-one"})
			return
		}
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("X-FC-Instance") != "instance-one" || r.URL.RawQuery != "" {
			t.Error("lost request binding")
		}
		switch r.URL.Path {
		case "/api/v1/projects/project-one/reviews/subagent-reports":
			var in client.SubagentRecordRequest
			if r.Method != "POST" || r.Header.Get("Idempotency-Key") != "record-key" || json.NewDecoder(r.Body).Decode(&in) != nil || in.Findings[0].Detail != request.Findings[0].Detail {
				t.Error("record request changed")
			}
			write(w, 201, client.SubagentReceipt{ID: subagentRunID, State: "recorded", Revision: 17})
		case "/api/v1/projects/project-one/reviews/subagent-reports/" + subagentRunID:
			if r.Method != "GET" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("read became mutation")
			}
			write(w, 200, report)
		case "/api/v1/projects/project-one/reviews/subagent-reports/" + subagentRunID + "/updates":
			var in client.SubagentUpdateRequest
			if r.Method != "POST" || r.Header.Get("Idempotency-Key") != "update-key" || json.NewDecoder(r.Body).Decode(&in) != nil || in.ExpectedRevision == nil || *in.ExpectedRevision != 17 || in.Decisions[0].Finding != 0 {
				t.Error("guarded update changed")
			}
			// Unrelated workspace activity may advance the new version by more than1.
			write(w, 200, client.SubagentReceipt{ID: subagentRunID, State: "updated", Revision: 23})
		default:
			t.Error("unexpected endpoint")
			write(w, 404, map[string]any{})
		}
	}))
	defer srv.Close()
	c := newClient(t, srv)
	first, err := c.RecordSubagentReview(t.Context(), request, "record-key")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := c.RecordSubagentReview(t.Context(), request, "record-key")
	if err != nil || first != replay {
		t.Fatal("keyed receipt changed", err)
	}
	got, err := c.ReadSubagentReview(t.Context(), subagentRunID)
	if err != nil || got.Revision != 17 || got.Findings[0].Detail != request.Findings[0].Detail {
		t.Fatal(got, err)
	}
	revision := int64(17)
	updated, err := c.UpdateSubagentReview(t.Context(), subagentRunID, client.SubagentUpdateRequest{ExpectedRevision: &revision, Decisions: []client.SubagentDecisionInput{{Finding: 0, Disposition: "accepted", Reason: "Verified source"}}}, "update-key")
	if err != nil || updated.Revision != 23 || calls.Load() != 4 {
		t.Fatal(updated, err, calls.Load())
	}
}

func TestSubagentDocumentAdmissionRejectsMissingNullAndDuplicateInputs(t *testing.T) {
	base, _ := json.Marshal(subagentRecord())
	for _, bad := range [][]byte{
		[]byte(`null`), []byte(`[]`), []byte(`{}`),
		[]byte(strings.Replace(string(base), `"pull_number":7,`, "", 1)),
		[]byte(strings.Replace(string(base), `"findings":[`, `"findings":null,"other":[`, 1)),
		[]byte(strings.Replace(string(base), `"reviewer":"Sol"`, `"reviewer":null`, 1)),
		[]byte(strings.Replace(string(base), `"severity":"high"`, `"severity":"high","severity":"low"`, 1)),
		[]byte(strings.TrimSuffix(string(base), "}") + `,"finalized":true}`),
		[]byte(strings.TrimSuffix(string(base), "}") + `,"actor_id":"other"}`),
		[]byte(strings.TrimSuffix(string(base), "}") + `,"coverage_limits":null}`),
	} {
		if _, err := client.DecodeSubagentRecordDocument(bad); err == nil {
			t.Fatal("invalid record admitted")
		}
	}
	zero := subagentRecord()
	zero.Findings = []client.SubagentFindingInput{}
	zero.Finalized = true
	good, _ := json.Marshal(zero)
	if _, err := client.DecodeSubagentRecordDocument(good); err != nil {
		t.Fatal("complete zero report refused", err)
	}
	for _, bad := range []string{
		`{}`, `{"expected_revision":null,"finalize":true}`, `{"expected_revision":0,"finalize":true}`, `{"expected_revision":1}`, `{"expected_revision":1,"decisions":null}`, `{"expected_revision":1,"decisions":[{"disposition":"accepted","reason":"proof"}]}`, `{"expected_revision":1,"decisions":[{"finding":0,"disposition":"accepted","reason":"proof"},{"finding":0,"disposition":"accepted","reason":"proof"}]}`, `{"expected_revision":1,"fixes":[{"finding":0,"fix_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","proof_note":"proof"}]}`,
	} {
		if _, err := client.DecodeSubagentUpdateDocument([]byte(bad)); err == nil {
			t.Fatal("invalid update admitted", bad)
		}
	}
	goodFix := `{"expected_revision":1,"fixes":[{"finding":0,"expected_sha":"","fix_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","proof_note":"proof"}]}`
	if _, err := client.DecodeSubagentUpdateDocument([]byte(goodFix)); err != nil {
		t.Fatal("explicit empty prior SHA refused", err)
	}
}

func TestSubagentClientRejectsMisboundReportsAndUnknownMutationResponses(t *testing.T) {
	var malformed atomic.Bool
	var reportCase atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			write(w, 200, map[string]string{"instance_id": "instance-one"})
			return
		}
		if r.Method == "POST" {
			if malformed.Load() {
				write(w, 201, map[string]any{"id": subagentRunID, "state": "recorded", "revision": 17, "extra": "PRIVATE"})
			} else {
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
			}
			return
		}
		value := subagentReport()
		switch reportCase.Load() {
		case 0:
			value.ProjectID = "foreign-project"
		case 1:
			value.Run.Source = "native-executor"
		case 2:
			value.Run.Channel = "cli"
		case 3:
			value.Findings[0].SourceFindingID = ""
		case 4:
			value.Findings = nil
		case 5:
			value.Run.Fixed = 1
		case 6:
			value.Run.HeadSHA = ""
		case 7:
			value.Run.Attributable = false
		case 8:
			value.Run.Complete = false
		case 9:
			value.Run.Routed = 1
		}
		write(w, 200, value)
	}))
	defer srv.Close()
	c := newClient(t, srv)
	for i := int64(0); i < 10; i++ {
		reportCase.Store(i)
		if _, err := c.ReadSubagentReview(t.Context(), subagentRunID); err == nil {
			t.Fatal("misbound report accepted", i)
		}
	}
	for _, badResponse := range []bool{false, true} {
		malformed.Store(badResponse)
		_, err := c.RecordSubagentReview(context.Background(), subagentRecord(), "retain-this-key")
		var failure *client.Error
		if !errors.As(err, &failure) || !failure.OutcomeUnknown {
			t.Fatal("lost uncertain mutation protection", err)
		}
	}
}

func TestSubagentClientActualHandlerRecordFinalizeFixAndHistory(t *testing.T) {
	f := newMutationFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	reviews.Register(mux, f.store)
	mux.Handle("/", f.handler)
	f.handler = mux
	zero := int64(0)
	f.call(t, http.MethodPost, "/api/v1/projects/"+f.project+"/reviews/observations", f.admin, "", reviews.ObservationRequest{ExpectedRevision: &zero, RepositoryID: 1, Source: "synthetic-enrollment", ObservedAt: "2026-10-25T01:00:00Z", Queue: []reviews.PullInput{{Number: 7, Title: "Selected PR", State: "open", HeadSHA: strings.Repeat("a", 40), BaseRef: "main", BaseSHA: strings.Repeat("b", 40), CIState: "unknown", Mergeability: "unknown"}}, Retired: []reviews.RetiredPull{}})
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	c := f.newClient(t, f.admin, f.sender, srv.URL)
	created, err := c.RecordSubagentReview(t.Context(), subagentRecord(), "subagent-actual-create")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := c.RecordSubagentReview(t.Context(), subagentRecord(), "subagent-actual-create")
	if err != nil || replay != created {
		t.Fatal("record replay changed", created, replay, err)
	}
	recorded, err := c.ReadSubagentReview(t.Context(), created.ID)
	if err != nil || recorded.Run.Source != "worker-subagent" || recorded.Run.Channel != "subagent" || recorded.Run.Found != 1 || recorded.Run.Accepted != 0 || recorded.Revision != created.Revision {
		t.Fatal(recorded, err)
	}
	changed, err := c.UpdateSubagentReview(t.Context(), created.ID, client.SubagentUpdateRequest{ExpectedRevision: &recorded.Revision, Decisions: []client.SubagentDecisionInput{{Finding: 0, Disposition: "accepted", Reason: "Source verified"}}, Finalize: true}, "subagent-actual-finalize")
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	fixed, err := c.UpdateSubagentReview(t.Context(), created.ID, client.SubagentUpdateRequest{ExpectedRevision: &changed.Revision, Fixes: []client.SubagentFixInput{{Finding: 0, ExpectedSHA: &empty, FixSHA: strings.Repeat("f", 40), ProofNote: "Verified focused proof"}}}, "subagent-actual-fix")
	if err != nil {
		t.Fatal(err)
	}
	final, err := c.ReadSubagentReview(t.Context(), created.ID)
	if err != nil || !final.Run.Finalized || final.Run.Accepted != 1 || final.Run.Fixed != 1 || final.Revision != fixed.Revision || final.Findings[0].FixSHA != strings.Repeat("f", 40) {
		t.Fatal(final, err)
	}
	ordinary, err := c.ReviewRun(t.Context(), created.ID, "")
	if err != nil || ordinary.Run.Channel != "subagent" || ordinary.Run.Fixed != 1 || ordinary.Findings[0].Detail != "PRIVATE_REPORT_DETAIL" {
		t.Fatal("ordinary history cannot read report", ordinary, err)
	}
	if f.providerCalls.Load() != 1 {
		t.Fatal("recording contacted a provider", f.providerCalls.Load())
	}
}

func TestSubagentRecordDiagnosticsNameFieldWithoutPrivateValues(t *testing.T) {
	for _, tc := range []struct {
		name, field, bound string
		change             func(*client.SubagentRecordRequest)
	}{
		{"scope bytes", "scope", "128", func(x *client.SubagentRecordRequest) { x.Scope = strings.Repeat("界", 43) }},
		{"reviewer", "reviewer", "128", func(x *client.SubagentRecordRequest) { x.Reviewer = strings.Repeat("PRIVATE", 30) }},
		{"coverage item", "coverage_limits[0]", "1024", func(x *client.SubagentRecordRequest) { x.CoverageLimits = []string{strings.Repeat("PRIVATE", 200)} }},
		{"coverage count", "coverage_limits", "16", func(x *client.SubagentRecordRequest) { x.CoverageLimits = make([]string, 17) }},
		{"title", "findings[0].title", "500", func(x *client.SubagentRecordRequest) { x.Findings[0].Title = strings.Repeat("PRIVATE", 100) }},
		{"detail", "findings[0].detail", "16384", func(x *client.SubagentRecordRequest) { x.Findings[0].Detail = strings.Repeat("PRIVATE", 3000) }},
		{"reason", "findings[0].reason", "2048", func(x *client.SubagentRecordRequest) {
			x.Findings[0].Disposition = "accepted"
			x.Findings[0].Reason = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := subagentRecord()
			tc.change(&x)
			err := client.ValidateSubagentRecordRequest(x)
			var diagnostic *client.Error
			if !errors.As(err, &diagnostic) || diagnostic.Code != "validation_failed" || !strings.Contains(diagnostic.Message, tc.field) || !strings.Contains(diagnostic.Action, tc.bound) || strings.Contains(diagnostic.Message+diagnostic.Action, "PRIVATE") {
				t.Fatalf("missing safe field diagnostic: %v", err)
			}
		})
	}
	x := subagentRecord()
	x.Scope = strings.Repeat("界", 42) + "ab"
	x.CoverageLimits = []string{strings.Repeat("x", 1024)}
	if err := client.ValidateSubagentRecordRequest(x); err != nil {
		t.Fatal("exact byte bounds refused", err)
	}
	base, _ := json.Marshal(subagentRecord())
	bad := strings.TrimSuffix(string(base), "}") + `,"coverage_limits":"PRIVATE_LIMIT_BODY"}`
	_, err := client.DecodeSubagentRecordDocument([]byte(bad))
	var diagnostic *client.Error
	if !errors.As(err, &diagnostic) || !strings.Contains(diagnostic.Message, "coverage_limits") || !strings.Contains(diagnostic.Action, "array") || strings.Contains(diagnostic.Message+diagnostic.Action, "PRIVATE_LIMIT_BODY") {
		t.Fatal("wrong shape diagnostic", err)
	}
}
