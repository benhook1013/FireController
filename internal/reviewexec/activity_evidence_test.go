package reviewexec

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestActivityEvidenceIsPrivateCurrentAndIdempotent(t *testing.T) {
	f := roundFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "spawn-evidence", "cli", 2, 1))
	a, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	v := ActivityEvidence{AttemptID: a.ID, HeadSHA: strings.Repeat("a", 40), Kind: "cli_process_spawned", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	v.ProcessStartedAt = v.ObservedAt
	if err := f.e.RecordActivityEvidence(t.Context(), attemptProject, a.ID, v); err != nil {
		t.Fatal(err)
	}
	later := v
	later.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	later.ProcessStartedAt = later.ObservedAt
	if err := f.e.RecordActivityEvidence(t.Context(), attemptProject, a.ID, later); err != nil {
		t.Fatal(err)
	}
	if count := attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.activity_observed'`); count != 1 {
		t.Fatal("repeated signal emitted event", count)
	}
	var payload string
	if err := f.store.DB().QueryRow(`SELECT payload_json FROM events WHERE event_type='review.activity_observed'`).Scan(&payload); err != nil || payload != "{}" {
		t.Fatal("private signal leaked into event", payload, err)
	}
	if got := progressStageChannel(t, f, "view=stages", 1); got["activity_evidence"] != nil {
		t.Fatal("ordinary read leaked optional evidence")
	}
	got := progressStageChannel(t, f, "view=stages&include_activity_evidence=true", 1)
	raw, _ := json.Marshal(got["activity_evidence"])
	var read ActivityEvidence
	if json.Unmarshal(raw, &read) != nil || read != v {
		t.Fatal("lost first proven spawn", string(raw), v)
	}
	for _, optIn := range []bool{false, true} {
		path := roundProgressPath(attemptProject) + "?pull_numbers=2&view=stages"
		if optIn {
			path += "&include_activity_evidence=true"
		}
		response := versionWaitRead(t, f, path, "5")
		var combined RoundProgress
		if err := json.Unmarshal(response.Body.Bytes(), &combined); err != nil {
			t.Fatal(err)
		}
		if len(combined.Items) != 1 || len(combined.Items[0].Channels) != 2 {
			t.Fatal("missing combined progress shape")
		}
		evidence := combined.Items[0].Channels[1].ActivityEvidence
		if optIn && (evidence == nil || *evidence != v) || !optIn && evidence != nil {
			t.Fatal("version5 activity opt-in changed", optIn, evidence)
		}
	}
	wrong := v
	wrong.HeadSHA = strings.Repeat("b", 40)
	if err := f.e.RecordActivityEvidence(t.Context(), attemptProject, a.ID, wrong); err == nil {
		t.Fatal("accepted other head")
	}
	if err := f.e.RecordActivityEvidence(t.Context(), otherAttemptProject, a.ID, v); err == nil {
		t.Fatal("accepted other project")
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateActivityEvidenceSnapshot(t.Context(), tx); err != nil {
		tx.Rollback()
		t.Fatal("valid retained evidence rejected", err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE review_activity_evidence SET body_json=json_set(body_json,'$.head_sha',?)`, strings.Repeat("b", 40)); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := ValidateActivityEvidenceSnapshot(t.Context(), tx); err == nil {
		tx.Rollback()
		t.Fatal("restore accepted mismatched evidence")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "failed", "process_failed", ""); err != nil {
		t.Fatal(err)
	}
	if got := progressStageChannel(t, f, "view=stages&include_activity_evidence=true", 1); got["activity_evidence"] != nil {
		t.Fatal("terminal evidence portrayed as current")
	}
}

func TestActivityEvidenceRejectsUnprovenTimesAndMixedKinds(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	good := ActivityEvidence{AttemptID: "review_attempt_123", HeadSHA: strings.Repeat("a", 40), Kind: "cli_process_spawned", ObservedAt: now, ProcessStartedAt: now}
	if !validActivityEvidence(good) {
		t.Fatal("valid signal refused")
	}
	for _, edit := range []func(*ActivityEvidence){func(v *ActivityEvidence) { v.ProcessStartedAt = "" }, func(v *ActivityEvidence) { v.SourceCommentID = 1 }, func(v *ActivityEvidence) { v.ObservedAt = "yesterday" }, func(v *ActivityEvidence) { v.Kind = "review_started" }} {
		v := good
		edit(&v)
		if validActivityEvidence(v) {
			t.Fatal("invalid signal accepted", v)
		}
	}
}

func TestActivityEvidenceHTTPOptInIsClosed(t *testing.T) {
	f := roundFixture(t)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	for _, query := range []string{"include_activity_evidence=true", "view=stages&include_activity_evidence=false", "view=stages&include_activity_evidence=true&include_activity_evidence=true"} {
		w := f.request(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=2&"+query, f.reader, "", "")
		if w.Code != 400 {
			t.Fatal("invalid projection option accepted", query, w.Code)
		}
	}
	w := f.request(http.MethodGet, roundProgressPath(attemptProject)+"?pull_numbers=2&view=stages&include_activity_evidence=true", f.other, "", "")
	if w.Code != 403 {
		t.Fatal("foreign project evidence accessible", w.Code)
	}
}

func TestActivityEvidenceSurvivesUncertainCaptureWithoutChangingStart(t *testing.T) {
	f := roundFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "uncertain-evidence", "cli", 2, 1))
	a, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	at := time.Now().UTC().Format(time.RFC3339Nano)
	v := ActivityEvidence{AttemptID: a.ID, HeadSHA: strings.Repeat("a", 40), Kind: "cli_process_spawned", ObservedAt: at, ProcessStartedAt: at}
	if err := f.e.RecordActivityEvidence(t.Context(), attemptProject, a.ID, v); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "capture_retention_failed", ""); err != nil {
		t.Fatal(err)
	}
	got := progressStageChannel(t, f, "view=stages&include_activity_evidence=true", 1)
	raw, _ := json.Marshal(got["activity_evidence"])
	var read ActivityEvidence
	if json.Unmarshal(raw, &read) != nil || read != v {
		t.Fatal("uncertain review lost original provider start", string(raw))
	}
	if got := progressStageChannel(t, f, "view=stages", 1); got["activity_evidence"] != nil {
		t.Fatal("evidence leaked without opt-in")
	}
}

func TestSubmissionEvidenceUsesOnlyMatchedAcknowledgedTrigger(t *testing.T) {
	f, a, _, _ := incompleteHostedFixture(t)
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var at string
	if err := tx.QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND state='active' AND reason='trigger_recorded'`, a.ID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	got, err := readSubmissionEvidence(t.Context(), tx, attemptProject, 2, "hosted")
	if err != nil || got == nil || got.AttemptID != a.ID || got.TriggerID != "101" || got.SubmittedAt != at {
		t.Fatalf("lost submission acknowledgement: %+v %v", got, err)
	}
	for _, channel := range []string{"cli", "manual"} {
		if other, err := readSubmissionEvidence(t.Context(), tx, attemptProject, 2, channel); err != nil || other != nil {
			t.Fatal("foreign channel estimate", other, err)
		}
	}
	if other, err := readSubmissionEvidence(t.Context(), tx, otherAttemptProject, 2, "hosted"); err != nil || other != nil {
		t.Fatal("foreign project estimate", other, err)
	}
	if _, err := tx.Exec(`UPDATE review_execution_transitions SET reason='trigger_recovered' WHERE attempt_id=? AND reason='trigger_recorded'`, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := readSubmissionEvidence(t.Context(), tx, attemptProject, 2, "hosted"); err != nil || got != nil {
		t.Fatal("recovery time used as submission", got, err)
	}
	if _, err := tx.Exec(`UPDATE review_execution_transitions SET reason='trigger_recorded',external_id='999' WHERE attempt_id=? AND reason='trigger_recovered'`, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := readSubmissionEvidence(t.Context(), tx, attemptProject, 2, "hosted"); err != nil || got != nil {
		t.Fatal("unrelated trigger used as submission", got, err)
	}
}

func TestHostedSubmissionEvidenceLegacyActiveAcknowledgement(t *testing.T) {
	f, attempt, _ := hostedObservationFixture(t)
	a, err := f.e.Advance(t.Context(), attemptProject, attempt.ID, attempt.Revision, "active", "trigger_recorded", "101")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := readSubmissionEvidence(t.Context(), tx, attemptProject, 2, "hosted")
	if err != nil || got == nil || got.AttemptID != a.ID || got.TriggerID != "101" || got.SubmittedAt == "" {
		t.Fatalf("active direct request lost its original acknowledgement: %+v %v", got, err)
	}
}

func TestHostedSubmissionEvidenceActiveAndExactReplay(t *testing.T) {
	f, attempt, input := hostedObservationFixture(t)
	a, err := f.e.Advance(t.Context(), attemptProject, attempt.ID, attempt.Revision, "active", "trigger_recovered", "101")
	if err != nil {
		t.Fatal(err)
	}
	read := func() *SubmissionEvidence {
		t.Helper()
		tx, err := f.store.DB().BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		got, err := readSubmissionEvidence(t.Context(), tx, attemptProject, 2, "hosted")
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); got != nil {
		t.Fatalf("recovery timestamp supplied by itself: %+v", got)
	}
	now := time.Now().UTC()
	trigger := reviewprovider.HostedTriggerRecovery{State: "adopted", CommentID: 101, CreatedAt: now.Truncate(time.Second)}
	record := func(anchor Anchor, trigger reviewprovider.HostedTriggerRecovery, at time.Time) error {
		return f.e.RecordHostedSubmissionEvidence(t.Context(), attemptProject, a.ID, anchor, trigger, at)
	}
	if err = record(input.ExpectedAnchor, trigger, now); err != nil {
		t.Fatal(err)
	}
	if err = record(input.ExpectedAnchor, trigger, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	want := SubmissionEvidence{AttemptID: a.ID, HeadSHA: input.ExpectedAnchor.CandidateSHA, TriggerID: "101", SubmittedAt: trigger.CreatedAt.Format(time.RFC3339Nano)}
	if got := read(); got == nil || *got != want {
		t.Fatalf("active request projection: got %+v, want %+v", got, want)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	raw, _ := json.Marshal(progressStageChannel(t, f, "view=stages&include_activity_evidence=true", 0)["submission_evidence"])
	var projected SubmissionEvidence
	if err := json.Unmarshal(raw, &projected); err != nil || projected != want {
		t.Fatalf("private progress omitted active request: %s %v", raw, err)
	}
	if got := progressStageChannel(t, f, "view=stages", 0)["submission_evidence"]; got != nil {
		t.Fatal("request timing leaked outside opt-in progress", got)
	}
	changed := trigger
	changed.CreatedAt = changed.CreatedAt.Add(-time.Second)
	if err = record(input.ExpectedAnchor, changed, now); err == nil {
		t.Fatal("conflicting original timestamp replaced the retained receipt")
	}
	changed = trigger
	changed.CommentID++
	if err = record(input.ExpectedAnchor, changed, now); err == nil {
		t.Fatal("accepted unrelated trigger")
	}
	var events int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM events WHERE event_type='review.activity_observed' AND resource_id=?`, a.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("submission published %d refresh events: %v", events, err)
	}
}
