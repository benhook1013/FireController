package reviewexec

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedActionAllowanceFixture(t *testing.T) (*attemptFixture, Attempt, HostedAllowanceQuery, HostedAllowanceInput) {
	t.Helper()
	f, a, q, in := hostedAllowanceFixture(t)
	// The older generic admission fixture used an arbitrary parent SHA. Final
	// source witnesses must model a realizable comparison: effective parent=base.
	q.Anchor.ParentSHA = q.Anchor.BaseSHA
	in.Anchor = q.Anchor
	admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET body_json=? WHERE attempt_id=?`, encoded(q), a.ID)
	return f, a, q, in
}

func hostedActionFixture(t *testing.T) (*attemptFixture, Attempt, HostedAllowanceInput) {
	t.Helper()
	f, a, _, in := hostedActionAllowanceFixture(t)
	if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil {
		t.Fatal(err)
	}
	intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: in.Anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	admitted := in.Anchor
	admitted.Repository = "renamed/repository"
	admitted.ObservedAt = stamp()
	a, err = f.e.Admit(t.Context(), attemptProject, a.ID, 1, admitted, intent)
	if err != nil {
		t.Fatal(err)
	}
	return f, a, in
}

func hostedActionSource(a Attempt) SourcePlan {
	anchor := *a.Anchor
	return SourcePlan{Repository: reviewprovider.Repository{ID: anchor.RepositoryID, FullName: anchor.Repository, DefaultBranch: anchor.BaseRef}, Pull: reviewprovider.Pull{Number: anchor.PullNumber, RepositoryID: anchor.RepositoryID, State: "open", HeadSHA: anchor.CandidateSHA, BaseSHA: anchor.BaseSHA, BaseRef: anchor.BaseRef, ChangedFiles: 1}, ParentSHA: anchor.ParentSHA, ParentPullNumber: anchor.ParentPullNumber, ObservedAt: stamp()}
}

func TestHostedAllowanceFinalGuardAndReadReplayAfterSettlement(t *testing.T) {
	f, a, in := hostedActionFixture(t)
	before := hostedAllowanceState(t, f, a)
	if err := f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, hostedActionSource(a)); err != nil || hostedAllowanceState(t, f, a) != before {
		t.Fatal("fresh exact source guard failed or wrote state", err)
	}
	read, err := f.e.ReadHostedAllowance(t.Context(), attemptProject, a.ID)
	if err != nil || read != in || hostedAllowanceState(t, f, a) != before {
		t.Fatal("read refreshed original source or changed state", read, err)
	}
	read.Anchor.Repository = "caller/changed"
	read.Observation.State = "caller changed"
	if again, err := f.e.ReadHostedAllowance(t.Context(), attemptProject, a.ID); err != nil || again != in {
		t.Fatal("read aliased retained values", again, err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "synthetic_posted", "123")
	if err != nil {
		t.Fatal(err)
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "completed", "synthetic_completed", "")
	if err != nil {
		t.Fatal(err)
	}
	admissionPolicy(t, f, 1)
	f.configure(t, attemptProject, "other-account", 1, true)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	before = hostedAllowanceState(t, f, a)
	if got, err := f.e.ReadHostedAllowance(t.Context(), attemptProject, a.ID); err != nil || got != in {
		t.Fatal("settled read reauthorized or changed original facts", got, err)
	}
	if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil || hostedAllowanceState(t, f, a) != before {
		t.Fatal("settled exact replay changed retained facts", err)
	}
}

func TestHostedAllowanceFinalGuardRequiresExactUnpostedReviewIntent(t *testing.T) {
	for _, change := range []string{"arbitrary command", "markerless full review", "altered head marker", "altered attempt marker", "already recorded external ID"} {
		t.Run(change, func(t *testing.T) {
			f, a, _, in := hostedActionAllowanceFixture(t)
			if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, in); err != nil {
				t.Fatal(err)
			}
			intent, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: in.Anchor.CandidateSHA})
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "arbitrary command":
				intent = "@coderabbitai resume"
			case "markerless full review":
				intent = "@coderabbitai full review"
			case "altered head marker":
				intent = strings.Replace(intent, in.Anchor.CandidateSHA, strings.Repeat("f", 40), 1)
			case "altered attempt marker":
				intent = strings.Replace(intent, a.ID, "review_attempt_"+strings.Repeat("f", 32), 1)
			}
			// The trusted admission API retains nonempty intent; the final hosted
			// guard must enforce the fixed command before permitting its POST.
			a, err = f.e.Admit(t.Context(), attemptProject, a.ID, 1, in.Anchor, intent)
			if err != nil {
				t.Fatal(err)
			}
			if change == "already recorded external ID" {
				// No public transition keeps state posting while adding a receipt.
				// Model that retained condition directly in this isolated fixture.
				admissionSQL(t, f, `UPDATE review_execution_attempts SET external_id='123' WHERE id=?`, a.ID)
			}
			before := hostedAllowanceState(t, f, a)
			attemptCode(t, f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, hostedActionSource(a)), "review_conflict")
			if hostedAllowanceState(t, f, a) != before {
				t.Fatal("invalid intent guard changed retained source or attempt")
			}
		})
	}
}

func TestHostedAllowanceFinalGuardRejectsInvalidCurrentBinding(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*testing.T, *attemptFixture, *Attempt)
		code   string
	}{
		"missing allowance": {func(t *testing.T, f *attemptFixture, a *Attempt) {
			admissionSQL(t, f, `DELETE FROM review_hosted_preflight WHERE attempt_id=?`, a.ID)
		}, "not_found"},
		"corrupt allowance": {func(t *testing.T, f *attemptFixture, a *Attempt) {
			admissionSQL(t, f, `UPDATE review_hosted_preflight SET body_json='invalid private capture' WHERE attempt_id=?`, a.ID)
		}, "storage_unavailable"},
		"channel": {func(t *testing.T, f *attemptFixture, a *Attempt) {
			admissionSQL(t, f, `UPDATE review_operations SET channel='cli' WHERE id=?`, a.OperationID)
		}, "review_conflict"},
		"source": {func(t *testing.T, f *attemptFixture, a *Attempt) {
			anchor := *a.Anchor
			anchor.ParentSHA = strings.Repeat("f", 40)
			admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, encoded(anchor), a.ID)
		}, "review_conflict"},
		"revision": {func(_ *testing.T, _ *attemptFixture, a *Attempt) { a.Revision++ }, "review_conflict"},
		"reservation": {func(t *testing.T, f *attemptFixture, a *Attempt) {
			admissionSQL(t, f, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, a.ID)
		}, "review_conflict"},
		"policy changed": {func(t *testing.T, f *attemptFixture, _ *Attempt) { admissionPolicy(t, f, 1) }, "review_conflict"},
		"project paused": {func(t *testing.T, f *attemptFixture, _ *Attempt) {
			f.configure(t, attemptProject, "test-capability", 1, true)
		}, "review_conflict"},
		"authorization": {func(t *testing.T, f *attemptFixture, _ *Attempt) {
			admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
		}, "forbidden"},
		"pending completed evidence": {func(t *testing.T, f *attemptFixture, a *Attempt) { coverageSeedOperations(t, f, *a, 1, "completed") }, "review_evidence_pending"},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, _ := hostedActionFixture(t)
			test.change(t, f, &a)
			before := hostedAllowanceState(t, f, a)
			attemptCode(t, f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, hostedActionSource(a)), test.code)
			if hostedAllowanceState(t, f, a) != before {
				t.Fatal("failed final guard changed state")
			}
		})
	}
}

func TestHostedAllowanceStaleGuardCannotRefreshRetainedRead(t *testing.T) {
	for _, stale := range []string{"answer", "fetch"} {
		t.Run(stale, func(t *testing.T) {
			f, a, _ := hostedActionFixture(t)
			record, _ := hostedAllowanceRead(t, f, a)
			old := time.Now().Add(-2 * time.Minute).UTC()
			switch stale {
			case "answer", "fetch":
				q, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
				if err != nil {
					t.Fatal(err)
				}
				q.EarliestPosting = old.Add(-time.Minute).Format(time.RFC3339Nano)
				q.Comment.CreatedAt, q.Comment.UpdatedAt = old.Add(-time.Second), old.Add(-time.Second)
				admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET body_json=? WHERE attempt_id=?`, encoded(q), a.ID)
				record.Input.Observation.QueryCreatedAt = q.Comment.CreatedAt
				record.Input.Observation.ResponseCreatedAt = old
				if stale == "fetch" {
					record.Input.ObservedAt = old.Add(time.Second).Format(time.RFC3339Nano)
				}
			}
			admissionSQL(t, f, `UPDATE review_hosted_preflight SET body_json=? WHERE attempt_id=?`, encoded(record), a.ID)
			before := hostedAllowanceState(t, f, a)
			for range 2 {
				attemptCode(t, f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, hostedActionSource(a)), "review_held")
				read, err := f.e.ReadHostedAllowance(t.Context(), attemptProject, a.ID)
				if err != nil || read != record.Input || hostedAllowanceState(t, f, a) != before {
					t.Fatal("stale guard/read updated source or observation time", read, err)
				}
			}
			// Exact immutable replay remains possible even for stale facts. It is
			// not a way to make those facts pass the next final action guard.
			if err := f.e.RecordHostedAllowance(t.Context(), attemptProject, a.ID, 1, record.Input); err != nil || hostedAllowanceState(t, f, a) != before {
				t.Fatal("stale exact replay failed or refreshed facts", err)
			}
			var input HostedAllowanceInput
			if json.Unmarshal([]byte(encoded(record.Input)), &input) != nil || input != record.Input {
				t.Fatal("owned read lost canonical source metadata")
			}
		})
	}
}

func TestHostedActionRequiresFreshExactSourceWitness(t *testing.T) {
	for name, test := range map[string]struct {
		change       func(*SourcePlan)
		code, reason string
	}{
		"missing":         {func(p *SourcePlan) { *p = SourcePlan{} }, "validation_failed", ""},
		"malformed time":  {func(p *SourcePlan) { p.ObservedAt = "bad" }, "validation_failed", ""},
		"closed":          {func(p *SourcePlan) { p.Pull.State = "closed" }, "validation_failed", ""},
		"invalid parent":  {func(p *SourcePlan) { p.ParentSHA = strings.Repeat("f", 40) }, "validation_failed", ""},
		"stale":           {func(p *SourcePlan) { p.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano) }, "review_held", "source_stale"},
		"future":          {func(p *SourcePlan) { p.ObservedAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano) }, "review_held", "source_stale"},
		"repository ID":   {func(p *SourcePlan) { p.Repository.ID++; p.Pull.RepositoryID = p.Repository.ID }, "review_held", "source_changed"},
		"repository name": {func(p *SourcePlan) { p.Repository.FullName = "other/repository" }, "review_held", "source_changed"},
		"pull":            {func(p *SourcePlan) { p.Pull.Number++ }, "review_held", "source_changed"},
		"head":            {func(p *SourcePlan) { p.Pull.HeadSHA = strings.Repeat("f", 40) }, "review_held", "source_changed"},
		"base":            {func(p *SourcePlan) { p.Pull.BaseSHA = strings.Repeat("f", 40); p.ParentSHA = p.Pull.BaseSHA }, "review_held", "source_changed"},
		"base ref":        {func(p *SourcePlan) { p.Pull.BaseRef = "other"; p.Repository.DefaultBranch = "other" }, "review_held", "source_changed"},
		"parent number":   {func(p *SourcePlan) { p.ParentPullNumber = 10 }, "review_held", "source_changed"},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, _ := hostedActionFixture(t)
			source := hostedActionSource(a)
			test.change(&source)
			before := hostedAllowanceState(t, f, a)
			err := f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, source)
			attemptCode(t, err, test.code)
			if test.reason != "" {
				var failure *core.Error
				if !errors.As(err, &failure) || failure.Details.(map[string]string)["reason"] != test.reason {
					t.Fatal("unstable hold", err)
				}
			}
			if hostedAllowanceState(t, f, a) != before {
				t.Fatal("invalid current witness changed retained facts")
			}
		})
	}
}

func TestHostedActionFreshWitnessPreservesHistoricalSourceTimes(t *testing.T) {
	f, a, _ := hostedActionFixture(t)
	source := hostedActionSource(a)
	record, _ := hostedAllowanceRead(t, f, a)
	old := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	record.Input.Anchor.ObservedAt = old
	admitted := *a.Anchor
	admitted.ObservedAt = old
	admissionSQL(t, f, `UPDATE review_execution_attempts SET anchor_json=? WHERE id=?`, encoded(admitted), a.ID)
	admissionSQL(t, f, `UPDATE review_hosted_preflight SET body_json=? WHERE attempt_id=?`, encoded(record), a.ID)
	before := hostedAllowanceState(t, f, a)
	for range 2 {
		if err := f.e.VerifyHostedAction(t.Context(), attemptProject, a.ID, a.Revision, source); err != nil {
			t.Fatal("fresh exact witness rejected for historical timestamp", err)
		}
	}
	read, err := f.e.ReadHostedAllowance(t.Context(), attemptProject, a.ID)
	if err != nil || read != record.Input || hostedAllowanceState(t, f, a) != before {
		t.Fatal("current source check refreshed historical facts", err)
	}
}
