package reviewexec

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func progressStageChannel(t *testing.T, f *attemptFixture, query string, channel int) map[string]any {
	t.Helper()
	w := f.request(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=2&"+query, f.reader, "", "")
	var response struct {
		Items []struct {
			Channels []map[string]any `json:"channels"`
		} `json:"items"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Items) != 1 || len(response.Items[0].Channels) != 2 {
		t.Fatal("invalid progress", w.Code, w.Body.String())
	}
	return response.Items[0].Channels[channel]
}

func assertProgressStages(t *testing.T, value map[string]any, execution, adjudication float64) {
	t.Helper()
	if value["pending_execution"] != execution || value["pending_adjudication"] != adjudication || value["pending"] != execution+adjudication {
		t.Fatal("stages lost or double-counted occupancy", value)
	}
}

func TestRoundProgressStagesExecutionAdjudicationAndFinalization(t *testing.T) {
	f := roundFixture(t)
	roundGrant(t, f, 2, "cli", 0, 0, 8, "grant-stages")
	a, evidence := roundCapture(t, f, 600, true)
	active := f.enqueue(t, attemptProject, "active-stages", "cli", 2, 1)
	attempt := f.start(t, attemptProject, active)
	if _, err := f.e.Admit(t.Context(), attemptProject, attempt.ID, attempt.Revision, attemptAnchor(attemptProject, 2), ""); err != nil {
		t.Fatal(err)
	}
	// Rebuilding without capabilities proves these are provider-free local reads.
	var err error
	f.e, err = New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	before := roundTables(t, f) + policyState(t, f) + attemptState(t, f, a)
	staged := progressStageChannel(t, f, "view=stages", 1)
	assertProgressStages(t, staged, 1, 1)
	read, err := f.e.ReadRoundProgressStages(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), []int64{2})
	if err != nil || len(read.Items) != 1 || read.Items[0].Channels[1].PendingExecution == nil || read.Items[0].Channels[1].PendingAdjudication == nil || *read.Items[0].Channels[1].PendingExecution != 1 || *read.Items[0].Channels[1].PendingAdjudication != 1 {
		t.Fatal("staged engine read disagrees", read, err)
	}
	assertProgressStages(t, progressStageChannel(t, f, "view=stages", 0), 0, 0)
	legacy := progressStageChannel(t, f, "", 1)
	if _, ok := legacy["pending_execution"]; ok {
		t.Fatal("legacy response gained stages")
	}
	if _, ok := legacy["pending_adjudication"]; ok {
		t.Fatal("legacy response gained stages")
	}
	delete(staged, "pending_execution")
	delete(staged, "pending_adjudication")
	if !reflect.DeepEqual(staged, legacy) || before != roundTables(t, f)+policyState(t, f)+attemptState(t, f, a) {
		t.Fatal("read changed accounting or legacy fields")
	}
	roundFinalize(t, f, a, evidence, 600, true)
	assertProgressStages(t, progressStageChannel(t, f, "view=stages", 1), 1, 0)
}

func TestRoundProgressStagesLegacyEpochAndStrictReadScope(t *testing.T) {
	f := roundFixture(t)
	seedLegacyRoundGrant(t, f, 2, "cli", 0, 0, 8, "legacy-stages")
	roundCapture(t, f, 601, true)
	// Legacy controls retain terminal-unfinalized capacity until adjudication.
	assertProgressStages(t, progressStageChannel(t, f, "view=stages", 1), 0, 1)
	roundGrant(t, f, 2, "cli", 1, 0, 8, "convert-stages")
	assertProgressStages(t, progressStageChannel(t, f, "view=stages", 1), 0, 1)
	for _, query := range []string{"view=", "view=all", "view=stages&view=stages", "view=stages&unknown=x"} {
		w := f.request(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=2&"+query, f.reader, "", "")
		if w.Code != http.StatusBadRequest {
			t.Fatal("invalid view accepted", query, w.Code)
		}
	}
	w := f.request(http.MethodGet, roundProgressPath(otherAttemptProject)+"?pull_numbers=2&view=stages", f.reader, "", "")
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), `"items"`) {
		t.Fatal("foreign project exposed", w.Code)
	}
	admissionSQL(t, f, `UPDATE grants SET can_read=0 WHERE credential_id='exec-reader-cred'`)
	w = f.request(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=2&view=stages", f.reader, "", "")
	if w.Code != http.StatusForbidden {
		t.Fatal("revoked grant retained stage access", w.Code)
	}
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
	w = f.request(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=2&view=stages", f.reader, "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatal("revoked credential retained stage access", w.Code)
	}
	// A predecessor-format epoch replacement fixture retains its original
	// capture/binding while a later epoch owns current capacity.
	g := roundFixture(t)
	seedLegacyRoundGrant(t, g, 2, "cli", 0, 0, 8, "old-epoch")
	roundCapture(t, g, 602, true)
	seedLegacyRoundGrant(t, g, 2, "cli", 1, 0, 8, "new-epoch")
	roundGrant(t, g, 2, "cli", 2, 0, 8, "convert-new-epoch")
	g.enqueue(t, attemptProject, "current-epoch-stage", "cli", 2, 1)
	assertProgressStages(t, progressStageChannel(t, g, "view=stages", 1), 1, 0)
}

func TestRoundProgressStagesCancelledWaitRetainsExactUnusedBaseline(t *testing.T) {
	fixtures := map[string]func(*testing.T) (*attemptFixture, Attempt){
		"hosted": func(t *testing.T) (*attemptFixture, Attempt) {
			f, a, _, _ := queuedCooldown(t)
			return f, a
		},
		"cli": func(t *testing.T) (*attemptFixture, Attempt) {
			f, a, _, _ := cliWaitFixture(t)
			if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
				t.Fatal(err)
			}
			return f, a
		},
	}
	for channel, fixture := range fixtures {
		t.Run(channel, func(t *testing.T) {
			f, a := fixture(t)
			if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
				t.Fatal(err)
			}
			index := 0
			if channel == "cli" {
				index = 1
			}
			scope := evidenceReadScope(t, f, attemptProject, f.reader)
			before, err := f.e.ReadRoundProgressStages(t.Context(), scope, []int64{2})
			if err != nil || before.Items[0].Channels[index].Wait == nil || before.Items[0].Channels[index].CancelledWait != nil {
				t.Fatal("queued wait became cancelled history", before, err)
			}
			timerControl(t, f, a.OperationID, "cancel", "cancel-progress-wait")
			o := bindingOperation(t, f, scope, a.OperationID)
			digest := func() string {
				var value string
				for _, table := range []string{"review_operations", "review_operation_waits", "review_execution_attempts", "review_execution_transitions", "review_execution_reservations", "receipts", "events"} {
					value += snapshotTable(t, f, table)
				}
				return value + roundTables(t, f)
			}
			original := digest()
			// No provider catalog or browser history is needed for this local
			// association; the original allowance record remains authoritative.
			f.e, err = New(f.store, nil)
			if err != nil {
				t.Fatal(err)
			}
			progress, err := f.e.ReadRoundProgressStages(t.Context(), scope, []int64{4, 2})
			if err != nil {
				t.Fatal(err)
			}
			got := progress.Items[1].Channels[index]
			want := &CancelledWait{BaselineAttemptID: a.ID, HeadSHA: o.HeadSHA, CancelledAt: o.UpdatedAt}
			if !reflect.DeepEqual(got.CancelledWait, want) || got.Wait != nil || got.Pending != 0 || got.PendingExecution == nil || *got.PendingExecution != 0 || got.PendingAdjudication == nil || *got.PendingAdjudication != 0 {
				t.Fatal("cancelled unused baseline lost or became pending", got, want)
			}
			if progress.Items[0].Channels[index].CancelledWait != nil || progress.Items[1].Channels[1-index].CancelledWait != nil {
				t.Fatal("cancelled association crossed PR/channel", progress)
			}
			other, err := f.e.ReadRoundProgressStages(t.Context(), evidenceReadScope(t, f, otherAttemptProject, f.other), []int64{2})
			if err != nil || other.Items[0].Channels[index].CancelledWait != nil {
				t.Fatal("cancelled association crossed project", other, err)
			}
			legacy, err := f.e.ReadRoundProgress(t.Context(), scope, []int64{2})
			if err != nil || legacy.Items[0].Channels[index].CancelledWait != nil {
				t.Fatal("legacy progress gained cancellation metadata", legacy, err)
			}
			if digest() != original {
				t.Fatal("progress rewrote original held attempt, wait, accounting or events")
			}
		})
	}
}

func TestRoundProgressStagesCancelledWaitRejectsUnrelatedAttemptBinding(t *testing.T) {
	f, a, _, _ := queuedCooldown(t)
	if err := EnsureRoundSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	timerControl(t, f, a.OperationID, "cancel", "cancel-bound-wait")
	unrelated := f.start(t, attemptProject, f.enqueue(t, attemptProject, "unrelated-wait-attempt", "hosted", 4, 1))
	admissionSQL(t, f, `UPDATE review_operation_waits SET baseline_attempt_id=? WHERE operation_id=?`, unrelated.ID, a.OperationID)
	if _, err := f.e.ReadRoundProgressStages(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), []int64{2}); err == nil {
		t.Fatal("unrelated attempt supplied cancellation disposition")
	}
}
