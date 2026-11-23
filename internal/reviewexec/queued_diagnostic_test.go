package reviewexec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueuedReservationRequiresCurrentCapability(t *testing.T) {
	for _, channel := range []string{"hosted", "cli"} {
		t.Run(channel, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			holder := f.enqueue(t, attemptProject, "diagnostic-holder", channel, 2, 1)
			f.start(t, attemptProject, holder)
			waiting := f.enqueue(t, attemptProject, "diagnostic-waiter", channel, 4, 1)
			op := roundOperation(t, f, waiting.ID)
			scope := f.scope(t, attemptProject, f.writer, false)
			cause, reservation := f.e.queuedDiagnosticDetails(t.Context(), scope, op.ID, op.Revision)
			if cause != "reservation_busy" || reservation == nil || reservation.OperationID != holder.ID {
				t.Fatalf("compatible reservation omitted: %q %+v", cause, reservation)
			}
			f.configure(t, attemptProject, "other-account", 1, false)
			cause, reservation = f.e.queuedDiagnosticDetails(t.Context(), scope, op.ID, op.Revision)
			if cause != "awaiting_scheduler" || reservation != nil {
				t.Fatalf("obsolete capability must leave a generic diagnostic with no reservation: %q %+v", cause, reservation)
			}
		})
	}
}

func TestQueuedReservationRedactsOtherProject(t *testing.T) {
	f := newAttemptFixture(t)
	for _, project := range []string{attemptProject, otherAttemptProject} {
		f.configure(t, project, "test-capability", 0, false)
	}
	holder := f.enqueue(t, otherAttemptProject, "foreign-holder", "cli", 2, 1)
	f.start(t, otherAttemptProject, holder)
	waiting := f.enqueue(t, attemptProject, "local-waiter", "cli", 4, 1)
	mux := http.NewServeMux()
	f.e.Register(mux)

	state := func() string {
		return snapshotTable(t, f, "review_operations") + snapshotTable(t, f, "review_execution_attempts") +
			snapshotTable(t, f, "review_execution_reservations") + snapshotTable(t, f, "review_operation_waits") +
			snapshotTable(t, f, "events") + snapshotTable(t, f, "receipts")
	}
	get := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+f.writer)
		request.Header.Set(operationWaitHeader, "3")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		return response
	}
	before := state()
	base := "/api/v1/projects/" + attemptProject + "/reviews/operations/"
	for _, path := range []string{base + waiting.ID, base + waiting.ID + "/status"} {
		response := get(path)
		if response.Code != http.StatusOK || response.Header().Get(queuedDiagnosticHeader) != "reservation_busy" {
			t.Fatal("missing authorized queue diagnostic", response.Code, response.Header(), response.Body.String())
		}
		reservation := response.Header().Get(queuedReservationHeader)
		var holderProjection queuedReservation
		if err := json.Unmarshal([]byte(reservation), &holderProjection); err != nil || holderProjection != (queuedReservation{Kind: "other_project"}) {
			t.Fatal("reservation disclosed another project's state", reservation, err)
		}
		if strings.Contains(response.Body.String(), holder.ID) {
			t.Fatal("response body disclosed another project's operation")
		}
	}
	for _, path := range []string{base + holder.ID, "/api/v1/projects/" + otherAttemptProject + "/reviews/operations/" + holder.ID} {
		response := get(path)
		if response.Code != http.StatusForbidden && response.Code != http.StatusNotFound {
			t.Fatal("foreign operation was visible", response.Code)
		}
		if response.Header().Get(queuedDiagnosticHeader) != "" || response.Header().Get(queuedReservationHeader) != "" {
			t.Fatal("refused access disclosed a queue diagnostic", response.Header())
		}
	}
	if after := state(); after != before {
		t.Fatal("diagnostic reads changed durable operation or journal state")
	}
}
