package reviewexec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type sourceCIStageFailure struct {
	stage string
	cause error
}

func (e sourceCIStageFailure) Error() string             { return "PRIVATE_PROVIDER_MESSAGE" }
func (e sourceCIStageFailure) Unwrap() error             { return e.cause }
func (e sourceCIStageFailure) CICollectionStage() string { return e.stage }

func TestSourceCIFailureStagePersistsWithoutPartialCredit(t *testing.T) {
	for _, stage := range []string{"head_results", "attempt", "PRIVATE_PROVIDER_URL"} {
		t.Run(stage, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			reader := &sourceCITestReader{ci: func(_ context.Context, target reviewprovider.CITarget) (reviewprovider.CIObservation, error) {
				return sourceCITestObservation(target, "satisfied"), sourceCIStageFailure{stage: stage, cause: &reviewprovider.Error{Code: "provider_invalid_response", Message: "PRIVATE", Status: 200}}
			}}
			receipt := refreshEnqueue(t, f, "stage-ci", sourceCIRequest())
			cancel, done := refreshStart(t, f, reader)
			got := refreshWait(t, f, receipt.ID, "completed")
			refreshStop(t, cancel, done)
			want := "ci_provider_invalid_response"
			if stage != "PRIVATE_PROVIDER_URL" {
				want += "_" + stage
			}
			if got.CI == nil || got.CI.State != "unproven" || got.CI.Reason != want || got.CI.Required != 0 {
				t.Fatal("unsafe failed stage", got)
			}
			before := sourceCITestState(t, f)
			for range 2 {
				assessment := readSourceCI(t, f)
				if assessment.Reason != want || assessment.State != "unproven" {
					t.Fatal(assessment)
				}
				body, _ := json.Marshal(assessment)
				if strings.Contains(string(body), "PRIVATE") {
					t.Fatal("private diagnostic leaked")
				}
			}
			if sourceCITestState(t, f) != before || reader.ciCalls.Load() != 1 {
				t.Fatal("stage read recollected or changed evidence")
			}
		})
	}
}
