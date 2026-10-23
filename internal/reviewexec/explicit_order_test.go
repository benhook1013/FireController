package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func explicitOrderConfigure(t *testing.T, f *attemptFixture) {
	t.Helper()
	zero := int64(0)
	_, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "explicit-stack", Configuration{ExpectedRevision: &zero, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}})
	if err != nil {
		t.Fatal(err)
	}
}
func explicitOrderRequest(channel string, allow bool) Request {
	return Request{ExpectedConfigurationRevision: 1, PullNumber: 4, Channel: channel, HeadSHA: strings.Repeat("a", 40), AllowOutOfOrder: allow}
}
func explicitOrderEnqueue(t *testing.T, f *attemptFixture, key, channel string, allow bool, revision int64) Receipt {
	t.Helper()
	request := explicitOrderRequest(channel, allow)
	request.ExpectedConfigurationRevision = revision
	body, err := f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), key, request)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if json.Unmarshal(body, &receipt) != nil {
		t.Fatal("invalid receipt")
	}
	return receipt
}
func explicitOrderAnchor() Anchor {
	anchor := attemptAnchor(attemptProject, 4)
	anchor.ParentPullNumber = 2
	anchor.ParentSHA = anchor.BaseSHA
	anchor.BaseRef = "parent-branch"
	return anchor
}
func explicitOrderAdmit(t *testing.T, f *attemptFixture, a Attempt, channel string) Attempt {
	t.Helper()
	intent := ""
	if channel == "hosted" {
		var err error
		intent, err = reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: strings.Repeat("a", 40)})
		if err != nil {
			t.Fatal(err)
		}
	}
	admitted, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, explicitOrderAnchor(), intent)
	if err != nil {
		t.Fatal(err)
	}
	return admitted
}
func explicitOrderReopen(t *testing.T, f *attemptFixture) {
	t.Helper()
	var n int
	var name, path string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&n, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	if err = EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	f.e, err = New(store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
}

func TestExplicitOrderHTTPStrictBooleanAndRetainedChoice(t *testing.T) {
	f := newAttemptFixture(t)
	explicitOrderConfigure(t, f)
	path := "/api/v1/projects/" + attemptProject + "/reviews/requests"
	base := `{"expected_configuration_revision":1,"pull_number":4,"channel":"cli","head_sha":"` + strings.Repeat("a", 40) + `"`
	for i, tail := range []string{`,"allow_out_of_order":null}`, `,"allow_out_of_order":"true"}`, `,"allow_out_of_order":1}`, `,"allow_out_of_order":0}`, `,"allow_out_of_order":{}}`, `,"allow_out_of_order":[]}`, `,"allow_out_of_order":true,"allow_out_of_order":false}`, `,"allow_out_of_order":true,"unknown":1}`} {
		before := policyState(t, f)
		response := f.request("POST", path, f.writer, fmt.Sprintf("invalid-order-%d", i), base+tail)
		if response.Code != 400 || before != policyState(t, f) {
			t.Fatal("malformed choice admitted or changed state", response.Code, response.Body.String())
		}
	}
	for i, tail := range []string{`}`, `,"allow_out_of_order":false}`, `,"allow_out_of_order":true}`} {
		response := f.request("POST", path, f.writer, fmt.Sprintf("valid-order-%d", i), base+tail)
		var receipt Receipt
		if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &receipt) != nil {
			t.Fatal(response.Code, response.Body.String())
		}
		detail := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/operations/"+receipt.ID, f.reader, "", "")
		var operation Operation
		if detail.Code != 200 || json.Unmarshal(detail.Body.Bytes(), &operation) != nil || operation.AllowOutOfOrder != (i == 2) || (i < 2 && strings.Contains(detail.Body.String(), "allow_out_of_order")) {
			t.Fatal("choice not faithfully retained", detail.Code, detail.Body.String())
		}
	}
	for _, token := range []string{f.reader, f.other, ""} {
		before := policyState(t, f)
		response := f.request("POST", path, token, "unauthorized-explicit", base+`,"allow_out_of_order":true}`)
		if response.Code == 202 || before != policyState(t, f) {
			t.Fatal("explicit choice changed authorization", response.Code)
		}
	}
}

func TestExplicitOrderLegacyFingerprintMigrationAndRestart(t *testing.T) {
	f := newAttemptFixture(t)
	explicitOrderConfigure(t, f)
	scope := f.scope(t, attemptProject, f.writer, false)
	request := explicitOrderRequest("cli", false)
	// This exact pre-extension struct is the historical fingerprint input.
	legacy := struct {
		ExpectedConfigurationRevision int64  `json:"expected_configuration_revision"`
		PullNumber                    int64  `json:"pull_number"`
		Channel                       string `json:"channel"`
		HeadSHA                       string `json:"head_sha"`
	}{1, 4, "cli", request.HeadSHA}
	if encoded(legacy) != encoded(request) {
		t.Fatal("ordinary request fingerprint bytes changed")
	}
	original, err := f.store.Mutate(t.Context(), scope, core.Mutation{Operation: "reviews.request", Target: "requests", Key: "legacy-order-key", Request: legacy}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		return f.e.enqueueRequestTx(ctx, tx, scope, request)
	})
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	_ = json.Unmarshal(original, &receipt)
	old, err := f.e.ReadOperation(t.Context(), scope, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Model a genuine schema6 snapshot: the new column is absent, not malformed.
	admissionSQL(t, f, `ALTER TABLE review_operations DROP COLUMN allow_out_of_order`)
	admissionSQL(t, f, `UPDATE metadata SET value='6' WHERE key='schema:review_execution'`)
	if read, err := readOperation(t.Context(), f.store.DB(), attemptProject, receipt.ID); err != nil || read.AllowOutOfOrder || !reflect.DeepEqual(old, read) {
		t.Fatal("old snapshot read did not retain false", read, err)
	}
	explicitOrderReopen(t, f)
	if after, err := f.e.ReadOperation(t.Context(), scope, receipt.ID); err != nil || !reflect.DeepEqual(old, after) {
		t.Fatal("migration rewrote original operation", after, err)
	}
	replay, err := f.e.Enqueue(t.Context(), scope, "legacy-order-key", request)
	if err != nil || !bytes.Equal(original, replay) {
		t.Fatal("legacy key no longer replays", err)
	}
	request.AllowOutOfOrder = true
	_, err = f.e.Enqueue(t.Context(), scope, "legacy-order-key", request)
	attemptCode(t, err, "idempotency_conflict")
	explicit := explicitOrderEnqueue(t, f, "retained-explicit", "hosted", true, 1)
	a := explicitOrderAdmit(t, f, f.start(t, attemptProject, explicit), "hosted")
	before := attemptState(t, f, a)
	intent := a.Intent
	explicitOrderReopen(t, f)
	op, err := f.e.ReadOperation(t.Context(), scope, explicit.ID)
	if err != nil || !op.AllowOutOfOrder || attemptState(t, f, a) != before {
		t.Fatal("restart lost retained choice/intent", op, err)
	}
	retained, err := readAttempt(t.Context(), f.store.DB(), attemptProject, a.ID)
	if err != nil || retained.Intent != intent || retained.Intent == "" {
		t.Fatal("restart changed exact external intent", err)
	}
	if _, err = f.store.DB().Exec(`UPDATE review_operations SET allow_out_of_order=2 WHERE id=?`, explicit.ID); err == nil {
		t.Fatal("schema admitted nonboolean")
	}
}

func TestExplicitOrderFinalGuardBypassesOnlyProgressionBothChannels(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, allow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", channel, allow), func(t *testing.T) {
				hostedAdmissionNoHTTP(t)
				f := newAttemptFixture(t)
				explicitOrderConfigure(t, f)
				a := explicitOrderAdmit(t, f, f.start(t, attemptProject, explicitOrderEnqueue(t, f, "order-final", channel, allow, 1)), channel)
				predecessor := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 2, State: "open", HeadSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("c", 40), BaseRef: "main", ChangedFiles: 1}, ParentSHA: strings.Repeat("c", 40), ObservedAt: stamp()}
				if allow {
					hostedAdmissionVerify(t, f, a, "", "")
					hostedAdmissionVerify(t, f, a, "validation_failed", "", predecessor)
				} else {
					hostedAdmissionVerify(t, f, a, "validation_failed", "")
					hostedAdmissionVerify(t, f, a, "review_held", "predecessor_unfinished", predecessor)
				}
				if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`)+attemptCount(t, f.store, `SELECT count(*) FROM review_round_completions`) != 0 {
					t.Fatal("bypass manufactured credit")
				}
			})
		}
	}
}

func TestExplicitOrderFinalGuardRetainsTargetGuardsBothChannels(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, mode := range []string{"configuration", "policy", "actor", "source-hold", "pending-evidence", "round-closed", "reservation", "wrong-anchor"} {
			t.Run(channel+"/"+mode, func(t *testing.T) {
				hostedAdmissionNoHTTP(t)
				f := newAttemptFixture(t)
				explicitOrderConfigure(t, f)
				if mode == "round-closed" {
					roundGrant(t, f, 4, channel, 0, 0, 3, "initial-grant")
				}
				a := explicitOrderAdmit(t, f, f.start(t, attemptProject, explicitOrderEnqueue(t, f, "guarded-explicit", channel, true, 1)), channel)
				code := "review_conflict"
				reason := ""
				switch mode {
				case "configuration":
					f.configure(t, attemptProject, "test-capability", 1, true)
				case "policy":
					admissionPolicy(t, f, 1)
				case "actor":
					admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
					code = "forbidden"
				case "source-hold":
					integrationImportHold(t, f, 4)
					code = "review_held"
					reason = "source_obligations_pending"
				case "pending-evidence":
					coverageSeedOperations(t, f, a, 1, "completed")
					code = "review_evidence_pending"
				case "round-closed":
					one := int64(1)
					_, err := f.e.ConfigureRoundControl(t.Context(), f.scope(t, attemptProject, f.admin, true), "late-close", 4, channel, RoundControlRequest{ExpectedRevision: &one, Action: "close"})
					if err != nil {
						t.Fatal(err)
					}
					code = "review_round_closed"
				case "reservation":
					admissionSQL(t, f, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID)
				case "wrong-anchor":
					anchor := *a.Anchor
					anchor.CandidateSHA = strings.Repeat("9", 40)
					admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, encoded(anchor), a.ID)
				}
				hostedAdmissionVerify(t, f, a, code, reason)
			})
		}
	}
}

func TestExplicitOrderIncomingRoutesGuardEveryDirectBoundary(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		for _, allow := range []bool{false, true} {
			for _, phase := range []string{"enqueue", "prepare", "admit", "dispatch", "dirty"} {
				t.Run(fmt.Sprintf("%s/%t/%s", channel, allow, phase), func(t *testing.T) {
					hostedAdmissionNoHTTP(t)
					f, source, id := routeFixture(t, 1)
					adjudicationComplete(t, f, source)
					var a Attempt
					if phase != "enqueue" {
						a = f.start(t, attemptProject, explicitOrderEnqueue(t, f, "route-target", channel, allow, 2))
						if phase == "dispatch" || phase == "dirty" {
							a = explicitOrderAdmit(t, f, a, channel)
						}
					}
					route := routeCreate(t, f, id, "late-target-route", routeRequest(0, 0, 4))
					if phase == "dirty" {
						admissionSQL(t, f, `UPDATE review_finding_route_revisions SET body_json='{}' WHERE route_id=? AND revision=1`, route.ID)
					}
					before := routeState(t, f, source)
					var err error
					switch phase {
					case "enqueue":
						request := explicitOrderRequest(channel, allow)
						request.ExpectedConfigurationRevision = 2
						_, err = f.e.Enqueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "route-target", request)
					case "prepare":
						_, err = f.e.ExecutionContext(t.Context(), attemptProject, a.OperationID)
					case "admit":
						intent := ""
						if channel == "hosted" {
							intent, _ = reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: strings.Repeat("a", 40)})
						}
						_, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, explicitOrderAnchor(), intent)
					default:
						err = f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision)
					}
					if phase == "dirty" {
						attemptCode(t, err, "storage_unavailable")
					} else {
						attemptCode(t, err, "review_held")
						var failure *core.Error
						if !errors.As(err, &failure) || !reflect.DeepEqual(failure.Details, map[string]string{"reason": "routed_findings_pending"}) {
							t.Fatal("wrong route hold", err)
						}
					}
					if routeState(t, f, source) != before {
						t.Fatal("held direct request changed route/journal/evidence state")
					}
				})
			}
		}
	}
}

func TestExplicitOrderAutomaticChildRemainsFalseIncludingOldSnapshot(t *testing.T) {
	f, scope := selectionSnapshotFixture(t)
	receipt := selectionSnapshotEnqueue(t, f, scope, "automatic-order")
	if worked, err := f.e.runNext(t.Context(), map[string]TargetReader{"test-capability": selectionSnapshotReader{}}); err != nil || !worked {
		t.Fatal(err)
	}
	run, err := readRun(t.Context(), f.store.DB(), attemptProject, receipt.ID)
	if err != nil || run.State != "selected" {
		t.Fatal(run, err)
	}
	op, err := readOperation(t.Context(), f.store.DB(), attemptProject, run.OperationID)
	if err != nil || op.AllowOutOfOrder {
		t.Fatal("automatic selection gained bypass", op, err)
	}
	if err = ValidateSelectionSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE review_operations SET allow_out_of_order=1 WHERE id=?`, op.ID)
	if err = ValidateSelectionSnapshot(t.Context(), f.store.DB()); err == nil {
		t.Fatal("forged automatic bypass admitted")
	}
	admissionSQL(t, f, `UPDATE review_operations SET allow_out_of_order=0 WHERE id=?`, op.ID)
	admissionSQL(t, f, `ALTER TABLE review_operations DROP COLUMN allow_out_of_order`)
	admissionSQL(t, f, `UPDATE metadata SET value='6' WHERE key='schema:review_execution'`)
	if err = ValidateSelectionSnapshot(t.Context(), f.store.DB()); err != nil {
		t.Fatal("genuine schema6 automatic child refused", err)
	}
}

func TestExplicitOrderMalformedStoredFlagOrVersionNeverDefaults(t *testing.T) {
	for _, mode := range []string{"nonboolean", "text", "blob", "missing-current-column", "unknown-version", "noncanonical-version"} {
		t.Run(mode, func(t *testing.T) {
			f := newAttemptFixture(t)
			explicitOrderConfigure(t, f)
			receipt := explicitOrderEnqueue(t, f, "stored-flag", "cli", true, 1)
			switch mode {
			case "nonboolean", "text", "blob":
				value := map[string]string{"nonboolean": "2", "text": "'true'", "blob": "CAST('1' AS BLOB)"}[mode]
				admissionSQL(t, f, `PRAGMA ignore_check_constraints=ON`)
				admissionSQL(t, f, `UPDATE review_operations SET allow_out_of_order=`+value+` WHERE id=?`, receipt.ID)
				admissionSQL(t, f, `PRAGMA ignore_check_constraints=OFF`)
			case "missing-current-column":
				admissionSQL(t, f, `ALTER TABLE review_operations DROP COLUMN allow_out_of_order`)
			case "unknown-version":
				admissionSQL(t, f, `UPDATE metadata SET value='8' WHERE key='schema:review_execution'`)
			case "noncanonical-version":
				admissionSQL(t, f, `UPDATE metadata SET value='07' WHERE key='schema:review_execution'`)
			}
			if _, err := readOperation(t.Context(), f.store.DB(), attemptProject, receipt.ID); err == nil {
				t.Fatal("malformed current state silently became false")
			}
		})
	}
}

func TestExplicitOrderRestoredProviderReleaseRequiresMigratedSeven(t *testing.T) {
	f := newAttemptFixture(t)
	explicitOrderConfigure(t, f)
	receipt := explicitOrderEnqueue(t, f, "old-queued", "cli", false, 1)
	restoredHeld(t, f)
	admissionSQL(t, f, `ALTER TABLE review_operations DROP COLUMN allow_out_of_order`)
	admissionSQL(t, f, `UPDATE metadata SET value='6' WHERE key='schema:review_execution'`)
	before := policyState(t, f)
	_, err := restoredCall(t, f, time.Now())
	attemptCode(t, err, "restore_reconciliation_schema")
	if policyState(t, f) != before || restoredSQL(t, f, `SELECT state FROM review_operations WHERE id=?`, receipt.ID) != "queued" {
		t.Fatal("unmigrated reconciliation changed work")
	}
	if err = EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	summary, err := restoredCall(t, f, time.Now())
	if err != nil || summary.QueuedOperations != 1 || restoredSQL(t, f, `SELECT allow_out_of_order FROM review_operations WHERE id=?`, receipt.ID) != "0" {
		t.Fatal("current migration did not preserve/reconcile old work", summary, err)
	}
}

func TestExplicitOrderRestoredProviderReleaseRetainsChoiceAndRefusesCorruptFlags(t *testing.T) {
	for _, mode := range []string{"valid", "nonboolean", "text", "blob", "missing-column"} {
		t.Run(mode, func(t *testing.T) {
			f := newAttemptFixture(t)
			explicitOrderConfigure(t, f)
			receipt := explicitOrderEnqueue(t, f, "restored-explicit", "cli", true, 1)
			restoredHeld(t, f)
			if mode == "missing-column" {
				admissionSQL(t, f, `ALTER TABLE review_operations DROP COLUMN allow_out_of_order`)
			} else if mode != "valid" {
				value := map[string]string{"nonboolean": "2", "text": "'true'", "blob": "CAST('1' AS BLOB)"}[mode]
				admissionSQL(t, f, `PRAGMA ignore_check_constraints=ON`)
				admissionSQL(t, f, `UPDATE review_operations SET allow_out_of_order=`+value+` WHERE id=?`, receipt.ID)
				admissionSQL(t, f, `PRAGMA ignore_check_constraints=OFF`)
			}
			before := policyState(t, f)
			summary, err := restoredCall(t, f, time.Now())
			if mode == "valid" {
				if err != nil || summary.QueuedOperations != 1 || restoredSQL(t, f, `SELECT allow_out_of_order FROM review_operations WHERE id=?`, receipt.ID) != "1" {
					t.Fatal("reconciliation lost explicit provenance", summary, err)
				}
			} else {
				code := "restore_reconciliation_invalid"
				if mode == "missing-column" {
					code = "restore_reconciliation_schema"
				}
				attemptCode(t, err, code)
				if before != policyState(t, f) || restoredSQL(t, f, `SELECT state FROM review_operations WHERE id=?`, receipt.ID) != "queued" {
					t.Fatal("corrupt flag refusal partially settled work")
				}
			}
		})
	}
}
