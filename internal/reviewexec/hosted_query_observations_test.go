package reviewexec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func queryObservationFixture(t *testing.T) (*attemptFixture, Attempt, HostedAllowanceQuery, HostedObservationInput) {
	t.Helper()
	f, a, anchor, poster, provider := hostedQueryFixture(t)
	query, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, a.Revision, anchor, poster, provider)
	if err != nil || !created {
		t.Fatal("query setup", err)
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	checksum := strings.Repeat("a", 64)
	in := HostedObservationInput{Kind: "allowance_query", CaptureID: "review_capture_" + checksum[:32], CaptureSHA256: checksum, CaptureBytes: 1024, StartedAt: at, FinishedAt: at, ObservedAt: at, ExpectedAnchor: anchor, Intent: query.Intent}
	return f, a, query, in
}

func TestHostedQueryObservationPreparingAppendReplayAndOwnership(t *testing.T) {
	f, a, query, input := queryObservationFixture(t)
	before := attemptState(t, f, a)
	receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	result, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	if err != nil || result.State != "observed" || result.Revision != 1 || attemptState(t, f, a) != before || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != receipts {
		t.Fatal("query reference changed execution/credit", result, err)
	}
	records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || len(records) != 1 || records[0].ID != result.ID || records[0].Input != input || records[0].Position != 1 {
		t.Fatal("query source lost", records, err)
	}
	var actor, payload string
	if err = f.store.DB().QueryRow(`SELECT actor_id,payload_json FROM events WHERE event_type='review.capture_observed'`).Scan(&actor, &payload); err != nil || actor != "exec-writer-principal" || strings.Contains(payload, query.Intent) || strings.Contains(payload, query.Anchor.Repository) {
		t.Fatal("source event lost attribution or leaked private input", payload, err)
	}
	retained := records[0]
	records[0].Input.Intent = "caller mutation"
	unchanged := hostedObservationState(t, f, a)
	replay, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	if err != nil || replay != result || hostedObservationState(t, f, a) != unchanged {
		t.Fatal("exact query reference replay repeated effect", err)
	}
	records, err = f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || !reflect.DeepEqual(records, []HostedObservation{retained}) {
		t.Fatal("caller changed immutable query source", err)
	}
}

func TestHostedQueryObservationSurvivesTerminalStateRestartAndRevocation(t *testing.T) {
	for _, state := range []string{"held", "cancelled", "completed"} {
		t.Run(state, func(t *testing.T) {
			f, a, query, input := queryObservationFixture(t)
			first, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			if state == "completed" {
				refreshed := query.Anchor
				refreshed.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
				body, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: refreshed.CandidateSHA})
				if err != nil {
					t.Fatal(err)
				}
				a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, refreshed, body)
				if err != nil {
					t.Fatal(err)
				}
				a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "query_fixture_posted", "100")
				if err != nil {
					t.Fatal(err)
				}
			}
			a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, state, "query_fixture_stopped", "")
			if err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
			cliAllowanceReopen(t, f)
			f.e, err = New(f.store, nil)
			if err != nil {
				t.Fatal(err)
			}
			before := hostedObservationState(t, f, a)
			records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
			if err != nil || len(records) != 1 || records[0].Input != input {
				t.Fatal("stopped query source unavailable", err)
			}
			replay, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
			if err != nil || replay != first || hostedObservationState(t, f, a) != before {
				t.Fatal("stopped replay changed lifecycle/effects", err)
			}
			// A separately retained query capture may arrive after preparation stopped.
			// Its original query anchor remains authoritative after later admission.
			input.CaptureSHA256 = strings.Repeat("b", 64)
			input.CaptureID = "review_capture_" + input.CaptureSHA256[:32]
			journal := attemptState(t, f, a)
			if _, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil || attemptState(t, f, a) != journal {
				t.Fatal("late query retention advanced/rejected stopped execution", err)
			}
			records, err = f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
			if err != nil || len(records) != 2 || records[1].Input.ExpectedAnchor != query.Anchor {
				t.Fatal("original query source replaced by later review admission", records, err)
			}
		})
	}
}

func TestHostedQueryObservationRejectsOtherScopeAnchorIntentAndFamily(t *testing.T) {
	for name, change := range map[string]func(*HostedObservationInput){
		"head":       func(in *HostedObservationInput) { in.ExpectedAnchor.CandidateSHA = strings.Repeat("f", 40) },
		"repository": func(in *HostedObservationInput) { in.ExpectedAnchor.RepositoryID++ },
		"anchor observed": func(in *HostedObservationInput) {
			in.ExpectedAnchor.ObservedAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
		},
		"intent":         func(in *HostedObservationInput) { in.Intent = "@coderabbitai rate limit" },
		"review family":  func(in *HostedObservationInput) { in.Kind = "" },
		"unknown family": func(in *HostedObservationInput) { in.Kind = "query" },
		"before query window": func(in *HostedObservationInput) {
			at := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
			in.StartedAt, in.ObservedAt, in.FinishedAt = at, at, at
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, _, input := queryObservationFixture(t)
			before := hostedObservationState(t, f, a)
			change(&input)
			if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err == nil {
				t.Fatal("unbound query capture retained")
			}
			if hostedObservationState(t, f, a) != before {
				t.Fatal("rejected reference mutated state")
			}
		})
	}
	f, a, _, input := queryObservationFixture(t)
	before := hostedObservationState(t, f, a)
	_, err := f.e.RecordHostedObservation(t.Context(), otherAttemptProject, a.ID, input)
	attemptCode(t, err, "not_found")
	_, err = f.e.ReadHostedObservations(t.Context(), otherAttemptProject, a.ID)
	attemptCode(t, err, "not_found")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("foreign scope changed journal")
	}
}

func TestHostedQueryObservationRejectsCorruptCanonicalAndIndexedBinding(t *testing.T) {
	for name, damage := range map[string]func(*testing.T, *attemptFixture, Attempt){
		"noncanonical body": func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json=body_json||' ' WHERE attempt_id=?`, a.ID)
		},
		"indexed hash": func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_hosted_observations SET capture_sha256=? WHERE attempt_id=?`, strings.Repeat("c", 64), a.ID)
		},
		"indexed operation": func(t *testing.T, f *attemptFixture, a Attempt) {
			other := f.enqueue(t, attemptProject, "other-index-operation", "hosted", 4, 1)
			admissionSQL(t, f, `UPDATE review_hosted_observations SET operation_id=? WHERE attempt_id=?`, other.ID, a.ID)
		},
		"query anchor": func(t *testing.T, f *attemptFixture, a Attempt) {
			q, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
			if err != nil {
				t.Fatal(err)
			}
			q.Anchor.PatchSHA256 = strings.Repeat("f", 64)
			body, err := json.Marshal(q)
			if err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET body_json=? WHERE attempt_id=?`, string(body), a.ID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, _, input := queryObservationFixture(t)
			if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil {
				t.Fatal(err)
			}
			damage(t, f, a)
			before := hostedObservationState(t, f, a)
			_, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
			attemptCode(t, err, "storage_unavailable")
			if hostedObservationState(t, f, a) != before {
				t.Fatal("read repaired corrupt source")
			}
		})
	}
}

func TestHostedQueryAndReviewObservationsShareCapacityWithoutBlockingReplay(t *testing.T) {
	f, a, query, input := queryObservationFixture(t)
	first, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	body, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: query.Anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, query.Anchor, body)
	if err != nil {
		t.Fatal(err)
	}
	review := input
	review.Kind = ""
	review.Intent = body
	for i := 1; i < hostedObservationLimit; i++ {
		review.CaptureSHA256 = fmt.Sprintf("%064x", i)
		review.CaptureID = "review_capture_" + review.CaptureSHA256[:32]
		if _, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, review); err != nil {
			t.Fatal(i, err)
		}
	}
	before := hostedObservationState(t, f, a)
	replay, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input)
	if err != nil || replay != first {
		t.Fatal("full shared journal blocked query replay", err)
	}
	changed := input
	changed.CaptureSHA256 = strings.Repeat("c", 64)
	changed.CaptureID = "review_capture_" + changed.CaptureSHA256[:32]
	_, err = f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, changed)
	attemptCode(t, err, "review_capacity")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("capacity/replay created effects")
	}
	records, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || len(records) != hostedObservationLimit || records[0].Input.Kind != "allowance_query" || records[hostedObservationLimit-1].Input.Kind != "" {
		t.Fatal("shared families lost ordering", len(records), err)
	}
}
