package reviewexec

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSourceTargetConfigurationPreservesPrefixAndActualParentMembership(t *testing.T) {
	original := Configuration{CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{9, 42, 2, 8}, Channels: []string{"cli", "hosted"}}
	for _, test := range []struct {
		name   string
		queue  []int64
		parent int64
		mutate func(*Configuration)
		valid  bool
	}{
		{name: "unchanged", valid: true},
		{name: "append unrelated suffix", queue: []int64{9, 42, 2, 8, 6}, valid: true},
		{name: "replace unrelated suffix", queue: []int64{9, 42, 6, 8}, valid: true},
		{name: "remove unrelated suffix", queue: []int64{9, 42, 8}, valid: true},
		{name: "move actual parent within suffix", queue: []int64{9, 42, 8, 2}, valid: true},
		{name: "pause with suffix edit", queue: []int64{9, 42, 8}, mutate: func(c *Configuration) { c.Paused = true }, valid: true},
		{name: "missing target", queue: []int64{9, 2, 8}},
		{name: "changed prefix", queue: []int64{42, 9, 2, 8}},
		{name: "removed predecessor", queue: []int64{42, 2, 8}},
		{name: "removed actual parent after target", queue: []int64{9, 42, 2}},
		{name: "external parent stays external", queue: []int64{9, 42, 6}, parent: 99, valid: true},
		{name: "external parent newly enrolled", queue: []int64{9, 42, 2, 8, 99}, parent: 99},
		{name: "capability changed", mutate: func(c *Configuration) { c.CapabilityID = "other-account" }},
		{name: "mode changed", mutate: func(c *Configuration) { c.Mode = "independent" }},
		{name: "channels changed", mutate: func(c *Configuration) { c.Channels = []string{"cli"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := original
			current.Queue = slices.Clone(original.Queue)
			current.Channels = slices.Clone(original.Channels)
			if test.queue != nil {
				current.Queue = test.queue
			}
			if test.mutate != nil {
				test.mutate(&current)
			}
			parent := test.parent
			if parent == 0 {
				parent = 8
			}
			beforeCurrent, beforeOriginal := encoded(current), encoded(original)
			if got := sameSourceTargetConfiguration(current, original, 42, parent); got != test.valid {
				t.Fatal("source target applicability", got, test.valid)
			}
			if beforeCurrent != encoded(current) || beforeOriginal != encoded(original) {
				t.Fatal("source compatibility changed retained/current plans")
			}
			if test.valid && !slices.Equal(current.Queue, original.Queue) && sameSourceConfiguration(current, original) {
				t.Fatal("whole-queue pause compatibility was relaxed")
			}
		})
	}
}

func sourceSuffixReader() *readinessTestReader {
	r := newReadinessTestReader()
	r.pull = func(_ context.Context, _ string, number int64) (reviewprovider.Pull, error) {
		return reviewprovider.Pull{Number: number, RepositoryID: 801, State: "open", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "source-4", ChangedFiles: 1}, nil
	}
	r.parents = map[string]reviewprovider.Pull{"source-4": {Number: 4, RepositoryID: 801, State: "open", HeadRef: "source-4", HeadSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("c", 40)}}
	return r
}

func TestSourceTargetAssessmentsReuseCombinedObservationAcrossSuffixEdits(t *testing.T) {
	f, a, evidence, _ := coverageFixture(t, 0, false)
	coverageFinalize(t, f, a, evidence, 0)
	assessmentPolicy(t, f, 1, []string{"cli"})
	revision := int64(1)
	configuration := Configuration{CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}
	replace := func() {
		t.Helper()
		configuration.ExpectedRevision = &revision
		if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), fmt.Sprintf("target-config-%d", revision), configuration); err != nil {
			t.Fatal(err)
		}
		revision++
	}
	replace()
	reader := sourceSuffixReader()
	original := collectReadiness(t, f, reader, "combined-source", 2, revision)
	if original.Observation == nil || original.Observation.ParentPullNumber != 4 {
		t.Fatal("fixture did not resolve actual parent after target", original)
	}
	retained := snapshotTable(t, f, "review_source_refreshes")
	sourceCalls, ciCalls, reviewCalls := reader.calls.Load(), reader.ciCalls.Load(), reader.reviewCalls.Load()
	scope := sourceCIScope(t, f)
	assertAssessments := func(stale bool) {
		t.Helper()
		before := readinessDurable(t, f, a)
		policy, err := f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
		if err != nil {
			t.Fatal(err)
		}
		ci, err := f.e.ReadSourceCIAssessment(t.Context(), scope, 2)
		if err != nil {
			t.Fatal(err)
		}
		diagnostics, err := f.e.ReadSourceCIDiagnostics(t.Context(), scope, 2)
		if err != nil {
			t.Fatal(err)
		}
		readiness, err := f.e.ReadSourceReadiness(t.Context(), scope, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, identity := range []struct {
			id       string
			target   SourceObservation
			at       string
			revision int64
		}{{policy.RefreshID, policy.Target, policy.SourceObservedAt, policy.ConfigurationRevision}, {ci.RefreshID, ci.Target, ci.SourceObservedAt, ci.ConfigurationRevision}, {diagnostics.RefreshID, diagnostics.Target, diagnostics.SourceObservedAt, diagnostics.ConfigurationRevision}, {readiness.RefreshID, readiness.Target, readiness.SourceObservedAt, readiness.ConfigurationRevision}} {
			if identity.id != original.ID || identity.target != *original.Observation || identity.at != original.Observation.ObservedAt || identity.revision != revision {
				t.Fatal("assessment changed original observation identity/time or missed current config", identity)
			}
		}
		if stale {
			if policy.State != "held" || policy.Reason != "source_configuration_changed" || ci.State != "held" || ci.Reason != "source_configuration_changed" || diagnostics.Reason != "source_configuration_changed" || readiness.State != "held" || !slices.Equal(readiness.Reasons, []string{"source_configuration_changed"}) {
				t.Fatal("meaningful binding change reused stale source", policy, ci, readiness)
			}
		} else if policy.State != "sufficient" || policy.Reason != "" || ci.State != "not_required" || diagnostics.State != "not_required" || readiness.State != "ready_as_observed" || len(readiness.Reasons) != 0 || !reflect.DeepEqual(readiness.CI, *original.CI) || !reflect.DeepEqual(readiness.ReviewState, *original.ReviewState) {
			t.Fatal("irrelevant suffix/pause invalidated retained comparison", policy, ci, readiness)
		}
		if before != readinessDurable(t, f, a) || retained != snapshotTable(t, f, "review_source_refreshes") || sourceCalls != reader.calls.Load() || ciCalls != reader.ciCalls.Load() || reviewCalls != reader.reviewCalls.Load() {
			t.Fatal("assessment mutated retained source/CI/readiness or contacted provider")
		}
	}
	assertAssessments(false)
	if _, err := f.e.ChangeQueue(t.Context(), f.scope(t, attemptProject, f.writer, false), "enroll-later", "add", QueueChange{ExpectedConfigurationRevision: revision, PullNumber: 6}); err != nil {
		t.Fatal(err)
	}
	revision++
	configuration.Queue = []int64{2, 4, 6}
	assertAssessments(false)
	configuration.Queue = []int64{2, 6, 4}
	configuration.Paused = true
	replace()
	assertAssessments(false)
	configuration.Queue = []int64{2, 4}
	configuration.Paused = false
	replace()
	assertAssessments(false)
	configuration.Queue = []int64{2, 6}
	replace()
	assertAssessments(true)
	configuration.Queue = []int64{2, 4, 6}
	replace()
	assertAssessments(false)
	configuration.Queue = []int64{6, 2, 4}
	replace()
	assertAssessments(true)
	configuration.Queue = []int64{2, 4, 6}
	replace()
	assertAssessments(false)
	configuration.CapabilityID = "other-account"
	replace()
	assertAssessments(true)
	configuration.CapabilityID = "test-capability"
	replace()
	assertAssessments(false)
	capabilities := append([]Capability(nil), attemptCapabilities...)
	capabilities[0].Fingerprint = strings.Repeat("9", 64)
	var err error
	f.e, err = New(f.store, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	assertAssessments(true)
	f.e, err = New(f.store, attemptCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	assertAssessments(false)
	// Applicability survives collection-actor revocation; current policy still
	// re-evaluates the same facts and can add a missing required channel.
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	assertAssessments(false)
	assessmentPolicy(t, f, 2, []string{"cli", "hosted"})
	policy, err := f.e.ReadSourcePolicyAssessment(t.Context(), scope, 2)
	if err != nil || policy.State != "held" || policy.Reason != "" || policy.PolicyRevision != 3 {
		t.Fatal("current policy did not re-evaluate retained facts", policy, err)
	}
	readiness, err := f.e.ReadSourceReadiness(t.Context(), scope, 2)
	if err != nil || readiness.State != "held" || !slices.Contains(readiness.Reasons, "review_coverage_missing") {
		t.Fatal("current readiness policy bypassed", readiness, err)
	}
	if retained != snapshotTable(t, f, "review_source_refreshes") || sourceCalls != reader.calls.Load() || ciCalls != reader.ciCalls.Load() || reviewCalls != reader.reviewCalls.Load() {
		t.Fatal("current policy update required provider refresh or rewrote observations")
	}
	// Repository rebinding invalidates native derived proofs too; keep this
	// destructive binding negative last rather than manufacture restored credit.
	admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
	assertAssessments(true)
}

func TestSourceTargetInFlightRefreshAllowsSuffixAndRejectsActualParentRemoval(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	revision := int64(1)
	configuration := Configuration{CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}}
	replace := func() {
		t.Helper()
		configuration.ExpectedRevision = &revision
		if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), fmt.Sprintf("inflight-config-%d", revision), configuration); err != nil {
			t.Fatal(err)
		}
		revision++
	}
	replace()
	request := refreshRequest()
	request.ExpectedConfigurationRevision = revision
	first := refreshEnqueue(t, f, "before-suffix", request)
	configuration.Queue = []int64{2, 4, 6}
	replace()
	reader := sourceSuffixReader()
	var change func()
	reader.repository = func(_ context.Context, id int64) (reviewprovider.Repository, error) {
		if change != nil {
			apply := change
			change = nil
			apply()
		}
		return reviewprovider.Repository{ID: id, FullName: "owner/repo", DefaultBranch: "main"}, nil
	}
	change = func() { configuration.Queue = []int64{2, 4, 6, 8}; configuration.Paused = true; replace() }
	worked, err := f.e.refreshNext(t.Context(), map[string]TargetReader{"test-capability": reader})
	if err != nil || !worked {
		t.Fatal(err)
	}
	completed := refreshRead(t, f, first.ID)
	if completed.State != "completed" || completed.ConfigurationRevision != 2 || completed.Observation == nil || completed.Observation.ParentPullNumber != 4 || reader.calls.Load() != 3 {
		t.Fatal("suffix before/during refresh invalidated original read", completed, reader.calls.Load())
	}
	request.ExpectedConfigurationRevision = revision
	second := refreshEnqueue(t, f, "remove-parent-during-read", request)
	change = func() { configuration.Queue = []int64{2, 6, 8}; replace() }
	worked, err = f.e.refreshNext(t.Context(), map[string]TargetReader{"test-capability": reader})
	if err != nil || !worked {
		t.Fatal(err)
	}
	held := refreshRead(t, f, second.ID)
	if held.State != "held" || held.Reason != "source_configuration_changed" || held.Observation != nil || held.CI != nil || held.ReviewState != nil || reader.calls.Load() != 6 {
		t.Fatal("actual parent removed during read became applicable", held, reader.calls.Load())
	}
	if after := refreshRead(t, f, first.ID); !reflect.DeepEqual(after, completed) {
		t.Fatal("failed later read changed original observation")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 {
		t.Fatal("source compatibility authorized review execution")
	}
}
