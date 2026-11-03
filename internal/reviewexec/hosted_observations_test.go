package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedObservationFixture(t *testing.T) (*attemptFixture, Attempt, HostedObservationInput) {
	t.Helper()
	f, a, source := evidenceFixture(t, "hosted", false)
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: source.ExpectedAnchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, source.ExpectedAnchor, intent)
	if err != nil {
		t.Fatal(err)
	}
	return f, a, HostedObservationInput{CaptureID: source.CaptureID, CaptureSHA256: source.CaptureSHA256, CaptureBytes: source.CaptureBytes, StartedAt: source.StartedAt, FinishedAt: source.FinishedAt, ObservedAt: source.FinishedAt, ExpectedAnchor: source.ExpectedAnchor, Intent: intent}
}

func hostedObservationState(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var observations string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(id||body_json,''),'') FROM review_hosted_observations`).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	return observations + evidenceState(t, f, a)
}

func TestHostedObservationsConcurrentReplayRestartAndSettlement(t *testing.T) {
	f, a, input := hostedObservationFixture(t)
	before := attemptState(t, f, a)
	receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	type result struct {
		r   Receipt
		err error
	}
	results := make(chan result, 6)
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		go func() {
			<-start
			r, err := f.e.RecordHostedObservation(context.Background(), attemptProject, a.ID, input)
			results <- result{r, err}
		}()
	}
	close(start)
	var receipt Receipt
	for i := 0; i < 6; i++ {
		got := <-results
		if got.err != nil || receipt.ID != "" && receipt != got.r {
			t.Fatal("concurrent reference replay changed identity", got.err)
		}
		receipt = got.r
	}
	if receipt.State != "observed" || receipt.Revision != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_observations`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.capture_observed'`) != 1 || attemptState(t, f, a) != before || receipts != attemptCount(t, f.store, `SELECT count(*) FROM receipts`) {
		t.Fatal("reference changed journal or duplicated effects", receipt)
	}
	records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || len(records) != 1 || records[0].ID != receipt.ID || records[0].Input != input || records[0].Position != 1 || records[0].OperationID != a.OperationID {
		t.Fatal("retained reference lost binding", records, err)
	}
	var actor, payload string
	if err := f.store.DB().QueryRow(`SELECT actor_id,payload_json FROM events WHERE event_type='review.capture_observed'`).Scan(&actor, &payload); err != nil || actor != "exec-writer-principal" || len(payload) > 1024 || !strings.Contains(payload, input.CaptureID) || !strings.Contains(payload, a.ID) || strings.Contains(payload, input.Intent) {
		t.Fatal("unsafe reference event", payload, err)
	}
	// Reads return owned values, not caller-controlled live storage.
	records[0].Input.Intent = "changed caller copy"
	cliAllowanceReopen(t, f)
	if err := EnsureEvidenceSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	f.e, err = New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	if err != nil || replay != receipt {
		t.Fatal("restart/empty catalog lost replay", err)
	}
	active, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_retained", "100")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.e.Advance(t.Context(), attemptProject, a.ID, active.Revision, "completed", "capture_retained", ""); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	settled := hostedObservationState(t, f, a)
	if replay, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil || replay != receipt || hostedObservationState(t, f, a) != settled {
		t.Fatal("exact replay after settlement/revocation changed state", err)
	}
	changed := input
	changed.CaptureBytes++
	_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, changed)
	attemptCode(t, err, "review_conflict")
	changed = input
	changed.CaptureSHA256 = strings.Repeat("2", 64)
	changed.CaptureID = "review_capture_" + changed.CaptureSHA256[:32]
	_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, changed)
	attemptCode(t, err, "review_conflict")
	if hostedObservationState(t, f, a) != settled {
		t.Fatal("late/changed observation mutated settled state")
	}
	records, err = f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || len(records) != 1 || records[0].Input != input {
		t.Fatal("settled references unavailable", err)
	}
	_, err = f.e.ReadHostedObservations(t.Context(), otherAttemptProject, a.ID)
	attemptCode(t, err, "not_found")
}

func TestHostedObservationsAdmissionValidationAndAtomicity(t *testing.T) {
	for name, change := range map[string]func(*HostedObservationInput){
		"capture id":           func(in *HostedObservationInput) { in.CaptureID += "x" },
		"hash":                 func(in *HostedObservationInput) { in.CaptureSHA256 = strings.Repeat("A", 64) },
		"bytes zero":           func(in *HostedObservationInput) { in.CaptureBytes = 0 },
		"bytes limit":          func(in *HostedObservationInput) { in.CaptureBytes = (48 << 20) + 1 },
		"noncanonical time":    func(in *HostedObservationInput) { in.StartedAt = "2026-01-01T00:00:00+00:00" },
		"reversed observation": func(in *HostedObservationInput) { in.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano) },
		"future": func(in *HostedObservationInput) {
			in.FinishedAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
		},
		"anchor":      func(in *HostedObservationInput) { in.ExpectedAnchor.CandidateSHA = "bad" },
		"intent utf8": func(in *HostedObservationInput) { in.Intent = "\xff" },
	} {
		t.Run(name, func(t *testing.T) {
			f, a, input := hostedObservationFixture(t)
			before := hostedObservationState(t, f, a)
			change(&input)
			_, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
			attemptCode(t, err, "validation_failed")
			if hostedObservationState(t, f, a) != before {
				t.Fatal("invalid observation had effects")
			}
		})
	}
	for name, change := range map[string]func(*HostedObservationInput){
		"intent":     func(in *HostedObservationInput) { in.Intent = "@coderabbitai review" },
		"head":       func(in *HostedObservationInput) { in.ExpectedAnchor.CandidateSHA = strings.Repeat("f", 40) },
		"repository": func(in *HostedObservationInput) { in.ExpectedAnchor.RepositoryID++ },
		"parent":     func(in *HostedObservationInput) { in.ExpectedAnchor.ParentSHA = strings.Repeat("f", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			f, a, input := hostedObservationFixture(t)
			before := hostedObservationState(t, f, a)
			change(&input)
			_, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
			attemptCode(t, err, "review_conflict")
			if hostedObservationState(t, f, a) != before {
				t.Fatal("changed binding had effects")
			}
		})
	}
	for name, sql := range map[string]string{
		"channel":            `UPDATE review_operations SET channel='cli'`,
		"stored intent":      `UPDATE review_execution_attempts SET intent='other'`,
		"stored repository":  `UPDATE projects SET github_repository_id=999 WHERE id='exec-project-one'`,
		"operation identity": `UPDATE review_operations SET attempt_id=''`,
		"capability":         `UPDATE review_operations SET capability_fingerprint=''`,
		"preparing":          `UPDATE review_execution_attempts SET state='preparing'`,
	} {
		t.Run(name, func(t *testing.T) {
			f, a, input := hostedObservationFixture(t)
			admissionSQL(t, f, sql)
			before := hostedObservationState(t, f, a)
			_, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
			attemptCode(t, err, "review_conflict")
			if hostedObservationState(t, f, a) != before {
				t.Fatal("invalid retained admission had effects")
			}
		})
	}
	f, a, input := hostedObservationFixture(t)
	before := hostedObservationState(t, f, a)
	admissionSQL(t, f, `CREATE TRIGGER reject_observation_event BEFORE INSERT ON events WHEN NEW.event_type='review.capture_observed' BEGIN SELECT RAISE(ABORT,'private injected failure'); END`)
	_, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	attemptCode(t, err, "storage_unavailable")
	if hostedObservationState(t, f, a) != before || strings.Contains(err.Error(), "injected") {
		t.Fatal("event rollback was incomplete or leaked details", err)
	}
}

func TestHostedObservationsCapacityAndIndexedRecovery(t *testing.T) {
	f, a, input := hostedObservationFixture(t)
	var first Receipt
	original := input
	for i := 1; i <= 200; i++ {
		input.CaptureSHA256 = fmt.Sprintf("%064x", i)
		input.CaptureID = "review_capture_" + input.CaptureSHA256[:32]
		r, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
		if err != nil {
			t.Fatal(i, err)
		}
		if i == 1 {
			first = r
			original = input
		}
	}
	before := hostedObservationState(t, f, a)
	replay, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, original)
	if err != nil || replay != first {
		t.Fatal("full journal blocked replay", err)
	}
	input.CaptureSHA256 = fmt.Sprintf("%064x", 201)
	input.CaptureID = "review_capture_" + input.CaptureSHA256[:32]
	_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	attemptCode(t, err, "review_capacity")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("capacity failure had effects")
	}
	records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || len(records) != 200 || records[199].Position != 200 {
		t.Fatal("bounded recovery lost references", err)
	}
	rows, err := f.store.DB().Query(`EXPLAIN QUERY PLAN SELECT position FROM review_hosted_observations WHERE project_id=? AND attempt_id=? ORDER BY position LIMIT 201`, attemptProject, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "review_hosted_observation_attempt") || strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN review_hosted") {
		t.Fatal("unbounded/sorting recovery plan", plan)
	}
}

func TestHostedObservationsCorruptionFailsClosed(t *testing.T) {
	for name, change := range map[string]func(*HostedObservation){
		"id":            func(r *HostedObservation) { r.ID = "review_hosted_observation_" + strings.Repeat("f", 32) },
		"operation":     func(r *HostedObservation) { r.OperationID = "review_operation_" + strings.Repeat("f", 32) },
		"reference":     func(r *HostedObservation) { r.Input.CaptureBytes++ },
		"anchor":        func(r *HostedObservation) { r.Input.ExpectedAnchor.ParentSHA = strings.Repeat("f", 40) },
		"position":      func(r *HostedObservation) { r.Position++ },
		"recorded time": func(r *HostedObservation) { r.RecordedAt = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			f, a, input := hostedObservationFixture(t)
			if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil {
				t.Fatal(err)
			}
			records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			r := records[0]
			change(&r)
			body, _ := json.Marshal(r)
			admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json=?`, string(body))
			before := hostedObservationState(t, f, a)
			got, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
			attemptCode(t, err, "storage_unavailable")
			if got != nil || hostedObservationState(t, f, a) != before {
				t.Fatal("corrupt reference escaped or read mutated state")
			}
		})
	}
}

func TestHostedObservationSchemaTwoMigrationPreservesFinalEvidenceAndReplay(t *testing.T) {
	f, a, input := evidenceFixture(t, "cli", true)
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	before := evidenceState(t, f, a)
	admissionSQL(t, f, `DROP TABLE review_hosted_observations`)
	admissionSQL(t, f, `DROP INDEX review_evidence_hosted_publication`)
	admissionSQL(t, f, `DROP INDEX review_evidence_hosted_run`)
	admissionSQL(t, f, `UPDATE metadata SET value='2' WHERE key='schema:review_evidence'`)
	cliAllowanceReopen(t, f)
	if err := EnsureEvidenceSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := EnsureEvidenceSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_evidence'`).Scan(&version); err != nil || version != "4" || evidenceState(t, f, a) != before {
		t.Fatal("migration changed retained evidence", version, err)
	}
	replay, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, input)
	if err != nil || replay != receipt {
		t.Fatal("migration broke final evidence replay", err)
	}
	// The new empty recovery read does not invent rows for historical CLI captures.
	records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || !reflect.DeepEqual(records, []HostedObservation{}) {
		t.Fatal("migration invented hosted history", records, err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 {
		t.Fatal("historical final evidence lost")
	}
}

func TestHostedObservationReplayRejectsCorruptIndexedScope(t *testing.T) {
	f, a, input := hostedObservationFixture(t)
	if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE review_hosted_observations SET project_id=?`, otherAttemptProject)
	before := hostedObservationState(t, f, a)
	_, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	attemptCode(t, err, "storage_unavailable")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("corrupt replay repaired/changed a foreign indexed scope")
	}
}

func TestHostedObservationUncertainCanRetainNewFacts(t *testing.T) {
	f, a, input := hostedObservationFixture(t)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "response_lost", ""); err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, a)
	if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil {
		t.Fatal("uncertain attempt could not retain correct target", err)
	}
	if attemptState(t, f, a) != before {
		t.Fatal("raw reference recovery advanced/retried uncertain work")
	}
}
