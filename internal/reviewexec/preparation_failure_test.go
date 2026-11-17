package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestPreparationFailureAtAllowsOnlyStableDiagnostics(t *testing.T) {
	const private = "/private/profile ghp_secret provider response"
	for _, tc := range []struct {
		name, stage string
		failure     error
		want        string
	}{
		{"storage", "attempt_publication", &core.Error{Code: "storage_unavailable", Message: private, Details: map[string]string{"path": private}}, "preparation_attempt_publication_storage_unavailable"},
		{"overload", "allowance_recording", core.Fail("overloaded", private, 503), "preparation_allowance_recording_overloaded"},
		{"provider", "source_resolution", &reviewprovider.Error{Code: "provider_unauthorized", Message: private}, "preparation_source_resolution_provider_unauthorized"},
		{"deadline", "allowance_wait", fmt.Errorf(private+": %w", context.DeadlineExceeded), "preparation_allowance_wait_deadline_exceeded"},
		{"cancelled", "execution_context", context.Canceled, "preparation_execution_context_cancelled"},
		{"missing", "retry_baseline", sql.ErrNoRows, "preparation_retry_baseline_not_found"},
		{"unknown stage", private, errors.New(private), "preparation_unknown_other"},
		{"unknown core code", "source_resolution", &core.Error{Code: private, Message: private}, "preparation_source_resolution_other"},
		{"unknown provider code", "source_resolution", &reviewprovider.Error{Code: private, Message: private}, "preparation_source_resolution_other"},
		{"untyped imitation", "source_refresh", errors.New("provider_unauthorized " + private), "preparation_source_refresh_other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := PreparationFailureAt(tc.stage, tc.failure)
			if !errors.Is(failure, tc.failure) || preparationReason(failure) != tc.want || !identifier.MatchString(tc.want) {
				t.Fatalf("unexpected classification: %v", failure)
			}
			for _, formatted := range []string{fmt.Sprint(failure), fmt.Sprintf("%+v", failure), fmt.Sprintf("%#v", failure)} {
				if formatted != tc.want || strings.Contains(formatted, private) {
					t.Fatal("unsafe error formatting")
				}
			}
			encoded, err := json.Marshal(failure)
			if err != nil || strings.Contains(string(encoded), private) {
				t.Fatal("unsafe error serialization", err)
			}
		})
	}
	if PreparationFailureAt("source_resolution", nil) != nil {
		t.Fatal("nil became a failure")
	}
	known := &core.Error{Code: "review_held", Message: private, Details: map[string]string{"reason": "parent_unavailable", "pull_number": "2", "base_ref": "topic"}}
	wrapped := PreparationFailureAt("source_resolution", known)
	if preparationReason(wrapped) != "parent_unavailable" || sourceRefusalFromError(wrapped, "held", "parent_unavailable", 2) == nil {
		t.Fatal("existing hold or source evidence changed")
	}
}

func TestSupervisorPreparationFailureAtRetainsSafeStageWithoutDispatch(t *testing.T) {
	f := supervisorFixture(t)
	driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
		return Preparation{}, PreparationFailureAt("attempt_publication", &core.Error{Code: "storage_unavailable", Message: "/private/path ghp_secret response body"})
	}}
	stop := runSupervisor(t, f, driver)
	receipt, _ := supervisorRequest(t, f, "attributed-preparation-failure", "cli", 2)
	waitOperationState(t, f, receipt.ID, "held")
	stop()
	response := f.request(http.MethodGet, "/api/v1/projects/"+attemptProject+"/reviews/operations/"+receipt.ID, f.reader, "", "")
	var operation Operation
	const want = "preparation_attempt_publication_storage_unavailable"
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &operation) != nil || operation.Reason != want {
		t.Fatalf("operation reason: %d %s", response.Code, response.Body.String())
	}
	var reason string
	if err := f.store.DB().QueryRow(`SELECT reason FROM review_execution_transitions WHERE attempt_id=? AND state='held'`, operation.AttemptID).Scan(&reason); err != nil || reason != want {
		t.Fatal("transition lost stage", err)
	}
	if driver.calls.Load() != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 || strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "ghp_secret") {
		t.Fatal("diagnostic exposed private data, dispatched, or retained a reservation")
	}
}
