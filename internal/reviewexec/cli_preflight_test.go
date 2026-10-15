package reviewexec

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func cliAllowanceFixture(t *testing.T) (*attemptFixture, Attempt, CLIAllowanceObservation) {
	t.Helper()
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "allowance-request", "cli", 2, 1))
	input := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 3, Limit: 5, FullCapacityInMS: 12000, RollingWindowMS: 3600000, ChangedFiles: 2}
	return f, a, input
}

func cliAllowanceState(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var rows string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(attempt_id||project_id||body_json,''),'') FROM review_cli_preflight`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return attemptState(t, f, a) + "/" + rows + "/" + policyState(t, f)
}

func cliAllowanceRead(t *testing.T, f *attemptFixture, a Attempt) (cliAllowanceRecord, string) {
	t.Helper()
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_cli_preflight WHERE attempt_id=?`, a.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var record cliAllowanceRecord
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		t.Fatal(err)
	}
	return record, body
}

func cliAllowanceReopen(t *testing.T, f *attemptFixture) {
	t.Helper()
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
}

func TestCLIAllowanceVersionThreeMigrationRetainsFactsAndReplayAcrossRestart(t *testing.T) {
	f, a, input := cliAllowanceFixture(t)
	journal := attemptState(t, f, a)
	admissionSQL(t, f, `DROP TABLE review_cli_preflight`)
	admissionSQL(t, f, `DROP TABLE review_hosted_preflight`)
	admissionSQL(t, f, `DROP TABLE review_hosted_allowance_queries`)
	admissionSQL(t, f, `DROP INDEX review_operations_completed_comparison`)
	admissionSQL(t, f, `UPDATE metadata SET value='3' WHERE key='schema:review_execution'`)
	cliAllowanceReopen(t, f)
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil || version != "6" || attemptState(t, f, a) != journal {
		t.Fatal("migration changed retained journal", version, err)
	}
	receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input); err != nil {
		t.Fatal(err)
	}
	record, body := cliAllowanceRead(t, f, a)
	operation := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), a.OperationID)
	if record.Observation != input || record.ProjectID != attemptProject || record.AttemptID != a.ID || record.AttemptRevision != a.Revision || record.OperationID != a.OperationID || record.ActorID != operation.ActorID || record.WorkerID != operation.WorkerID || record.ConfigurationRevision != operation.ConfigurationRevision || record.PolicyRevision != operation.PolicyRevision || record.CapabilityFingerprint != operation.CapabilityFingerprint || record.Provider != a.Provider || record.Account != a.Account {
		t.Fatal("allowance lost observed facts or original binding", record)
	}
	if attemptState(t, f, a) != journal || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != receipts {
		t.Fatal("allowance record admitted/settled attempt or manufactured receipt")
	}
	var actor, payload string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT actor_id,payload_json,revision FROM events WHERE event_type='review.cli_preflight' AND resource_id=?`, a.ID).Scan(&actor, &payload, &revision); err != nil || actor != operation.ActorID || revision != 1 || len(payload) > 1024 || !strings.Contains(payload, operation.ID) || !strings.Contains(payload, input.Anchor.CandidateSHA) {
		t.Fatal("compact allowance event lost original identity", payload, err)
	}
	for _, private := range []string{f.writer, f.admin, "bootstrap-admin.token", "executable", "environment"} {
		if strings.Contains(body, private) || strings.Contains(payload, private) {
			t.Fatal("allowance included private runtime configuration")
		}
	}
	before := cliAllowanceState(t, f, a)
	cliAllowanceReopen(t, f)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input); err != nil || cliAllowanceState(t, f, a) != before || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.cli_preflight'`) != 1 {
		t.Fatal("restart/replay replaced observation or repeated event", err)
	}
}

func TestCLIAllowanceReplayAfterSettlementAccessAndPolicyChanges(t *testing.T) {
	f, a, input := cliAllowanceFixture(t)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Admit(t.Context(), attemptProject, a.ID, a.Revision, input.Anchor, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision+1, "completed", "retained", ""); err != nil {
		t.Fatal(err)
	}
	admissionPolicy(t, f, 1, "cli")
	f.configure(t, attemptProject, "other-account", 1, true)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	before := cliAllowanceState(t, f, a)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input); err != nil || cliAllowanceState(t, f, a) != before {
		t.Fatal("exact replay reauthorized or changed retained observation", err)
	}
	for name, change := range map[string]func(*CLIAllowanceObservation){
		"allowance-time": func(in *CLIAllowanceObservation) {
			in.ObservedAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
		},
		"source-time": func(in *CLIAllowanceObservation) {
			in.Anchor.ObservedAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
		},
		"comparison":     func(in *CLIAllowanceObservation) { in.Anchor.PatchSHA256 = strings.Repeat("f", 64) },
		"remaining":      func(in *CLIAllowanceObservation) { in.Remaining-- },
		"limit":          func(in *CLIAllowanceObservation) { in.Limit++ },
		"full-capacity":  func(in *CLIAllowanceObservation) { in.FullCapacityInMS++ },
		"rolling-window": func(in *CLIAllowanceObservation) { in.RollingWindowMS++ },
		"changed-files":  func(in *CLIAllowanceObservation) { in.ChangedFiles++ },
	} {
		t.Run(name, func(t *testing.T) {
			changed := input
			change(&changed)
			attemptCode(t, f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, changed), "review_conflict")
			if cliAllowanceState(t, f, a) != before {
				t.Fatal("changed replay overwrote immutable facts")
			}
		})
	}
	attemptCode(t, f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision+1, input), "review_conflict")
	// Reconstruct an already-aged observation in this owned fixture without a
	// minute-long clock wait. Freshness must not prevent exact durable replay.
	record, _ := cliAllowanceRead(t, f, a)
	record.Observation.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	record.Observation.Anchor.ObservedAt = record.Observation.ObservedAt
	admissionSQL(t, f, `UPDATE review_cli_preflight SET body_json=? WHERE attempt_id=?`, encoded(record), a.ID)
	before = cliAllowanceState(t, f, a)
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, record.Observation); err != nil || cliAllowanceState(t, f, a) != before {
		t.Fatal("old exact replay re-applied admission freshness", err)
	}
}

func TestCLIAllowanceRejectsInvalidFactsAndIdentityWithoutEffects(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*CLIAllowanceObservation)
		code   string
	}{
		"remaining-empty":  {func(in *CLIAllowanceObservation) { in.Remaining = 0 }, "validation_failed"},
		"remaining-excess": {func(in *CLIAllowanceObservation) { in.Remaining = in.Limit + 1 }, "validation_failed"},
		"limit-empty":      {func(in *CLIAllowanceObservation) { in.Limit = 0 }, "validation_failed"},
		"limit-large":      {func(in *CLIAllowanceObservation) { in.Limit = 10001 }, "validation_failed"},
		"full-negative":    {func(in *CLIAllowanceObservation) { in.FullCapacityInMS = -1 }, "validation_failed"},
		"full-large":       {func(in *CLIAllowanceObservation) { in.FullCapacityInMS = 86400001 }, "validation_failed"},
		"rolling-empty":    {func(in *CLIAllowanceObservation) { in.RollingWindowMS = 0 }, "validation_failed"},
		"rolling-large":    {func(in *CLIAllowanceObservation) { in.RollingWindowMS = 86400001 }, "validation_failed"},
		"files-empty":      {func(in *CLIAllowanceObservation) { in.ChangedFiles = 0 }, "validation_failed"},
		"files-large":      {func(in *CLIAllowanceObservation) { in.ChangedFiles = 100001 }, "validation_failed"},
		"malformed-time":   {func(in *CLIAllowanceObservation) { in.ObservedAt = "invalid" }, "validation_failed"},
		"noncanonical-time": {func(in *CLIAllowanceObservation) {
			in.ObservedAt = time.Now().Format("2006-01-02T15:04:05.999999999-07:00")
		}, "validation_failed"},
		"old-allowance": {func(in *CLIAllowanceObservation) {
			in.ObservedAt = time.Now().Add(-61 * time.Second).UTC().Format(time.RFC3339Nano)
		}, "validation_failed"},
		"future-allowance": {func(in *CLIAllowanceObservation) {
			in.ObservedAt = time.Now().Add(6 * time.Second).UTC().Format(time.RFC3339Nano)
		}, "validation_failed"},
		"old-source": {func(in *CLIAllowanceObservation) {
			in.Anchor.ObservedAt = time.Now().Add(-61 * time.Second).UTC().Format(time.RFC3339Nano)
		}, "validation_failed"},
		"future-source": {func(in *CLIAllowanceObservation) {
			in.Anchor.ObservedAt = time.Now().Add(6 * time.Second).UTC().Format(time.RFC3339Nano)
		}, "validation_failed"},
		"malformed-source": {func(in *CLIAllowanceObservation) { in.Anchor.PatchSHA256 = "unknown" }, "validation_failed"},
		"wrong-repository": {func(in *CLIAllowanceObservation) { in.Anchor.RepositoryID++ }, "review_conflict"},
		"wrong-pull":       {func(in *CLIAllowanceObservation) { in.Anchor.PullNumber++ }, "review_conflict"},
		"wrong-head":       {func(in *CLIAllowanceObservation) { in.Anchor.CandidateSHA = strings.Repeat("9", 40) }, "review_conflict"},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, input := cliAllowanceFixture(t)
			test.change(&input)
			before := cliAllowanceState(t, f, a)
			attemptCode(t, f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input), test.code)
			if cliAllowanceState(t, f, a) != before {
				t.Fatal("invalid observation changed durable state")
			}
		})
	}
}

func TestCLIAllowanceRechecksCurrentAdmissionBeforeFirstRecord(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*testing.T, *attemptFixture, Attempt)
		code   string
	}{
		"grant": {func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE grants SET can_write=0 WHERE credential_id='exec-writer-cred'`)
		}, "forbidden"},
		"worker": {func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_operations SET worker_id='absent-worker' WHERE id=?`, a.OperationID)
		}, "forbidden"},
		"channel": {func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_operations SET channel='hosted' WHERE id=?`, a.OperationID)
		}, "review_conflict"},
		"policy":        {func(t *testing.T, f *attemptFixture, a Attempt) { admissionPolicy(t, f, 1) }, "review_conflict"},
		"paused-policy": {func(t *testing.T, f *attemptFixture, a Attempt) { admissionPolicy(t, f, 1, "cli") }, "review_conflict"},
		"configuration": {func(t *testing.T, f *attemptFixture, a Attempt) {
			f.configure(t, attemptProject, "test-capability", 1, false)
		}, "review_conflict"},
		"revocation": {func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
		}, "forbidden"},
		"capability": {func(t *testing.T, f *attemptFixture, a Attempt) { bindEngine(t, f, bindingCapability()) }, "review_conflict"},
		"account": {func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET account='other-account' WHERE id=?`, a.ID)
		}, "review_conflict"},
		"provider": {func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET provider='other-provider' WHERE id=?`, a.ID)
		}, "review_conflict"},
		"operation": {func(t *testing.T, f *attemptFixture, a Attempt) {
			admissionSQL(t, f, `UPDATE review_operations SET attempt_id='other-attempt' WHERE id=?`, a.OperationID)
		}, "review_conflict"},
		"settled": {func(t *testing.T, f *attemptFixture, a Attempt) {
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "held", "source_hold", ""); err != nil {
				t.Fatal(err)
			}
		}, "review_conflict"},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, input := cliAllowanceFixture(t)
			test.change(t, f, a)
			before := cliAllowanceState(t, f, a)
			attemptCode(t, f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input), test.code)
			if cliAllowanceState(t, f, a) != before {
				t.Fatal("changed admission created allowance record")
			}
		})
	}
	t.Run("file-ceiling", func(t *testing.T) {
		f := newAttemptFixture(t)
		policy := policyInput(1)
		policy.MaxReviewFiles = 1
		policyConfigure(t, f, f.scope(t, attemptProject, f.admin, true), "file-ceiling", policy)
		f.configure(t, attemptProject, "test-capability", 0, false)
		a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "large-patch", "cli", 2, 1))
		input := CLIAllowanceObservation{Anchor: attemptAnchor(attemptProject, 2), ObservedAt: stamp(), Remaining: 1, Limit: 1, RollingWindowMS: 1, ChangedFiles: 2}
		before := cliAllowanceState(t, f, a)
		attemptCode(t, f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input), "review_held")
		if cliAllowanceState(t, f, a) != before {
			t.Fatal("over-ceiling observation produced effects")
		}
		input.ChangedFiles = 1
		if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input); err != nil {
			t.Fatal("exact ceiling/small positive allowance rejected", err)
		}
	})
}

func TestCLIAllowanceConcurrentReplayAndAtomicEventFailure(t *testing.T) {
	f, a, input := cliAllowanceFixture(t)
	admissionSQL(t, f, `CREATE TRIGGER reject_cli_preflight BEFORE INSERT ON events WHEN NEW.event_type='review.cli_preflight' BEGIN SELECT RAISE(ABORT,'synthetic private diagnostic'); END`)
	before := cliAllowanceState(t, f, a)
	err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input)
	if err == nil || strings.Contains(err.Error(), "synthetic") || cliAllowanceState(t, f, a) != before {
		t.Fatal("event failure did not safely roll back observation", err)
	}
	admissionSQL(t, f, `DROP TRIGGER reject_cli_preflight`)
	start, results := make(chan struct{}), make(chan error, 6)
	for i := 0; i < 6; i++ {
		go func() {
			<-start
			results <- f.e.RecordCLIAllowance(context.Background(), attemptProject, a.ID, a.Revision, input)
		}()
	}
	close(start)
	for i := 0; i < 6; i++ {
		if err := <-results; err != nil {
			t.Fatal("concurrent exact observation failed", err)
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_cli_preflight`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.cli_preflight'`) != 1 {
		t.Fatal("concurrent replay repeated observation/event")
	}
	record, _ := cliAllowanceRead(t, f, a)
	if !reflect.DeepEqual(record.Observation, input) {
		t.Fatal("concurrent replay replaced facts")
	}
}

func TestCLIAllowanceScopeRevisionAndUpperBounds(t *testing.T) {
	f, a, input := cliAllowanceFixture(t)
	before := cliAllowanceState(t, f, a)
	attemptCode(t, f.e.RecordCLIAllowance(t.Context(), otherAttemptProject, a.ID, a.Revision, input), "not_found")
	attemptCode(t, f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision+1, input), "review_conflict")
	attemptCode(t, f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, 0, input), "validation_failed")
	attemptCode(t, f.e.RecordCLIAllowance(t.Context(), "invalid/project", a.ID, a.Revision, input), "validation_failed")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.e.RecordCLIAllowance(ctx, attemptProject, a.ID, a.Revision, input); err == nil {
		t.Fatal("cancelled observation succeeded")
	}
	if cliAllowanceState(t, f, a) != before {
		t.Fatal("invalid scope/revision or cancellation changed facts")
	}
	// Unlimited project policy is independent of provider allowance, and each
	// documented inclusive maximum remains retainable without overflow.
	input.Remaining, input.Limit = 10000, 10000
	input.FullCapacityInMS, input.RollingWindowMS, input.ChangedFiles = 86400000, 86400000, 100000
	if err := f.e.RecordCLIAllowance(t.Context(), attemptProject, a.ID, a.Revision, input); err != nil {
		t.Fatal("valid upper bounds rejected", err)
	}
}
