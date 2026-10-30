package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestPredecessorRefusalRetainedAndNegotiatedAcrossResponses(t *testing.T) {
	f := supervisorFixture(t)
	for _, cause := range []string{"not_in_queue", "discovery_incomplete", "incoming_route_pending"} {
		key := "predecessor-" + cause
		receipt := f.enqueue(t, attemptProject, key, "cli", 2, 1)
		driver := &controlledDriver{prepare: func(context.Context, Operation, Configuration, Attempt) (Preparation, error) {
			return Preparation{}, predecessorUnfinishedHold(4, cause)
		}}
		if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
			t.Fatal(err)
		}
		want := &SourceRefusal{PullNumber: 4, Cause: cause}
		status, err := f.e.ReadOperationStatus(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), receipt.ID)
		if err != nil || status.State != "held" || status.Reason != "predecessor_unfinished" || !reflect.DeepEqual(status.SourceRefusal, want) || driver.calls.Load() != 0 || driver.prepares.Load() != 1 {
			t.Fatal("preparation hold attribution changed dispatch", status, err)
		}
		a := Attempt{ID: status.AttemptID, ProjectID: attemptProject, OperationID: receipt.ID}
		before := hostedObservationState(t, f, a)
		for _, view := range []string{"compact", "full", "collected"} {
			for _, header := range []string{"", "true", "v2", "v3"} {
				method, token := "GET", f.reader
				var body string
				if view == "collected" {
					method, token = "POST", f.writer
					body = encoded(Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
				}
				r := httptest.NewRequest(method, "/api/v1/projects/"+attemptProject+"/reviews/operations/"+receipt.ID, strings.NewReader(body))
				r.SetPathValue("project_id", attemptProject)
				r.SetPathValue("operation_id", receipt.ID)
				r.Header.Set("Authorization", "Bearer "+token)
				r.Header.Set("X-FC-Source-Refusal", header)
				w := httptest.NewRecorder()
				if view == "compact" {
					f.e.readOperationStatusHTTP(w, r)
				} else if view == "full" {
					f.e.readOperationHTTP(w, r)
				} else {
					r.Header.Set("Prefer", "wait=1")
					r.Header.Set("Idempotency-Key", key)
					f.e.enqueueHTTP(w, r)
				}
				var got struct {
					SourceRefusal *SourceRefusal `json:"source_refusal"`
				}
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
					t.Fatal(view, header, w.Code, w.Body.String())
				}
				if header == "v3" && !reflect.DeepEqual(got.SourceRefusal, want) || header != "v3" && got.SourceRefusal != nil {
					t.Fatal("cause escaped its negotiated response", view, header, w.Body.String())
				}
			}
		}
		if hostedObservationState(t, f, a) != before || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
			t.Fatal("diagnostics changed journal or review credit")
		}
	}
}

func TestPredecessorRefusalStrictShapeAndNoPrivateDetails(t *testing.T) {
	base := map[string]string{"reason": "predecessor_unfinished", "pull_number": "4", "cause": "discovery_incomplete", "private": "PRIVATE_NOT_RETAINED"}
	for _, cause := range []string{"not_in_queue", "discovery_incomplete", "incoming_route_pending"} {
		d := map[string]string{}
		for k, v := range base {
			d[k] = v
		}
		d["cause"] = cause
		got := sourceRefusalFromError(&core.Error{Code: "review_held", Details: d}, "held", "predecessor_unfinished", 2)
		if got == nil || got.PullNumber != 4 || got.Cause != cause || strings.Contains(sourceRefusalJSON(got), "PRIVATE") {
			t.Fatal(got)
		}
	}
	for _, mutation := range []map[string]string{{"cause": "private_or_unknown"}, {"cause": ""}, {"pull_number": "2"}, {"pull_number": "0"}, {"pull_number": "04"}, {"base_ref": "branch"}, {"parent_pull_number": "6"}, {"observed_base_sha": strings.Repeat("a", 40)}, {"expected_parent_sha": strings.Repeat("b", 40)}} {
		d := map[string]string{}
		for k, v := range base {
			d[k] = v
		}
		for k, v := range mutation {
			d[k] = v
		}
		if got := sourceRefusalFromError(&core.Error{Code: "review_held", Details: d}, "held", "predecessor_unfinished", 2); got != nil {
			t.Fatal("malformed known variant retained", got)
		}
	}
	for _, reason := range []string{"parent_unavailable", "predecessor_parent_moved"} {
		if validSourceRefusal(&SourceRefusal{PullNumber: 4, Cause: "discovery_incomplete"}, "held", reason, 2) {
			t.Fatal("cause accepted under another hold")
		}
	}
}

func TestPredecessorRefusalSchemaMigrationSnapshotAndRollback(t *testing.T) {
	f := supervisorFixture(t)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "old-parent-format", "cli", 2, 1))
	old := &SourceRefusal{PullNumber: 2, BaseRef: "former-parent"}
	var err error
	a, err = f.e.advanceWithSourceRefusal(t.Context(), attemptProject, a.ID, a.Revision, "held", "parent_unavailable", "", old)
	if err != nil {
		t.Fatal(err)
	}
	original := sourceRefusalJSON(old)
	admissionSQL(t, f, `UPDATE metadata SET value='2' WHERE key='schema:review_source_refusals'`)
	validate := func() error {
		tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback()
		return ValidateSourceRefusalsSnapshot(t.Context(), tx)
	}
	if err = validate(); err != nil {
		t.Fatal("valid retained v2 rejected", err)
	}
	admissionSQL(t, f, `CREATE TRIGGER reject_refusal_upgrade BEFORE UPDATE ON metadata WHEN NEW.key='schema:review_source_refusals' AND NEW.value='3' BEGIN SELECT RAISE(ABORT,'synthetic upgrade failure'); END`)
	if err = EnsureSchema(t.Context(), f.store); err == nil {
		t.Fatal("migration failure ignored")
	}
	var version, body string
	if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_source_refusals'`).Scan(&version); err != nil || version != "2" {
		t.Fatal("failed upgrade changed version", version, err)
	}
	admissionSQL(t, f, `DROP TRIGGER reject_refusal_upgrade`)
	if err = EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err = f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_source_refusals'`).Scan(&version); err != nil || version != "3" {
		t.Fatal(version, err)
	}
	if err = f.store.DB().QueryRow(`SELECT source_refusal_json FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&body); err != nil || body != original {
		t.Fatal("migration rewrote old metadata", body, err)
	}
	// A declared v1 snapshot retains only the older comparison union; null
	// historical diagnostics remain unknown under every supported version.
	comparison := &SourceRefusal{PullNumber: 4, ParentPullNumber: 6, ObservedBaseSHA: strings.Repeat("a", 40), ExpectedParentSHA: strings.Repeat("b", 40)}
	admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='predecessor_parent_moved',source_refusal_json=? WHERE attempt_id=? AND revision=?`, sourceRefusalJSON(comparison), a.ID, a.Revision)
	admissionSQL(t, f, `UPDATE metadata SET value='1' WHERE key='schema:review_source_refusals'`)
	if err = validate(); err != nil {
		t.Fatal("valid retained v1 rejected", err)
	}
	cause := sourceRefusalJSON(&SourceRefusal{PullNumber: 4, Cause: "not_in_queue"})
	admissionSQL(t, f, `UPDATE review_execution_transitions SET reason='predecessor_unfinished',source_refusal_json=? WHERE attempt_id=? AND revision=?`, cause, a.ID, a.Revision)
	admissionSQL(t, f, `UPDATE review_operations SET reason='predecessor_unfinished' WHERE id=?`, a.OperationID)
	for _, v := range []string{"1", "2", "3"} {
		admissionSQL(t, f, `UPDATE metadata SET value=? WHERE key='schema:review_source_refusals'`, v)
		err = validate()
		_, readErr := f.e.ReadOperationStatus(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), a.OperationID)
		if (err == nil) != (v == "3") || (readErr == nil) != (v == "3") {
			t.Fatal("declared older version accepted future cause", v, err, readErr)
		}
	}
	admissionSQL(t, f, `UPDATE review_execution_transitions SET source_refusal_json='null' WHERE attempt_id=? AND revision=?`, a.ID, a.Revision)
	if got, err := f.e.ReadOperationStatus(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), a.OperationID); err != nil || got.SourceRefusal != nil {
		t.Fatal("historical null fabricated details", got, err)
	}
	// Metadata failure must roll back the entire held transition and release.
	pending := f.start(t, attemptProject, f.enqueue(t, attemptProject, "cause-rollback", "cli", 2, 1))
	before := attemptState(t, f, pending)
	admissionSQL(t, f, `CREATE TRIGGER reject_cause BEFORE INSERT ON review_execution_transitions WHEN NEW.source_refusal_json<>'null' BEGIN SELECT RAISE(ABORT,'synthetic diagnostic failure'); END`)
	if _, err = f.e.advanceWithSourceRefusal(t.Context(), attemptProject, pending.ID, pending.Revision, "held", "predecessor_unfinished", "", &SourceRefusal{PullNumber: 4, Cause: "not_in_queue"}); err == nil || attemptState(t, f, pending) != before {
		t.Fatal("diagnostic failure partially held or released reservation", err)
	}
}

func TestPredecessorRefusalFinalGuardsNameActualBlocker(t *testing.T) {
	f, source, id := routeFixture(t, 1)
	// Target6 has predecessor4; the existing unresolved route to4 must remain
	// the first final guard without attributing its source PR2 as the blocker.
	created := routeCreate(t, f, id, "route-to-predecessor", routeRequest(0, 0, 4))
	parent := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 4, State: "open", HeadSHA: strings.Repeat("d", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ChangedFiles: 1}, ParentSHA: strings.Repeat("b", 40), ObservedAt: stamp()}
	current := ExecutionContext{Operation: Operation{PullNumber: 6, Channel: "cli"}, Configuration: Configuration{Mode: "stacked", Queue: []int64{2, 4, 6}}, RepositoryID: 801}
	target := Anchor{ParentPullNumber: 4, ParentSHA: parent.Pull.HeadSHA}
	before := routeState(t, f, source)
	check := func() error {
		return f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			return verifyPredecessors(ctx, tx, attemptProject, current, target, []SourcePlan{parent})
		})
	}
	got := sourceRefusalFromError(check(), "held", "predecessor_unfinished", 6)
	if !reflect.DeepEqual(got, &SourceRefusal{PullNumber: 4, Cause: "incoming_route_pending"}) {
		t.Fatal("wrong final route blocker", got)
	}
	if routeState(t, f, source) != before {
		t.Fatal("final guard changed route or accounting")
	}
	// After this explicit fixture decision clears the incoming obligation, the
	// existing channel discovery guard names the same still-unreviewed ancestor.
	routeDecision(t, f, created.ID, "clear-predecessor-route", 1, "rejected")
	before = routeState(t, f, source)
	got = sourceRefusalFromError(check(), "held", "predecessor_unfinished", 6)
	if !reflect.DeepEqual(got, &SourceRefusal{PullNumber: 4, Cause: "discovery_incomplete"}) || routeState(t, f, source) != before {
		t.Fatal("wrong final discovery blocker or changed state", got)
	}
}
