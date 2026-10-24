package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func admissionPolicy(t *testing.T, f *attemptFixture, previous int64) {
	t.Helper()
	input := policyInput(previous)
	policyConfigure(t, f, f.scope(t, attemptProject, f.admin, true), "admission-policy-"+strconv.FormatInt(previous, 10), input)
}

func TestPolicyAdmissionEnqueueRequiresPolicyAndRetainsKeyedRevision(t *testing.T) {
	f := newAttemptFixtureWithPolicy(t, false)
	f.configure(t, attemptProject, "test-capability", 0, false)
	scope := f.scope(t, attemptProject, f.writer, false)
	request := Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
	before := policyState(t, f)
	_, err := f.e.Enqueue(t.Context(), scope, "missing-policy", request)
	attemptCode(t, err, "reviews_policy_unconfigured")
	if policyState(t, f) != before || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("missing policy allocated request effects")
	}
	// Required channels describe sufficiency, not which channel may run. An
	// optional CLI request remains independent of required hosted coverage.
	input := policyInput(0)
	input.RequiredChannels = []string{"hosted"}
	policyConfigure(t, f, f.scope(t, attemptProject, f.admin, true), "explicit-policy", input)
	before = policyState(t, f)
	original, err := f.e.Enqueue(t.Context(), scope, "cli-key", request)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if json.Unmarshal(original, &receipt) != nil {
		t.Fatal("invalid receipt")
	}
	operation, err := f.e.ReadOperation(t.Context(), scope, receipt.ID)
	if err != nil || operation.PolicyRevision != 1 {
		t.Fatal("request did not bind policy revision", operation, err)
	}
	prepared := f.start(t, attemptProject, receipt)
	active, err := f.e.Admit(t.Context(), attemptProject, prepared.ID, prepared.Revision, attemptAnchor(attemptProject, 2), "")
	if err != nil {
		t.Fatal("required hosted coverage blocked CLI admission", err)
	}
	if err := f.e.VerifyAction(t.Context(), attemptProject, active.ID, active.Revision); err != nil {
		t.Fatal("required hosted coverage blocked CLI final check", err)
	}
	operation, err = f.e.ReadOperation(t.Context(), scope, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.configure(t, attemptProject, "test-capability", 1, true)
	before = policyState(t, f)
	replay, err := f.e.Enqueue(t.Context(), scope, "cli-key", request)
	if err != nil || !bytes.Equal(original, replay) || policyState(t, f) != before {
		t.Fatal("policy update changed exact keyed recovery", err)
	}
	retained, err := f.e.ReadOperation(t.Context(), scope, receipt.ID)
	if err != nil || !reflect.DeepEqual(operation, retained) {
		t.Fatal("replay silently adopted new policy", retained, err)
	}
	pausedRequest := request
	pausedRequest.ExpectedConfigurationRevision = 2
	_, err = f.e.Enqueue(t.Context(), scope, "new-paused-key", pausedRequest)
	attemptCode(t, err, "reviews_paused")
	if policyState(t, f) != before || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 1 {
		t.Fatal("paused new key changed journal")
	}
	if _, err = f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.Enqueue(t.Context(), scope, "cli-key", request)
	attemptCode(t, err, "unauthorized")
}

func TestPolicyAdmissionStartHoldsWithoutPreparingOrDispatch(t *testing.T) {
	for _, state := range []string{"changed", "paused", "missing", "legacy-unbound"} {
		t.Run(state, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			receipt := f.enqueue(t, attemptProject, "queued", "cli", 2, 1)
			reason := "policy_changed"
			switch state {
			case "changed":
				admissionPolicy(t, f, 1)
			case "paused":
				f.configure(t, attemptProject, "test-capability", 1, true)
				reason = "configuration_changed"
			case "missing":
				if _, err := f.store.DB().Exec(`DELETE FROM review_policy_config WHERE project_id=?`, attemptProject); err != nil {
					t.Fatal(err)
				}
				reason = "policy_unconfigured"
			case "legacy-unbound":
				if _, err := f.store.DB().Exec(`UPDATE review_operations SET policy_revision=0 WHERE id=?`, receipt.ID); err != nil {
					t.Fatal(err)
				}
				reason = "policy_unbound"
			}
			driver := &controlledDriver{}
			if err := f.e.processNext(t.Context(), map[string]Driver{"test-capability": driver}); err != nil {
				t.Fatal(err)
			}
			operation, err := f.e.ReadOperation(t.Context(), f.scope(t, attemptProject, f.writer, false), receipt.ID)
			if err != nil || operation.State != "held" || operation.Reason != reason || operation.AttemptID != "" {
				t.Fatal("policy did not durably hold request", operation, err)
			}
			if driver.prepares.Load() != 0 || driver.calls.Load() != 0 {
				t.Fatal("held request reached driver")
			}
			for _, table := range []string{"review_execution_attempts", "review_execution_reservations", "review_execution_transitions"} {
				if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
					t.Fatal("held request created attempt state", table)
				}
			}
		})
	}
}

func TestPolicyAdmissionBetweenPreparationAndAdmitIsReadOnlyFailure(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "revision", true: "pause"}[paused], func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "queued", "cli", 2, 1))
			current, err := f.e.ExecutionContext(t.Context(), attemptProject, a.OperationID)
			if err != nil || current.Operation.PolicyRevision != 1 || current.Policy.Revision != 1 || current.RepositoryID != 801 || current.Configuration.CapabilityID != "test-capability" {
				t.Fatal("preparation context did not expose exact binding", current, err)
			}
			if paused {
				f.configure(t, attemptProject, "test-capability", 1, true)
			} else {
				admissionPolicy(t, f, 1)
			}
			before := attemptState(t, f, a)
			_, err = f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "")
			attemptCode(t, err, "review_conflict")
			if attemptState(t, f, a) != before {
				t.Fatal("failed policy admission changed attempt, reservation or transition")
			}
			current, err = f.e.ExecutionContext(t.Context(), attemptProject, a.OperationID)
			attemptCode(t, err, "review_conflict")
			if !reflect.DeepEqual(current, ExecutionContext{}) || attemptState(t, f, a) != before {
				t.Fatal("failed preparation context returned partial facts or effects")
			}
		})
	}
}

func TestVerifyActionCurrentBindingsAndReservation(t *testing.T) {
	for _, test := range []struct {
		name, code string
		change     func(*testing.T, *attemptFixture, Attempt)
	}{
		{"unchanged", "", nil},
		{"policy-changed", "review_conflict", func(t *testing.T, f *attemptFixture, a Attempt) { admissionPolicy(t, f, 1) }},
		{"policy-paused", "review_conflict", func(t *testing.T, f *attemptFixture, a Attempt) {
			f.configure(t, attemptProject, "test-capability", 1, true)
		}},
		{"policy-unbound", "review_conflict", func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_operations SET policy_revision=0 WHERE id=?`, a.OperationID)
		}},
		{"config-changed", "review_conflict", func(t *testing.T, f *attemptFixture, a Attempt) {
			f.configure(t, attemptProject, "test-capability", 1, false)
		}},
		{"capability-changed", "review_conflict", func(t *testing.T, f *attemptFixture, a Attempt) {
			var err error
			f.e, err = New(f.store, []Capability{{ID: "test-capability", Provider: "coderabbit", Account: "test-account", Channels: []string{"cli", "hosted"}, Fingerprint: strings.Repeat("f", 64)}})
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"actor-revoked", "forbidden", func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
		}},
		{"worker-removed", "forbidden", func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `DELETE FROM workers WHERE project_id=? AND id='admission-worker'`, attemptProject)
		}},
		{"reservation-missing", "review_conflict", func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID)
		}},
		{"reservation-wrong", "review_conflict", func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_execution_reservations SET pull_number=4 WHERE attempt_id=?`, a.ID)
		}},
		{"uncertain", "review_conflict", func(t *testing.T, f *attemptFixture, a Attempt) {
			_, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "test_uncertain", "")
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"terminal", "review_conflict", func(t *testing.T, f *attemptFixture, a Attempt) {
			_, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "test_complete", "")
			if err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			admissionSQL(t, f, `INSERT INTO workers(project_id,id,name) VALUES(?,'admission-worker','Builder')`, attemptProject)
			scope := f.scope(t, attemptProject, f.writer, false)
			scope.WorkerID = "admission-worker"
			body, err := f.e.Enqueue(t.Context(), scope, "worker-request", Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)})
			var r Receipt
			if err != nil || json.Unmarshal(body, &r) != nil {
				t.Fatal(err)
			}
			prepared := f.start(t, attemptProject, r)
			a, err := f.e.Admit(t.Context(), attemptProject, prepared.ID, prepared.Revision, attemptAnchor(attemptProject, 2), "")
			if err != nil {
				t.Fatal(err)
			}
			if test.change != nil {
				test.change(t, f, a)
			}
			before := attemptState(t, f, a)
			journal := policyState(t, f)
			// Use the current revision to prove terminal/uncertain state is rejected in
			// its own right, rather than merely because the old revision is stale.
			var revision int64
			if err := f.store.DB().QueryRow(`SELECT revision FROM review_execution_attempts WHERE id=?`, a.ID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			err = f.e.VerifyAction(t.Context(), attemptProject, a.ID, revision)
			if test.code == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				attemptCode(t, err, test.code)
			}
			if attemptState(t, f, a) != before || policyState(t, f) != journal {
				t.Fatal("final local verification changed journal")
			}
			if test.change == nil {
				attemptCode(t, f.e.VerifyAction(t.Context(), otherAttemptProject, a.ID, revision), "not_found")
				attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, a.ID, revision+1), "review_conflict")
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				start := time.Now()
				if f.e.VerifyAction(ctx, attemptProject, a.ID, revision) == nil || time.Since(start) > time.Second {
					t.Fatal("cancelled guard did not fail promptly")
				}
			}
		})
	}
}

func admissionSQL(t *testing.T, f *attemptFixture, query string, args ...any) {
	t.Helper()
	if _, err := f.store.DB().Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyActionHostedPostingOnly(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "hosted-request", "hosted", 2, 1))
	a, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 2), "exact-hosted-intent")
	if err != nil {
		t.Fatal(err)
	}
	before := attemptState(t, f, a)
	if err := f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision); err != nil || before != attemptState(t, f, a) {
		t.Fatal("posting hosted action was not verifiable", err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "posted", "comment123")
	if err != nil {
		t.Fatal(err)
	}
	attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision), "review_conflict")
}

func TestPolicyAdmissionVersionTwoMigrationRetainsUnboundJournal(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	scope := f.scope(t, attemptProject, f.writer, false)
	request := Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "cli", HeadSHA: strings.Repeat("a", 40)}
	body, err := f.e.Enqueue(t.Context(), scope, "pre-policy-key", request)
	if err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if json.Unmarshal(body, &receipt) != nil {
		t.Fatal("receipt")
	}
	before, err := f.e.ReadOperation(t.Context(), scope, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `DROP TABLE review_hosted_preflight`)
	admissionSQL(t, f, `DROP TABLE review_hosted_allowance_queries`)
	admissionSQL(t, f, `DROP INDEX review_operations_completed_comparison`)
	admissionSQL(t, f, `DROP TABLE review_cli_preflight`)
	admissionSQL(t, f, `DROP INDEX IF EXISTS review_operations_coverage`)
	admissionSQL(t, f, `ALTER TABLE review_operations DROP COLUMN policy_revision`)
	admissionSQL(t, f, `ALTER TABLE review_operations DROP COLUMN allow_out_of_order`)
	admissionSQL(t, f, `UPDATE metadata SET value='2' WHERE key='schema:review_execution'`)
	var sequence int
	var name, path string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
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
	if err := EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	f.e, err = New(store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.e.ReadOperation(t.Context(), scope, receipt.ID)
	before.PolicyRevision = 0
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("policy migration rewrote retained fields", after, err)
	}
	replay, err := f.e.Enqueue(t.Context(), scope, "pre-policy-key", request)
	if err != nil || !bytes.Equal(body, replay) {
		t.Fatal("migration lost exact receipt", err)
	}
	a, err := f.e.StartAttempt(t.Context(), attemptProject, receipt.ID)
	if err != nil || a.ID != "" {
		t.Fatal("unbound operation started", a, err)
	}
	after, err = f.e.ReadOperation(t.Context(), scope, receipt.ID)
	if err != nil || after.State != "held" || after.Reason != "policy_unbound" || after.PolicyRevision != 0 {
		t.Fatal("legacy policy binding did not hold", after, err)
	}
	if attemptCount(t, store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("migration hold created work")
	}
}

func TestVerifyActionRejectsAllHeadPendingAndLateCorrection(t *testing.T) {
	f, prior, id, current := coverageFixture(t, 1, false)
	adjudicationComplete(t, f, prior)
	next := coverageNextAttempt(t, f, "new-head-intent", "cli", strings.Repeat("9", 40))
	before := adjudicationState(t, f, next)
	attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, next.ID, next.Revision), "review_evidence_pending")
	if adjudicationState(t, f, next) != before {
		t.Fatal("refused action mutated journal")
	}
	adjudicationDecide(t, f, id, "reject-old", 0, 0, "rejected")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "finalize-old", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, id, "finalized", 2)
	if err := f.e.VerifyAction(t.Context(), attemptProject, next.ID, next.Revision); err != nil {
		t.Fatal("closed old comparison blocked new head", err)
	}
	// A decision correction can land after preparation's source/coverage read.
	// The final guard must re-read that obligation in its own current snapshot.
	adjudicationDecide(t, f, id, "late-acceptance", 2, 0, "accepted")
	before = adjudicationState(t, f, next)
	attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, next.ID, next.Revision), "review_evidence_pending")
	if adjudicationState(t, f, next) != before {
		t.Fatal("late refusal mutated state")
	}
	coverageFix(t, f, id, "late-fix", 3, 0, "")
	if err := f.e.VerifyAction(t.Context(), attemptProject, next.ID, next.Revision); err != nil {
		t.Fatal(err)
	}
	coverageComparison(t, f, prior, current, ComparisonState{Covered: true})
}

func TestVerifyActionAlreadyCoveredAndHostedIndependent(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f, prior, id, current := coverageFixture(t, 0, false)
			coverageFinalize(t, f, prior, id, 0)
			next := coverageNextAttempt(t, f, "explicit-current", channel, current.Pull.HeadSHA)
			before := adjudicationState(t, f, next)
			err := f.e.VerifyAction(t.Context(), attemptProject, next.ID, next.Revision)
			if channel == "cli" {
				attemptCode(t, err, "review_held")
			} else if err != nil {
				t.Fatal("CLI coverage blocked hosted", err)
			}
			if adjudicationState(t, f, next) != before {
				t.Fatal("action check changed state")
			}
		})
	}
}

func TestVerifyActionStackedRequiresExactActualParentWitness(t *testing.T) {
	f, prior, id, open := coverageFixture(t, 1, false)
	adjudicationDecide(t, f, id, "reject-predecessor", 0, 0, "rejected")
	coverageFinalize(t, f, prior, id, 1)
	previous := int64(1)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "stack-with-merged", Configuration{ExpectedRevision: &previous, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 3, 4}, Channels: []string{"cli", "hosted"}}); err != nil {
		t.Fatal(err)
	}
	child := f.start(t, attemptProject, f.enqueue(t, attemptProject, "stack-child", "cli", 4, 2))
	anchor := attemptAnchor(attemptProject, 4)
	anchor.ParentPullNumber, anchor.ParentSHA, anchor.BaseSHA, anchor.BaseRef = 2, open.Pull.HeadSHA, open.Pull.HeadSHA, "parent-branch"
	child, err := f.e.Admit(t.Context(), attemptProject, child.ID, child.Revision, anchor, "")
	if err != nil {
		t.Fatal(err)
	}
	merged := open
	merged.Pull.Number, merged.Pull.Merged, merged.Pull.State = 3, true, "closed"
	merged.ParentSHA, merged.ParentPullNumber = "", 0
	open.Pull.HeadRef = "parent-branch"
	dependencies := []SourcePlan{open}
	before := adjudicationState(t, f, child)
	if err := f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, dependencies...); err != nil {
		t.Fatal("valid covered actual parent failed", err)
	}
	for _, test := range []struct {
		name   string
		change func([]SourcePlan) []SourcePlan
	}{
		{"missing-all", func([]SourcePlan) []SourcePlan { return nil }},
		{"unrelated-merged-prefix", func(in []SourcePlan) []SourcePlan { return append(in, merged) }},
		{"duplicate", func(in []SourcePlan) []SourcePlan { return append(in, in[0]) }},
		{"wrong-parent-number", func(in []SourcePlan) []SourcePlan { in[0].Pull.Number = 3; return in }},
		{"foreign-repository", func(in []SourcePlan) []SourcePlan { in[0].Repository.ID = 802; return in }},
		{"foreign-pull-repository", func(in []SourcePlan) []SourcePlan { in[0].Pull.RepositoryID = 802; return in }},
		{"malformed-head", func(in []SourcePlan) []SourcePlan { in[0].Pull.HeadSHA = "bad"; return in }},
		{"closed-unmerged", func(in []SourcePlan) []SourcePlan { in[0].Pull.State = "closed"; return in }},
		{"merged", func(in []SourcePlan) []SourcePlan { in[0].Pull.State, in[0].Pull.Merged = "closed", true; return in }},
		{"malformed-open-parent", func(in []SourcePlan) []SourcePlan { in[0].ParentSHA = strings.Repeat("9", 40); return in }},
		{"missing-observation", func(in []SourcePlan) []SourcePlan { in[0].ObservedAt = ""; return in }},
	} {
		t.Run(test.name, func(t *testing.T) {
			plans := test.change(append([]SourcePlan(nil), dependencies...))
			attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, plans...), "validation_failed")
			if adjudicationState(t, f, child) != before {
				t.Fatal("invalid predecessor proof changed retained state")
			}
		})
	}
	uncovered := append([]SourcePlan(nil), dependencies...)
	uncovered[0].Pull.HeadSHA = strings.Repeat("9", 40)
	attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, uncovered...), "validation_failed")
	if adjudicationState(t, f, child) != before {
		t.Fatal("moved actual parent refusal changed retained state")
	}
	// The proof was ready at preparation, but a real current correction now needs
	// a fix. The final guard must check predecessors and target in the same read.
	adjudicationDecide(t, f, id, "late-predecessor-acceptance", 2, 0, "accepted")
	afterCorrection := adjudicationState(t, f, child)
	attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, dependencies...), "review_held")
	if adjudicationState(t, f, child) != afterCorrection {
		t.Fatal("late predecessor refusal mutated state")
	}
	coverageFix(t, f, id, "fix-corrected-predecessor", 3, 0, "")
	if err := f.e.VerifyAction(t.Context(), attemptProject, child.ID, child.Revision, dependencies...); err != nil {
		t.Fatal("fixed predecessor remained held", err)
	}
}

func TestVerifyActionIndependentAndHostedRejectUnexpectedCLIPlans(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			f := newAttemptFixture(t)
			f.configure(t, attemptProject, "test-capability", 0, false)
			a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "independent-action", channel, 4, 1))
			intent := ""
			if channel == "hosted" {
				intent = "hosted-intent"
			}
			a, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, attemptAnchor(attemptProject, 4), intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
				t.Fatal("zero-dependency caller changed", err)
			}
			attemptCode(t, f.e.VerifyAction(t.Context(), attemptProject, a.ID, a.Revision, SourcePlan{}), "validation_failed")
		})
	}
}
