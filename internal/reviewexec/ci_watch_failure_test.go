package reviewexec

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func ciWatchMixedFailure(target reviewprovider.CITarget, gate string) reviewprovider.CIObservation {
	o := sourceCITestObservation(target, "failed")
	o.Authority.Branch = gate
	o.Authority.Checks[0].Context = "Smoke Gate"
	o.Results.Statuses[0].Context = "Smoke Gate"
	for i, name := range []string{"Compile", "Unit", "Lint"} {
		o.Authority.Checks = append(o.Authority.Checks, reviewprovider.RequiredCheck{Context: name})
		o.Results.Statuses = append(o.Results.Statuses, reviewprovider.CICommitStatus{ID: int64(992 + i), Context: name, State: "success", CreatedAt: o.ObservedAt, UpdatedAt: o.ObservedAt})
	}
	o.Authority.Checks = append(o.Authority.Checks, reviewprovider.RequiredCheck{Context: "Validation"})
	o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
	o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
	return o
}

type ciWatchMixedFailureReader struct {
	*ciWatchReader
	nonstrict bool
}

func (r ciWatchMixedFailureReader) ObserveCIGate(ctx context.Context, target reviewprovider.CITarget, gate string) (reviewprovider.CIObservation, error) {
	if target.PullNumber == 43 {
		o := ciWatchMixedFailure(target, gate)
		if r.nonstrict {
			mismatch := sourceCINonstrictMismatchObservation(target)
			o.Candidate, o.MergeState = mismatch.Candidate, mismatch.MergeState
		}
		return o, nil
	}
	return r.ciWatchReader.ObserveCIGate(ctx, target, gate)
}

func TestCIWatchProvenFailureWakesWithMissingRequiredCheck(t *testing.T) {
	f, schedule, _, reader, scope := ciWatchFixture(t)
	schedule.readers["test-capability"] = ciWatchMixedFailureReader{ciWatchReader: reader}
	before := lifecycleHintCounts(t, f)
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	targets := []CIWatchTarget{{PullNumber: 42, HeadSHA: head, BaseSHA: base}, {PullNumber: 43, HeadSHA: head, BaseSHA: base}}
	out, cancel := startCIWatch(t, f, scope, targets)
	defer cancel()
	ciWatchRegistered(t, schedule, 1)
	watchTick(t, f, schedule)
	select {
	case reply := <-out:
		t.Fatal("missing-only required check ended the wait", reply)
	default:
	}
	watchTick(t, f, schedule)
	reply := receiveCIWatch(t, out)
	if reply.err != nil || reply.value.State != "failed" || reply.value.TimedOut || len(reply.value.Targets) != 2 || reply.value.Targets[0].State != "pending" || reply.value.Targets[0].Unproven != 1 {
		t.Fatal("batch did not wake on the proven failure", reply)
	}
	failed := reply.value.Targets[1]
	if failed.State != "failed" || failed.Reason != "ci_required_checks_failed" || !reflect.DeepEqual(failed.CIWatchTarget, targets[1]) || failed.Required != 5 || failed.Satisfied != 3 || failed.Failed != 1 || failed.Unproven != 1 || failed.Pending != 0 || len(failed.Checks) != 5 || failed.Checks[0].Context != "Smoke Gate" || failed.Checks[0].State != "failed" || failed.Checks[4].Context != "Validation" || failed.Checks[4].State != "unproven" {
		t.Fatal("early failure lost the original pins or partial check facts", failed)
	}
	ciWatchRegistered(t, schedule, 0)
	if !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("read-only failure watch changed durable review state")
	}
}

func TestCIWatchNonstrictHeadFailureWakesWithMissingRequiredCheck(t *testing.T) {
	f, schedule, _, reader, scope := ciWatchFixture(t)
	schedule.readers["test-capability"] = ciWatchMixedFailureReader{ciWatchReader: reader, nonstrict: true}
	before := lifecycleHintCounts(t, f)
	target := CIWatchTarget{PullNumber: 43, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}
	out, cancel := startCIWatch(t, f, scope, []CIWatchTarget{target})
	defer cancel()
	ciWatchRegistered(t, schedule, 1)
	watchTick(t, f, schedule)
	reply := receiveCIWatch(t, out)
	if reply.err != nil || reply.value.State != "failed" || reply.value.TimedOut || len(reply.value.Targets) != 1 {
		t.Fatal("non-strict head failure did not end the wait", reply)
	}
	failed := reply.value.Targets[0]
	if failed.State != "failed" || failed.Reason != "ci_required_checks_failed" || !reflect.DeepEqual(failed.CIWatchTarget, target) || failed.Selection != "head" || failed.CICommitSHA != target.HeadSHA || failed.GateRef != "develop" || failed.Required != 5 || failed.Satisfied != 3 || failed.Failed != 1 || failed.Unproven != 1 || failed.Pending != 0 || len(failed.Checks) != 5 || failed.Checks[0].State != "failed" || failed.Checks[4].State != "unproven" || failed.ObservationKey == "" {
		t.Fatal("non-strict failure lost selected head or mixed check facts", failed)
	}
	ciWatchRegistered(t, schedule, 0)
	if !reflect.DeepEqual(before, lifecycleHintCounts(t, f)) {
		t.Fatal("read-only non-strict failure watch changed durable review state")
	}
}

func TestCIWatchNonstrictMixedFailureRequiresApplicableTrustedFacts(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 43, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "stack/parent"}
	for _, test := range []struct {
		name   string
		mutate func(*reviewprovider.CIObservation)
		want   bool
	}{
		{"proven nonstrict head failure", func(*reviewprovider.CIObservation) {}, true},
		{"strict authority", func(o *reviewprovider.CIObservation) { o.Authority.Strict = true }, false},
		{"unproven authority", func(o *reviewprovider.CIObservation) { o.Authority.State = "unproven" }, false},
		{"unknown candidate", func(o *reviewprovider.CIObservation) { o.Candidate.Reason = "ci_merge_identity_unproven" }, false},
		{"missing candidate", func(o *reviewprovider.CIObservation) { o.Candidate = nil }, false},
		{"foreign candidate", func(o *reviewprovider.CIObservation) { o.Candidate.RepositoryID++ }, false},
		{"foreign repository name", func(o *reviewprovider.CIObservation) { o.Candidate.Repository = "other/repo" }, false},
		{"different pull", func(o *reviewprovider.CIObservation) { o.Candidate.PullNumber++ }, false},
		{"stale head", func(o *reviewprovider.CIObservation) { o.Candidate.HeadSHA = strings.Repeat("c", 40) }, false},
		{"stale base", func(o *reviewprovider.CIObservation) { o.Candidate.BaseSHA = strings.Repeat("c", 40) }, false},
		{"changed base ref", func(o *reviewprovider.CIObservation) { o.Candidate.BaseRef = "different" }, false},
		{"missing mismatch", func(o *reviewprovider.CIObservation) { o.Candidate.MergeBaseMismatch = nil }, false},
		{"unproven live mismatch", func(o *reviewprovider.CIObservation) { o.Candidate.MergeBaseMismatch.LiveBaseSHA = target.BaseSHA }, false},
		{"unbound merge parent", func(o *reviewprovider.CIObservation) {
			o.Candidate.MergeBaseMismatch.MergeBaseParentSHA = strings.Repeat("c", 40)
		}, false},
		{"claims verified merge", func(o *reviewprovider.CIObservation) { o.MergeState = "verified" }, false},
		{"selects stale merge", func(o *reviewprovider.CIObservation) {
			o.Selection, o.SelectedCommitSHA = "test_merge", o.Candidate.MergeBaseMismatch.MergeSHA
		}, false},
		{"unexpected merge inventory", func(o *reviewprovider.CIObservation) {
			v := *o.Results
			o.TestMergeResults = &v
		}, false},
		{"coherence downgrade", func(o *reviewprovider.CIObservation) { o.Reason = "ci_observation_changed" }, false},
		{"forged retained failure", func(o *reviewprovider.CIObservation) { o.Results.Statuses = o.Results.Statuses[1:] }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := ciWatchMixedFailure(target, "develop")
			mismatch := sourceCINonstrictMismatchObservation(target)
			o.Candidate, o.MergeState = mismatch.Candidate, mismatch.MergeState
			source := SourceObservation{RepositoryID: target.RepositoryID, RepositoryName: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, ParentSHA: target.BaseSHA, ObservedAt: o.StartedAt.Add(-time.Millisecond).Format(time.RFC3339Nano)}
			if !validCIWithAuthorityBranch(o, source, "develop", false) {
				t.Fatal("positive non-strict mixed failure fixture rejected")
			}
			test.mutate(&o)
			before := encoded(o)
			got := validCIWithAuthorityBranch(o, source, "develop", false) && ciWatchRequiredFailure(o)
			if got != test.want || encoded(o) != before {
				t.Fatal("non-strict failure wakeup accepted unbound facts or changed evidence", got, test.want)
			}
		})
	}
}

func TestCIWatchMixedFailureRequiresApplicableTrustedFacts(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 43, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "stack/parent"}
	reassess := func(o *reviewprovider.CIObservation) {
		o.Assessment, _ = reviewprovider.AssessRequiredCI(o.Authority, *o.Results, *o.Actions, o.Attempts)
		o.State, o.Reason = o.Assessment.State, o.Assessment.Reason
	}
	for _, test := range []struct {
		name   string
		mutate func(*reviewprovider.CIObservation)
		want   bool
	}{
		{"proven mixed failure", func(*reviewprovider.CIObservation) {}, true},
		{"strict bound failure", func(o *reviewprovider.CIObservation) { o.Authority.Strict = true }, true},
		{"missing only", func(o *reviewprovider.CIObservation) { o.Results.Statuses = o.Results.Statuses[1:]; reassess(o) }, false},
		{"pending plus missing", func(o *reviewprovider.CIObservation) { o.Results.Statuses[0].State = "pending"; reassess(o) }, false},
		{"untrusted status origin", func(o *reviewprovider.CIObservation) { o.Authority.Checks[0].AppID = 100; reassess(o) }, false},
		{"coherence downgrade", func(o *reviewprovider.CIObservation) { o.Reason = "ci_observation_changed" }, false},
		{"incomplete candidate", func(o *reviewprovider.CIObservation) { o.Candidate.State = "unproven" }, false},
		{"foreign candidate", func(o *reviewprovider.CIObservation) { o.Candidate.RepositoryID++ }, false},
		{"stale head", func(o *reviewprovider.CIObservation) { o.Candidate.HeadSHA = strings.Repeat("c", 40) }, false},
		{"stale base", func(o *reviewprovider.CIObservation) { o.Candidate.BaseSHA = strings.Repeat("c", 40) }, false},
		{"changed base ref", func(o *reviewprovider.CIObservation) { o.Candidate.BaseRef = "different" }, false},
		{"changed merge identity", func(o *reviewprovider.CIObservation) { o.Candidate.MergeState = "verified" }, false},
		{"missing current attempts", func(o *reviewprovider.CIObservation) { o.Attempts = nil }, false},
		{"forged retained failure", func(o *reviewprovider.CIObservation) { o.Results.Statuses = o.Results.Statuses[1:] }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			o := ciWatchMixedFailure(target, "develop")
			source := SourceObservation{RepositoryID: target.RepositoryID, RepositoryName: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, ParentSHA: target.BaseSHA, ObservedAt: o.StartedAt.Add(-time.Millisecond).Format(time.RFC3339Nano)}
			test.mutate(&o)
			before := encoded(o)
			got := validCIWithAuthorityBranch(o, source, "develop", false) && ciWatchRequiredFailure(o)
			if got != test.want || encoded(o) != before {
				t.Fatal("failure wakeup accepted unproven authority or changed retained facts", got, test.want)
			}
		})
	}
}
