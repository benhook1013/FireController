package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

const sourceRefreshID = "review_refresh_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func sampleSourceObservation() client.ReviewSourceObservation {
	return client.ReviewSourceObservation{RepositoryID: 1, RepositoryName: "owner/repo", PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ParentSHA: strings.Repeat("b", 40), ObservedAt: "2026-10-16T00:00:00Z"}
}

func sampleSourceOperation() client.ReviewSourceOperation {
	observation := sampleSourceObservation()
	return client.ReviewSourceOperation{ID: sourceRefreshID, ProjectID: "project-one", PullNumber: 2, HeadSHA: observation.HeadSHA, ConfigurationRevision: 1, State: "completed", Revision: 3, CreatedAt: observation.ObservedAt, UpdatedAt: observation.ObservedAt, Observation: &observation}
}

func sampleSourceAssessment() client.ReviewSourcePolicyAssessment {
	observation := sampleSourceObservation()
	return client.ReviewSourcePolicyAssessment{ProjectID: "project-one", RefreshID: sourceRefreshID, Source: "observed_comparison", ConfigurationRevision: 2, PolicyRevision: 1, SourceObservedAt: observation.ObservedAt, EvaluatedAt: observation.ObservedAt, State: "held", Target: observation, Channels: []client.ReviewPolicyAssessmentChannel{{Channel: "cli"}, {Channel: "hosted"}}}
}

type sourceReader struct{ calls atomic.Int64 }

func (r *sourceReader) Repository(_ context.Context, id int64) (reviewprovider.Repository, error) {
	r.calls.Add(1)
	return reviewprovider.Repository{ID: id, FullName: "renamed/repository", DefaultBranch: "main"}, nil
}
func (r *sourceReader) Pull(_ context.Context, repository string, number int64) (reviewprovider.Pull, error) {
	r.calls.Add(1)
	return reviewprovider.Pull{RepositoryID: 1, Number: number, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, nil
}

func sourceCounts(t *testing.T, f *mutationFixture) [5]int {
	t.Helper()
	var result [5]int
	for i, table := range []string{"review_source_refreshes", "events", "receipts", "review_operations", "review_execution_reservations"} {
		if err := f.store.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&result[i]); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestReviewSourcePublicHTTPReplayLostResponseAndStoredAssessment(t *testing.T) {
	f, engine := executionFixture(t)
	var drop atomic.Bool
	var posts atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.Header.Get("X-FC-Instance") != f.store.InstanceID() || r.URL.Path != "/api/v1/instance" && r.Header.Get("X-FC-Worker") != f.sender {
			t.Error("source operation lost selected binding or added fanout selectors")
		}
		if r.Method == "POST" {
			posts.Add(1)
			if drop.Swap(false) {
				recorder := httptest.NewRecorder()
				f.handler.ServeHTTP(recorder, r)
				if recorder.Code != 202 {
					t.Errorf("request did not commit before lost response: %d %s", recorder.Code, recorder.Body.String())
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = connection.Close()
				}
				return
			}
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	request := client.ReviewSourceRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}
	providers := f.providerCalls.Load()
	drop.Store(true)
	_, err := c.RefreshReviewSource(t.Context(), "source-once", request, 0)
	var failure *client.Error
	if !errors.As(err, &failure) || !failure.OutcomeUnknown || posts.Load() != 1 || sourceCounts(t, f)[0] != 1 {
		t.Fatal("lost request was retried or its uncertainty omitted", err)
	}
	before := sourceCounts(t, f)
	result, err := c.RefreshReviewSource(t.Context(), "source-once", request, 0)
	if err != nil || result.Receipt == nil || result.Operation != nil || result.Receipt.State != "queued" || result.Receipt.Revision != 1 || sourceCounts(t, f) != before {
		t.Fatal("exact retry changed durable effect", result, err)
	}
	receipt := *result.Receipt
	op, err := c.ReviewSourceOperation(t.Context(), receipt.ID)
	if err != nil || op.State != "queued" || op.Observation != nil || op.ProjectID != f.project || sourceCounts(t, f) != before || f.providerCalls.Load() != providers {
		t.Fatal("stored operation read changed facts or refreshed providers", op, err)
	}
	_, err = c.ReviewSourcePolicyAssessment(t.Context(), 2)
	checkError(t, err, "reviews_source_unavailable")
	reader := &sourceReader{}
	ctx, cancel := context.WithCancel(t.Context())
	done, err := engine.StartSourceRefresh(ctx, map[string]reviewexec.TargetReader{"synthetic-cli": reader})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(time.Second)
	for {
		op, err = c.ReviewSourceOperation(t.Context(), receipt.ID)
		if err != nil || op.State == "completed" || op.State == "held" || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil || op.State != "completed" || op.Observation == nil || op.Observation.RepositoryName != "renamed/repository" || reader.calls.Load() != 2 {
		t.Fatal("actual source worker did not retain exact selected facts", op, err)
	}
	before = sourceCounts(t, f)
	assessment, err := c.ReviewSourcePolicyAssessment(t.Context(), 2)
	if err != nil || assessment.Source != "observed_comparison" || assessment.State != "held" || assessment.RefreshID != receipt.ID || assessment.Target != *op.Observation || len(assessment.Channels) != 2 || sourceCounts(t, f) != before || reader.calls.Load() != 2 {
		t.Fatal("assessment refreshed or misrepresented stored comparison", assessment, err)
	}
	replay, err := c.RefreshReviewSource(t.Context(), "source-once", request, 0)
	if err != nil || replay.Receipt == nil || *replay.Receipt != receipt || sourceCounts(t, f) != before || reader.calls.Load() != 2 {
		t.Fatal("completed replay changed original receipt", replay, err)
	}
	changed := request
	changed.HeadSHA = strings.Repeat("c", 40)
	_, err = c.RefreshReviewSource(t.Context(), "source-once", changed, 0)
	checkError(t, err, "idempotency_conflict")
	changed = request
	changed.ExpectedConfigurationRevision = 2
	_, err = c.RefreshReviewSource(t.Context(), "stale-configuration", changed, 0)
	checkError(t, err, "review_conflict")
	if sourceCounts(t, f) != before {
		t.Fatal("refused request changed source or review execution")
	}
	fast, err := c.RefreshReviewSource(t.Context(), "source-once", request, 2)
	if err != nil || fast.Receipt != nil || fast.Operation == nil || fast.Operation.ID != receipt.ID || fast.Operation.State != "completed" || sourceCounts(t, f) != before || reader.calls.Load() != 2 {
		t.Fatal("terminal replay did not return direct stored facts", fast, err)
	}
	encoded, err := json.Marshal(fast)
	if err != nil || strings.Contains(string(encoded), `"Operation"`) || strings.Contains(string(encoded), `"Receipt"`) {
		t.Fatal("result added union wrappers", string(encoded), err)
	}
	heldRequest := request
	heldRequest.HeadSHA = strings.Repeat("c", 40)
	held, err := c.RefreshReviewSource(t.Context(), "source-held", heldRequest, 2)
	if err != nil || held.Operation == nil || held.Operation.State != "held" || held.Receipt != nil {
		t.Fatal("fast refusal did not return direct terminal facts", held, err)
	}
}

func TestReviewSourceInvalidArgumentsAreLocal(t *testing.T) {
	var c *client.Client // Invalid arguments must not access even the client binding.
	valid := client.ReviewSourceRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}
	for _, change := range []func(*client.ReviewSourceRequest){
		func(r *client.ReviewSourceRequest) { r.ExpectedConfigurationRevision = 0 },
		func(r *client.ReviewSourceRequest) { r.PullNumber = 0 },
		func(r *client.ReviewSourceRequest) { r.PullNumber = 2147483648 },
		func(r *client.ReviewSourceRequest) { r.HeadSHA = strings.Repeat("A", 40) },
	} {
		request := valid
		change(&request)
		_, err := c.RefreshReviewSource(t.Context(), "source-key", request, 0)
		checkError(t, err, "validation_failed")
	}
	for _, key := range []string{"", " padded", "line\n", "é", strings.Repeat("k", 129)} {
		_, err := c.RefreshReviewSource(t.Context(), key, valid, 0)
		checkError(t, err, "validation_failed")
	}
	for _, seconds := range []int{-1, 61} {
		_, err := c.RefreshReviewSource(t.Context(), "explicit", valid, seconds)
		checkError(t, err, "validation_failed")
	}
	for _, id := range []string{"", "../private", executionOperationID, "review_refresh_" + strings.Repeat("A", 32)} {
		_, err := c.ReviewSourceOperation(t.Context(), id)
		checkError(t, err, "validation_failed")
	}
	for _, n := range []int64{0, -1, 2147483648} {
		_, err := c.ReviewSourcePolicyAssessment(t.Context(), n)
		checkError(t, err, "validation_failed")
	}
}

func TestReviewSourceStrictResponsesAndStateConsistency(t *testing.T) {
	var response atomic.Value
	response.Store("{}")
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			w.WriteHeader(202)
		}
		_, _ = w.Write([]byte(response.Load().(string)))
	}))
	defer service.Close()
	c := newClient(t, service)
	for _, kind := range []string{"operation", "assessment", "receipt"} {
		t.Run(kind, func(t *testing.T) {
			var value any = sampleSourceOperation()
			if kind == "assessment" {
				value = sampleSourceAssessment()
			}
			if kind == "receipt" {
				value = client.ReviewRequestReceipt{ID: sourceRefreshID, State: "queued", Revision: 1}
			}
			valid, _ := json.Marshal(value)
			invoke := func() error {
				switch kind {
				case "operation":
					_, err := c.ReviewSourceOperation(t.Context(), sourceRefreshID)
					return err
				case "assessment":
					_, err := c.ReviewSourcePolicyAssessment(t.Context(), 2)
					return err
				default:
					_, err := c.RefreshReviewSource(t.Context(), "explicit", client.ReviewSourceRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}, 0)
					return err
				}
			}
			response.Store(string(valid))
			if err := invoke(); err != nil {
				t.Fatal("valid response refused", err)
			}
			var original map[string]any
			_ = json.Unmarshal(valid, &original)
			bad := func(name string, edit func(map[string]any)) {
				t.Helper()
				var body map[string]any
				_ = json.Unmarshal(valid, &body)
				edit(body)
				data, _ := json.Marshal(body)
				response.Store(string(data))
				err := invoke()
				checkError(t, err, "invalid_response")
				var failure *client.Error
				if kind == "receipt" && (!errors.As(err, &failure) || !failure.OutcomeUnknown) {
					t.Fatal("invalid receipt lost uncertainty", name, err)
				}
			}
			for field := range original {
				if field == "observation" {
					continue
				}
				bad("missing "+field, func(v map[string]any) { delete(v, field) })
				bad("null "+field, func(v map[string]any) { v[field] = nil })
			}
			bad("unknown", func(v map[string]any) { v["private_actor"] = "secret" })
			bad("wrong state", func(v map[string]any) { v["state"] = "merge_ready" })
			if kind == "operation" {
				for _, edit := range []func(map[string]any){
					func(v map[string]any) { v["id"] = "review_refresh_" + strings.Repeat("b", 32) },
					func(v map[string]any) { v["project_id"] = "foreign" },
					func(v map[string]any) { v["pull_number"] = 2.5 },
					func(v map[string]any) { delete(v, "observation") },
					func(v map[string]any) { v["observation"] = nil },
					func(v map[string]any) { v["state"] = "running" },
					func(v map[string]any) { v["observation"].(map[string]any)["head_sha"] = strings.Repeat("b", 40) },
					func(v map[string]any) { delete(v["observation"].(map[string]any), "parent_pull_number") },
				} {
					bad("operation binding", edit)
				}
			} else if kind == "assessment" {
				for _, edit := range []func(map[string]any){
					func(v map[string]any) { v["project_id"] = "foreign" },
					func(v map[string]any) { v["source"] = "live" },
					func(v map[string]any) { v["state"] = "sufficient" },
					func(v map[string]any) { v["source_observed_at"] = "2026-10-17T00:00:00Z" },
					func(v map[string]any) { v["target"].(map[string]any)["pull_number"] = 4 },
					func(v map[string]any) { v["target"].(map[string]any)["parent_sha"] = strings.Repeat("c", 40) },
					func(v map[string]any) { v["channels"].([]any)[1].(map[string]any)["channel"] = "cli" },
					func(v map[string]any) { delete(v["channels"].([]any)[0].(map[string]any), "pending") },
					func(v map[string]any) { v["reason"] = "merge_conflict" },
				} {
					bad("assessment binding", edit)
				}
			}
			for _, raw := range []string{"null", strings.Replace(string(valid), `"state":`, `"State":`, 1), strings.Replace(string(valid), `"state":`, `"state":"held","state":`, 1), strings.Replace(string(valid), `"state":`, `"bad":"\ud800","state":`, 1)} {
				response.Store(raw)
				checkError(t, invoke(), "invalid_response")
			}
			if kind != "receipt" {
				response.Store(string(valid) + strings.Repeat(" ", 16<<10))
				checkError(t, invoke(), "invalid_response")
			}
		})
	}
	for _, state := range []string{"sufficient", "unconfigured", "held"} {
		value := sampleSourceAssessment()
		value.State = state
		switch state {
		case "sufficient":
			for i := range value.Channels {
				value.Channels[i].Covered = true
			}
		case "unconfigured":
			value.PolicyRevision = 0
			value.Channels = []client.ReviewPolicyAssessmentChannel{}
		case "held":
			value.Reason = "source_configuration_changed"
			value.PolicyRevision = 0
			value.Channels = []client.ReviewPolicyAssessmentChannel{}
		}
		data, _ := json.Marshal(value)
		response.Store(string(data))
		if _, err := c.ReviewSourcePolicyAssessment(t.Context(), 2); err != nil {
			t.Fatal("legitimate assessment state refused", state, err)
		}
	}
}

func TestReviewSourceCancellationReleasesHTTPWithoutRetry(t *testing.T) {
	var calls atomic.Int64
	closed := make(chan struct{}, 3)
	shutdown := make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-shutdown:
		}
		closed <- struct{}{}
	}))
	defer service.Close()
	defer close(shutdown)
	c := newClient(t, service)
	for _, operation := range []string{"read", "assess", "refresh"} {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		var err error
		switch operation {
		case "read":
			_, err = c.ReviewSourceOperation(ctx, sourceRefreshID)
		case "assess":
			_, err = c.ReviewSourcePolicyAssessment(ctx, 2)
		case "refresh":
			_, err = c.RefreshReviewSource(ctx, "explicit", client.ReviewSourceRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}, 0)
		}
		cancel()
		checkError(t, err, "deadline_exceeded")
		var failure *client.Error
		if operation == "refresh" && (!errors.As(err, &failure) || !failure.OutcomeUnknown) {
			t.Fatal("timed-out mutation lost uncertainty")
		}
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("canceled operation left HTTP open")
		}
	}
	if calls.Load() != 3 {
		t.Fatal("operation retried or fanned out", calls.Load())
	}
}

func TestReviewSourceMutationTerminalShapeAndPostcommitAuthorizationUncertainty(t *testing.T) {
	var response atomic.Value
	var status atomic.Int64
	response.Store(`{}`)
	status.Store(200)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Header.Get("Prefer") != "wait=2" {
			t.Error("explicit wait preference lost")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(response.Load().(string)))
	}))
	defer service.Close()
	c := newClient(t, service)
	request := client.ReviewSourceRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}
	for _, state := range []string{"completed", "held"} {
		value := sampleSourceOperation()
		value.State = state
		if state == "held" {
			value.Observation = nil
			value.Reason = "source_unavailable"
		}
		data, _ := json.Marshal(value)
		response.Store(string(data))
		got, err := c.RefreshReviewSource(t.Context(), "explicit", request, 2)
		if err != nil || got.Operation == nil || got.Receipt != nil || got.Operation.State != state {
			t.Fatal("terminal response refused", got, err)
		}
	}
	for _, edit := range []func(*client.ReviewSourceOperation){
		func(v *client.ReviewSourceOperation) { v.HeadSHA = strings.Repeat("b", 40) },
		func(v *client.ReviewSourceOperation) { v.ProjectID = "foreign" },
		func(v *client.ReviewSourceOperation) { v.ConfigurationRevision = 2 },
		func(v *client.ReviewSourceOperation) { v.State = "queued"; v.Observation = nil },
	} {
		value := sampleSourceOperation()
		edit(&value)
		data, _ := json.Marshal(value)
		response.Store(string(data))
		_, err := c.RefreshReviewSource(t.Context(), "explicit", request, 2)
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "invalid_response" || !failure.OutcomeUnknown {
			t.Fatal("invalid terminal response lost uncertainty", err)
		}
	}
	valid, _ := json.Marshal(sampleSourceOperation())
	response.Store(string(valid) + strings.Repeat(" ", 16<<10))
	_, err := c.RefreshReviewSource(t.Context(), "explicit", request, 2)
	checkError(t, err, "invalid_response")
	status.Store(403)
	for _, unknown := range []bool{false, true} {
		body := map[string]any{"error": map[string]any{"code": "forbidden", "message": "Access refused."}, "request_id": "request_one"}
		if unknown {
			body["error"].(map[string]any)["details"] = map[string]any{"outcome_unknown": true}
		}
		data, _ := json.Marshal(body)
		response.Store(string(data))
		_, err := c.RefreshReviewSource(t.Context(), "explicit", request, 2)
		var failure *client.Error
		if !errors.As(err, &failure) || failure.Code != "forbidden" || failure.Status != 403 || failure.OutcomeUnknown != unknown || failure.RequestID != "request_one" {
			t.Fatal("postcommit uncertainty changed known refusal semantics", unknown, err)
		}
		if unknown && !strings.Contains(failure.Action, "exact same idempotency key") {
			t.Fatal("postcommit failure omitted recovery action")
		}
	}
}

func TestReviewSourceExplicitLongWaitExceedsDefaultTransportWithoutChangingReads(t *testing.T) {
	// This one controlled delay crosses the previous five-second header timeout.
	// The explicit wait is six seconds; ordinary reads keep their own budgets.
	var calls atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls.Add(1)
		if r.Method == "POST" {
			_, _ = io.Copy(io.Discard, r.Body)
			if r.Header.Get("Prefer") != "wait=6" {
				t.Error("long preference missing")
			}
			select {
			case <-time.After(5100 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		write(w, 200, sampleSourceOperation())
	}))
	defer service.Close()
	c := newClient(t, service)
	result, err := c.RefreshReviewSource(t.Context(), "long-wait", client.ReviewSourceRequest{ExpectedConfigurationRevision: 1, PullNumber: 2, HeadSHA: strings.Repeat("a", 40)}, 6)
	if err != nil || result.Operation == nil || result.Operation.State != "completed" {
		t.Fatal("explicit wait retained default five-second timeout", result, err)
	}
	if _, err := c.ReviewSourceOperation(t.Context(), sourceRefreshID); err != nil || calls.Load() != 2 {
		t.Fatal("per-call transport changed ordinary read", err)
	}
}
