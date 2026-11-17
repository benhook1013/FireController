package client_test

// Selected regression coverage recovered from the earlier review-controls work.
// Production behavior and superseded source-obligation expectations are not restored.
import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewPolicyAssessmentRoundStateValidation(t *testing.T) {
	var response atomic.Value
	c := assessmentPolicyClient(t, "/api/v1/projects/project-one/reviews/evidence/"+controlEvidenceID+"/policy", &response)
	for _, test := range []struct {
		name  string
		edit  func(*client.ReviewPolicyAssessment)
		valid bool
	}{
		{"complete rounds", func(v *client.ReviewPolicyAssessment) {
			v.Channels[0].RoundsConfigured = true
			v.Channels[0].RoundsComplete = true
		}, true},
		{"unfinished rounds hold", func(v *client.ReviewPolicyAssessment) { v.State = "held"; v.Channels[0].RoundsConfigured = true }, true},
		{"unfinished rounds false sufficient", func(v *client.ReviewPolicyAssessment) { v.Channels[0].RoundsConfigured = true }, false},
		{"round completion without configuration", func(v *client.ReviewPolicyAssessment) { v.Channels[0].RoundsComplete = true }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := samplePolicyAssessment()
			value.State = "sufficient"
			value.Channels[1].Covered = true
			value.Channels[1].Pending = false
			test.edit(&value)
			response.Store(value)
			got, err := c.ReviewPolicyAssessment(t.Context(), controlEvidenceID)
			if test.valid {
				if err != nil || got.SourceObligationsPending != value.SourceObligationsPending || got.Channels[0] != value.Channels[0] {
					t.Fatal("round/obligation facts lost", got, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		})
	}
}

func TestReadinessRoundStateCannotCreateFalseReady(t *testing.T) {
	var response atomic.Value
	c := assessmentPolicyClient(t, "/api/v1/projects/project-one/reviews/source/2/readiness", &response)
	for _, test := range []struct {
		name  string
		edit  func(*client.ReviewSourceReadinessAssessment)
		valid bool
	}{
		{"complete rounds", func(v *client.ReviewSourceReadinessAssessment) {
			v.Channels[0].RoundsConfigured = true
			v.Channels[0].RoundsComplete = true
		}, true},
		{"unfinished rounds hold", func(v *client.ReviewSourceReadinessAssessment) {
			v.State = "held"
			v.Reasons = []string{"rounds_pending"}
			v.Channels[0].RoundsConfigured = true
		}, true},
		{"false ready rounds", func(v *client.ReviewSourceReadinessAssessment) { v.Channels[0].RoundsConfigured = true }, false},
		{"round completion without configuration", func(v *client.ReviewSourceReadinessAssessment) { v.Channels[0].RoundsComplete = true }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := sampleReadiness()
			test.edit(&value)
			response.Store(value)
			got, err := c.ReviewSourceReadiness(t.Context(), 2)
			if test.valid {
				if err != nil || got.SourceObligationsPending != value.SourceObligationsPending || got.Channels[0] != value.Channels[0] {
					t.Fatal("readiness round/obligation facts lost", got, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		})
	}
}

func TestReviewSourcePolicyRoundStateValidation(t *testing.T) {
	var response atomic.Value
	c := assessmentPolicyClient(t, "/api/v1/projects/project-one/reviews/source/2/policy", &response)
	for _, test := range []struct {
		name  string
		edit  func(*client.ReviewSourcePolicyAssessment)
		valid bool
	}{
		{"complete rounds", func(v *client.ReviewSourcePolicyAssessment) {
			v.Channels[0].RoundsConfigured = true
			v.Channels[0].RoundsComplete = true
		}, true},
		{"unfinished rounds hold", func(v *client.ReviewSourcePolicyAssessment) { v.State = "held"; v.Channels[0].RoundsConfigured = true }, true},
		{"false sufficient rounds", func(v *client.ReviewSourcePolicyAssessment) { v.Channels[0].RoundsConfigured = true }, false},
		{"round completion without configuration", func(v *client.ReviewSourcePolicyAssessment) { v.Channels[0].RoundsComplete = true }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := sampleSourceAssessment()
			value.State = "sufficient"
			for i := range value.Channels {
				value.Channels[i].Covered = true
			}
			test.edit(&value)
			response.Store(value)
			got, err := c.ReviewSourcePolicyAssessment(t.Context(), 2)
			if test.valid {
				if err != nil || got.SourceObligationsPending != value.SourceObligationsPending || got.Channels[0] != value.Channels[0] {
					t.Fatal("source round/obligation facts lost", got, err)
				}
			} else {
				checkError(t, err, "invalid_response")
			}
		})
	}
}

func TestReviewRoundsPublicHTTPCloseTaperNoopAndOriginalReplay(t *testing.T) {
	f, _ := executionFixture(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	beforeProviders := f.providerCalls.Load()
	initial, err := c.ReviewRoundControl(t.Context(), 2, "cli")
	if err != nil || !initial.Configured || initial.Taper != 2 || initial.Used != 0 || initial.Dry != 0 || initial.OriginSource != "" {
		t.Fatal("enrollment did not retain native default2", initial, err)
	}
	input := client.ConfigureReviewRoundControlRequest{ExpectedRevision: roundInt(initial.Revision), Action: "grant", Minimum: roundInt(0), Maximum: roundInt(8)}
	granted, err := c.ConfigureReviewRoundControl(t.Context(), 2, "cli", "grant-for-retune", input)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := c.ConfigureReviewRoundControl(t.Context(), 2, "cli", "close-for-retune", client.ConfigureReviewRoundControlRequest{ExpectedRevision: roundInt(granted.Revision), Action: "close"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := c.ReviewRoundControl(t.Context(), 2, "cli")
	if err != nil {
		t.Fatal(err)
	}
	request := client.ConfigureReviewRoundControlRequest{ExpectedRevision: roundInt(closed.Revision), Action: "taper", Taper: roundInt(3)}
	changed, err := c.ConfigureReviewRoundControl(t.Context(), 2, "cli", "retune-closed", request)
	if err != nil || changed.Outcome != "taper_updated" {
		t.Fatal(changed, err)
	}
	after, err := c.ReviewRoundControl(t.Context(), 2, "cli")
	if err != nil {
		t.Fatal(err)
	}
	expected := before
	expected.Taper = 3
	expected.Revision = changed.Revision
	expected.UpdatedAt = after.UpdatedAt
	expected.ActorID = after.ActorID
	if after != expected || !after.Closed || after.Epoch != before.Epoch {
		t.Fatal("taper reopened or reset control", after, before)
	}
	noop := client.ConfigureReviewRoundControlRequest{ExpectedRevision: roundInt(after.Revision), Action: "taper", Taper: roundInt(3)}
	unchanged, err := c.ConfigureReviewRoundControl(t.Context(), 2, "cli", "same-threshold", noop)
	if err != nil || unchanged.Revision != after.Revision {
		t.Fatal("same threshold did not return original revision", unchanged, err)
	}
	replay, err := c.ConfigureReviewRoundControl(t.Context(), 2, "cli", "retune-closed", request)
	if err != nil || replay != changed {
		t.Fatal("taper original-key receipt changed", replay, err)
	}
	original, err := c.ConfigureReviewRoundControl(t.Context(), 2, "cli", "grant-for-retune", input)
	if err != nil || original != granted {
		t.Fatal("original grant receipt reissued allocation", original, err)
	}
	final, err := c.ReviewRoundControl(t.Context(), 2, "cli")
	if err != nil || final != after || f.providerCalls.Load() != beforeProviders {
		t.Fatal("replay/noop changed state or performed provider work", final, err)
	}
	if executionCounts(t, f)[0] != 0 {
		t.Fatal("round controls created review operations")
	}
}

func assessmentPolicyClient(t *testing.T, path string, response *atomic.Value) *client.Client {
	t.Helper()
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		if r.Method != "GET" || r.URL.Path != path || r.URL.RawQuery != "" || r.Header.Get("X-FC-Worker") != "worker-one" || r.Header.Get("Prefer") != "" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("assessment became a refresh/mutation or lost binding")
		}
		write(w, http.StatusOK, response.Load())
	}))
	t.Cleanup(service.Close)
	return newClient(t, service)
}
