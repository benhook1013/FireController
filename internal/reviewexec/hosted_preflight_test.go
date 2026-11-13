package reviewexec

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedAllowanceFixture(t *testing.T) (*attemptFixture, Attempt, HostedAllowanceQuery, HostedAllowanceInput) {
	t.Helper()
	f, a, anchor, poster, provider := hostedQueryFixture(t)
	q, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, hostedQueryComment(t, q))
	if err != nil {
		t.Fatal(err)
	}
	checksum := strings.Repeat("c", 64)
	in := HostedAllowanceInput{QueryID: q.ID, Anchor: anchor, ObservedAt: stamp(), ChangedFiles: 2, CaptureID: "review_capture_" + checksum[:32], CaptureSHA256: checksum, CaptureBytes: 1234,
		Observation: reviewprovider.HostedAllowanceObservation{State: "available_observed", Reason: "provider_available_observed", QueryCommentID: q.Comment.ID, ResponseCommentID: 92, QueryCreatedAt: q.Comment.CreatedAt, ResponseCreatedAt: q.Comment.CreatedAt.Add(time.Nanosecond)}}
	return f, a, q, in
}

func hostedAllowanceState(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var body string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(attempt_id||project_id||body_json,''),'') FROM review_hosted_preflight`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	return body + hostedQueryState(t, f, a)
}

func hostedAllowanceRead(t *testing.T, f *attemptFixture, a Attempt) (hostedAllowanceRecord, string) {
	t.Helper()
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_preflight WHERE attempt_id=?`, a.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var record hostedAllowanceRecord
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		t.Fatal(err)
	}
	return record, body
}

func TestHostedAllowanceRecordOriginalBindingReplayAndNoAdmission(t *testing.T) {
	f, a, q, in := hostedAllowanceFixture(t)
	before, receipts := attemptState(t, f, a), attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	record, body := hostedAllowanceRead(t, f, a)
	o := bindingOperation(t, f, f.scope(t, attemptProject, f.writer, false), a.OperationID)
	if record.Input != in || record.ProjectID != attemptProject || record.AttemptID != a.ID || record.OperationID != o.ID || record.ActorID != o.ActorID || record.WorkerID != o.WorkerID || record.ConfigurationRevision != o.ConfigurationRevision || record.PolicyRevision != o.PolicyRevision || record.CapabilityFingerprint != o.CapabilityFingerprint {
		t.Fatal("retained observation lost original source/binding", record)
	}
	if attemptState(t, f, a) != before || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != receipts || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_observations`) != 0 {
		t.Fatal("allowance advanced/admitted/released review or created receipt/final evidence")
	}
	var actor, payload string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT actor_id,payload_json,revision FROM events WHERE event_type='review.hosted_preflight'`).Scan(&actor, &payload, &revision); err != nil || actor != o.ActorID || revision != 1 || len(payload) > 1024 || !strings.Contains(payload, q.ID) || !strings.Contains(payload, in.CaptureID) || strings.Contains(payload, q.Intent) {
		t.Fatal("compact event lost source or leaked command", payload, err)
	}
	// Reopen first, then remove every condition required for a new record. Replay
	// preserves the original observation and does not reauthorize or refresh it.
	cliAllowanceReopen(t, f)
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, 1, "cancelled", "preparation_stopped", ""); err != nil {
		t.Fatal(err)
	}
	admissionPolicy(t, f, 1)
	f.configure(t, attemptProject, "other-account", 1, true)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	state := hostedAllowanceState(t, f, a)
	if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil || hostedAllowanceState(t, f, a) != state {
		t.Fatal("exact replay reapplied current eligibility or refreshed facts", err)
	}
	_, after := hostedAllowanceRead(t, f, a)
	if after != body || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.hosted_preflight'`) != 1 {
		t.Fatal("replay replaced original bytes or repeated event")
	}
	for name, change := range map[string]func(*HostedAllowanceInput){
		"changed files": func(v *HostedAllowanceInput) { v.ChangedFiles++ },
		"capture bytes": func(v *HostedAllowanceInput) { v.CaptureBytes++ },
		"response ID":   func(v *HostedAllowanceInput) { v.Observation.ResponseCommentID++ },
		"source time":   func(v *HostedAllowanceInput) { v.Anchor.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano) },
		"fetch time":    func(v *HostedAllowanceInput) { v.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := in
			change(&changed)
			attemptCode(t, f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, changed), "review_conflict")
			if hostedAllowanceState(t, f, a) != state {
				t.Fatal("changed replay overwrote original facts")
			}
		})
	}
}

func TestHostedAllowanceRejectsInvalidInputWithoutEffects(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*HostedAllowanceInput)
		code   string
	}{
		"query ID":         {func(v *HostedAllowanceInput) { v.QueryID = "review_hosted_query_" + strings.Repeat("f", 32) }, "review_conflict"},
		"query comment":    {func(v *HostedAllowanceInput) { v.Observation.QueryCommentID += 2 }, "review_conflict"},
		"same comment IDs": {func(v *HostedAllowanceInput) { v.Observation.ResponseCommentID = v.Observation.QueryCommentID }, "validation_failed"},
		"query time": {func(v *HostedAllowanceInput) {
			v.Observation.QueryCreatedAt = v.Observation.QueryCreatedAt.Add(-time.Second)
		}, "review_conflict"},
		"source parent":   {func(v *HostedAllowanceInput) { v.Anchor.ParentSHA = strings.Repeat("f", 40) }, "review_conflict"},
		"repository":      {func(v *HostedAllowanceInput) { v.Anchor.RepositoryID++ }, "review_conflict"},
		"candidate":       {func(v *HostedAllowanceInput) { v.Anchor.CandidateSHA = strings.Repeat("f", 40) }, "review_conflict"},
		"empty files":     {func(v *HostedAllowanceInput) { v.ChangedFiles = 0 }, "validation_failed"},
		"excess files":    {func(v *HostedAllowanceInput) { v.ChangedFiles = 100001 }, "validation_failed"},
		"checksum":        {func(v *HostedAllowanceInput) { v.CaptureSHA256 = strings.Repeat("C", 64) }, "validation_failed"},
		"capture ID":      {func(v *HostedAllowanceInput) { v.CaptureID = "review_capture_" + strings.Repeat("f", 32) }, "validation_failed"},
		"empty bytes":     {func(v *HostedAllowanceInput) { v.CaptureBytes = 0 }, "validation_failed"},
		"excess bytes":    {func(v *HostedAllowanceInput) { v.CaptureBytes = (48 << 20) + 1 }, "validation_failed"},
		"cooldown":        {func(v *HostedAllowanceInput) { v.Observation.State = "cooldown_observed" }, "validation_failed"},
		"unproven":        {func(v *HostedAllowanceInput) { v.Observation.State = "unproven" }, "validation_failed"},
		"ambiguous":       {func(v *HostedAllowanceInput) { v.Observation.State = "ambiguous" }, "validation_failed"},
		"unknown":         {func(v *HostedAllowanceInput) { v.Observation.State = "available" }, "validation_failed"},
		"reason":          {func(v *HostedAllowanceInput) { v.Observation.Reason = "allowance_not_proven" }, "validation_failed"},
		"retry time":      {func(v *HostedAllowanceInput) { v.Observation.RetryNotBefore = time.Now().UTC() }, "validation_failed"},
		"missing reply":   {func(v *HostedAllowanceInput) { v.Observation.ResponseCommentID = 0 }, "validation_failed"},
		"unordered reply": {func(v *HostedAllowanceInput) { v.Observation.ResponseCreatedAt = v.Observation.QueryCreatedAt }, "validation_failed"},
		"non UTC reply": {func(v *HostedAllowanceInput) {
			v.Observation.ResponseCreatedAt = v.Observation.ResponseCreatedAt.In(time.FixedZone("offset", 0))
		}, "validation_failed"},
		"stale source": {func(v *HostedAllowanceInput) {
			v.Anchor.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
		}, "validation_failed"},
		"future source": {func(v *HostedAllowanceInput) {
			v.Anchor.ObservedAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
		}, "validation_failed"},
		"stale observed": {func(v *HostedAllowanceInput) {
			v.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
		}, "validation_failed"},
		"future facts": {func(v *HostedAllowanceInput) {
			v.Observation.ResponseCreatedAt = time.Now().Add(time.Minute).UTC()
			v.ObservedAt = v.Observation.ResponseCreatedAt.Format(time.RFC3339Nano)
		}, "validation_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, _, in := hostedAllowanceFixture(t)
			test.change(&in)
			before := hostedAllowanceState(t, f, a)
			attemptCode(t, f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in), test.code)
			if hostedAllowanceState(t, f, a) != before {
				t.Fatal("invalid observation changed durable state")
			}
		})
	}
}

func TestHostedAllowanceNewRecordRequiresCurrentEligibilityAndConfirmedQuery(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*testing.T, *attemptFixture, Attempt, HostedAllowanceQuery)
		code   string
	}{
		"unconfirmed query": {func(t *testing.T, f *attemptFixture, a Attempt, q HostedAllowanceQuery) {
			q.Revision, q.Comment = 1, nil
			admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET revision=1,body_json=? WHERE attempt_id=?`, encoded(q), a.ID)
		}, "review_conflict"},
		"revoked": {func(t *testing.T, f *attemptFixture, _ Attempt, _ HostedAllowanceQuery) {
			admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
		}, "forbidden"},
		"project pause": {func(t *testing.T, f *attemptFixture, _ Attempt, _ HostedAllowanceQuery) {
			f.configure(t, attemptProject, "test-capability", 1, true)
		}, "review_conflict"},
		"configuration pause": {func(t *testing.T, f *attemptFixture, _ Attempt, _ HostedAllowanceQuery) {
			f.configure(t, attemptProject, "test-capability", 1, true)
		}, "review_conflict"},
		"installed capability": {func(t *testing.T, f *attemptFixture, _ Attempt, _ HostedAllowanceQuery) {
			capability := attemptCapabilities[0]
			capability.Fingerprint = strings.Repeat("f", 64)
			var err error
			f.e, err = New(f.store, []Capability{capability})
			if err != nil {
				t.Fatal(err)
			}
		}, "review_conflict"},
		"account": {func(t *testing.T, f *attemptFixture, a Attempt, _ HostedAllowanceQuery) {
			admissionSQL(t, f, `UPDATE review_execution_attempts SET account='different-account' WHERE id=?`, a.ID)
		}, "review_conflict"},
		"channel": {func(t *testing.T, f *attemptFixture, a Attempt, _ HostedAllowanceQuery) {
			admissionSQL(t, f, `UPDATE review_operations SET channel='cli' WHERE id=?`, a.OperationID)
		}, "review_conflict"},
		"worker": {func(t *testing.T, f *attemptFixture, a Attempt, _ HostedAllowanceQuery) {
			admissionSQL(t, f, `UPDATE review_operations SET worker_id='missing-worker' WHERE id=?`, a.OperationID)
		}, "forbidden"},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, q, in := hostedAllowanceFixture(t)
			test.change(t, f, a, q)
			before := hostedAllowanceState(t, f, a)
			attemptCode(t, f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in), test.code)
			if hostedAllowanceState(t, f, a) != before {
				t.Fatal("ineligible record had effects")
			}
		})
	}
	f, a, q, in := hostedAllowanceFixture(t)
	// The history was fetched now, but its actual answer is old. Reconstruct an
	// otherwise valid old confirmed query so rejection exercises answer freshness.
	q.EarliestPosting = time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339Nano)
	q.Comment.CreatedAt = time.Now().Add(-2 * time.Minute).UTC()
	q.Comment.UpdatedAt = q.Comment.CreatedAt
	admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET body_json=? WHERE attempt_id=?`, encoded(q), a.ID)
	in.Observation.QueryCreatedAt = q.Comment.CreatedAt
	in.Observation.ResponseCreatedAt = q.Comment.CreatedAt.Add(time.Second)
	in.ObservedAt = stamp()
	before := hostedAllowanceState(t, f, a)
	attemptCode(t, f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in), "validation_failed")
	if hostedAllowanceState(t, f, a) != before {
		t.Fatal("fresh fetch laundered stale provider answer")
	}
}

func TestHostedAllowanceConcurrencyEventRollbackScopeAndBounds(t *testing.T) {
	f, a, _, in := hostedAllowanceFixture(t)
	before := hostedAllowanceState(t, f, a)
	for _, revision := range []int64{0, 2} {
		attemptCode(t, f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, revision, in), "validation_failed")
	}
	attemptCode(t, f.e.RecordHostedAllowance(t.Context(), otherAttemptProject, a.ID, 1, in), "not_found")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.e.RecordHostedAllowance(ctx, attemptProject, a.ID, 1, in); err == nil {
		t.Fatal("cancelled record succeeded")
	}
	admissionSQL(t, f, `CREATE TRIGGER reject_hosted_preflight BEFORE INSERT ON events WHEN NEW.event_type='review.hosted_preflight' BEGIN SELECT RAISE(ABORT,'private synthetic fault'); END`)
	if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in); err == nil || strings.Contains(err.Error(), "private") || hostedAllowanceState(t, f, a) != before {
		t.Fatal("event fault did not roll back safely", err)
	}
	admissionSQL(t, f, `DROP TRIGGER reject_hosted_preflight`)
	// Inclusive byte/file maxima are metadata bounds; no file is read by Engine.
	in.ChangedFiles, in.CaptureBytes = 100000, 48<<20
	start, results := make(chan struct{}), make(chan error, 6)
	for range 6 {
		go func() {
			<-start
			results <- f.e.RecordHostedAllowance(context.Background(), attemptProject, a.ID, 1, in)
		}()
	}
	close(start)
	for range 6 {
		if err := <-results; err != nil {
			t.Fatal("concurrent identical record failed", err)
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_preflight`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.hosted_preflight'`) != 1 {
		t.Fatal("concurrent record duplicated durable effects")
	}
	record, _ := hostedAllowanceRead(t, f, a)
	if record.Input != in {
		t.Fatal("concurrent calls changed trusted metadata")
	}
}

func TestHostedAllowanceCorruptionFailsClosedWithoutRepair(t *testing.T) {
	for name, damage := range map[string]func(*hostedAllowanceRecord){
		"actor":         func(r *hostedAllowanceRecord) { r.ActorID = "different-actor" },
		"worker":        func(r *hostedAllowanceRecord) { r.WorkerID = "different-worker" },
		"operation":     func(r *hostedAllowanceRecord) { r.OperationID = "review_operation_" + strings.Repeat("f", 32) },
		"configuration": func(r *hostedAllowanceRecord) { r.ConfigurationRevision++ },
		"policy":        func(r *hostedAllowanceRecord) { r.PolicyRevision++ },
		"fingerprint":   func(r *hostedAllowanceRecord) { r.CapabilityFingerprint = strings.Repeat("f", 64) },
		"source":        func(r *hostedAllowanceRecord) { r.Input.Anchor.ParentSHA = strings.Repeat("f", 40) },
		"query":         func(r *hostedAllowanceRecord) { r.Input.QueryID = "review_hosted_query_" + strings.Repeat("f", 32) },
		"query comment": func(r *hostedAllowanceRecord) { r.Input.Observation.QueryCommentID += 2 },
	} {
		t.Run(name, func(t *testing.T) {
			f, a, _, in := hostedAllowanceFixture(t)
			if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil {
				t.Fatal(err)
			}
			record, _ := hostedAllowanceRead(t, f, a)
			damage(&record)
			admissionSQL(t, f, `UPDATE review_hosted_preflight SET body_json=? WHERE attempt_id=?`, encoded(record), a.ID)
			before := hostedAllowanceState(t, f, a)
			attemptCode(t, f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, record.Input), "storage_unavailable")
			if hostedAllowanceState(t, f, a) != before {
				t.Fatal("corrupt original binding was repaired")
			}
		})
	}
	f, a, _, in := hostedAllowanceFixture(t)
	if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	_, body := hostedAllowanceRead(t, f, a)
	for _, corrupted := range []string{body + " ", strings.TrimSuffix(body, "}") + `,"unknown":true}`, strings.Replace(body, `"configuration_revision":1`, `"configuration_revision":1,"Configuration_Revision":1`, 1), strings.Repeat(" ", (16<<10)+1)} {
		admissionSQL(t, f, `UPDATE review_hosted_preflight SET body_json=? WHERE attempt_id=?`, corrupted, a.ID)
		before := hostedAllowanceState(t, f, a)
		attemptCode(t, f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in), "storage_unavailable")
		if hostedAllowanceState(t, f, a) != before {
			t.Fatal("malformed record was repaired")
		}
	}
}

func TestHostedAllowanceProjectFileCeiling(t *testing.T) {
	f := newAttemptFixture(t)
	policy := policyInput(1)
	policy.MaxReviewFiles = 1
	policyConfigure(t, f, f.scope(t, attemptProject, f.admin, true), "hosted-file-ceiling", policy)
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "hosted-ceiling", "hosted", 2, 1))
	anchor := attemptAnchor(attemptProject, 2)
	poster := reviewprovider.Author{ID: 12, Login: "poster", Kind: "User"}
	bot := reviewprovider.Author{ID: 30, Login: "synthetic-provider", Kind: "Bot"}
	q, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, bot)
	if err != nil {
		t.Fatal(err)
	}
	q, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, hostedQueryComment(t, q))
	if err != nil {
		t.Fatal(err)
	}
	checksum := strings.Repeat("c", 64)
	in := HostedAllowanceInput{QueryID: q.ID, Anchor: anchor, ObservedAt: stamp(), ChangedFiles: 2, CaptureID: "review_capture_" + checksum[:32], CaptureSHA256: checksum, CaptureBytes: 1, Observation: reviewprovider.HostedAllowanceObservation{State: "available_observed", Reason: "provider_available_observed", QueryCommentID: q.Comment.ID, ResponseCommentID: 92, QueryCreatedAt: q.Comment.CreatedAt, ResponseCreatedAt: q.Comment.CreatedAt.Add(time.Nanosecond)}}
	before := hostedAllowanceState(t, f, a)
	attemptCode(t, f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in), "review_held")
	if hostedAllowanceState(t, f, a) != before {
		t.Fatal("over-ceiling observation persisted")
	}
	in.ChangedFiles = 1
	if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal("exact file ceiling rejected", err)
	}
	record, _ := hostedAllowanceRead(t, f, a)
	if !reflect.DeepEqual(record.Input, in) {
		t.Fatal("file boundary changed source metadata")
	}
}
