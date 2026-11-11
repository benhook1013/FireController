package reviewexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/githubhook"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type ciGateReader interface {
	ObserveCIGate(context.Context, reviewprovider.CITarget, string) (reviewprovider.CIObservation, error)
}

type ciGateWorkflowReader interface {
	ObserveCIGateWithWorkflows(context.Context, reviewprovider.CITarget, string, []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error)
}

type ciGateHeadWorkflowReader interface {
	ObserveCIGateWithWorkflowAttention(context.Context, reviewprovider.CITarget, string, []int64, []int64, []int64) (reviewprovider.CIObservation, []reviewprovider.CIWorkflowFailure, error)
}

const ciWatchFallbackInterval = time.Minute

type ciWatchClaim struct {
	entry                 *ciWatchEntry
	generation            uint64
	metadataGeneration    uint64
	version               uint64
	ctx                   context.Context
	workflowIDs           []int64
	headWorkflowIDs       []int64
	historicalWorkflowIDs []int64
}

func (s *lifecycleSchedule) registerCIWatch(scope core.Scope, binding ciWatchBinding, targets []CIWatchTarget) (*ciWatchSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.readers[binding.capability].(ciGateReader); !ok {
		return nil, ciWatchUnavailable()
	}
	if len(s.watchers) >= 64 || s.watchPrincipals[scope.PrincipalID] >= 8 {
		return nil, ciWatchCapacity()
	}
	additional := 0
	for _, target := range targets {
		entry := s.watches[ciWatchKey{binding, target.PullNumber}]
		if len(target.FailureWorkflowIDs) > 0 {
			reader, ok := s.readers[binding.capability].(ciGateWorkflowReader)
			if !ok {
				return nil, ciWatchWorkflowUnsupported()
			}
			if available, known := reader.(interface{ SupportsCIWorkflowFailures() bool }); known && !available.SupportsCIWorkflowFailures() {
				return nil, ciWatchWorkflowUnsupported()
			}
		}
		if len(ciWatchWorkflowUnion(entry, target.FailureWorkflowIDs)) > 16 {
			return nil, core.Fail("ci_watch_workflow_capacity", "The shared PR watch already selects sixteen attention workflows; no extra observation was started.", 503)
		}
		if entry == nil {
			additional++
		}
	}
	if len(s.watches)+additional > 256 {
		return nil, ciWatchCapacity()
	}
	if s.watches == nil {
		s.watches = map[ciWatchKey]*ciWatchEntry{}
		s.watchers = map[*ciWatchSubscription]bool{}
		s.watchPrincipals = map[string]int{}
	}
	sub := &ciWatchSubscription{scope: scope, changed: make(chan struct{}, 1), targets: append([]CIWatchTarget(nil), targets...)}
	for i := range sub.targets {
		sub.targets[i].FailureWorkflowIDs = append([]int64(nil), sub.targets[i].FailureWorkflowIDs...)
	}
	for _, target := range sub.targets {
		key := ciWatchKey{binding, target.PullNumber}
		entry := s.watches[key]
		if entry == nil {
			entry = &ciWatchEntry{key: key, owners: map[*ciWatchSubscription]bool{}}
			s.watches[key] = entry
		}
		entry.owners[sub] = true
		sub.entries = append(sub.entries, entry)
		// No pre-existing observation, including an older mismatch, can settle
		// a new watch. Overlapping subscribers before a claim share its check.
		sub.minimum = append(sub.minimum, entry.started+1)
		sub.bases = append(sub.bases, target.BaseSHA)
	}
	s.watchers[sub] = true
	s.watchPrincipals[scope.PrincipalID]++
	return sub, nil
}

func (s *lifecycleSchedule) unregisterCIWatch(sub *ciWatchSubscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.watchers, sub)
	s.watchPrincipals[sub.scope.PrincipalID]--
	if s.watchPrincipals[sub.scope.PrincipalID] == 0 {
		delete(s.watchPrincipals, sub.scope.PrincipalID)
	}
	for _, entry := range sub.entries {
		delete(entry.owners, sub)
		if len(entry.owners) == 0 {
			if s.watches[entry.key] == entry {
				delete(s.watches, entry.key)
			}
		}
		// A departed subscriber can leave an in-flight freshness-only read
		// behind even when older owners retain already-fresh observations.
		if entry.cancel != nil && (len(entry.owners) == 0 || !ciWatchHasDemand(entry, s.options.Now())) {
			entry.cancel()
		}
	}
}

// Called under the scheduler lock. Real hints, due fallback checks and each
// remaining owner's freshness requirement independently retain collection.
func ciWatchHasDemand(entry *ciWatchEntry, now time.Time) bool {
	if entry.wanted != entry.done || !now.Before(entry.next) {
		return true
	}
	for sub := range entry.owners {
		for i, owned := range sub.entries {
			if owned == entry && sub.minimum[i] > entry.version {
				return true
			}
		}
	}
	return false
}

func (s *lifecycleSchedule) ciWatchResult(sub *ciWatchSubscription) CIWatchResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := CIWatchResult{ProjectID: sub.scope.ProjectID, State: "pending", Targets: make([]CIWatchItem, 0, len(sub.targets))}
	for i, target := range sub.targets {
		item := CIWatchItem{State: "pending", Reason: "ci_watch_check_pending", Checks: []SourceCIContext{}}
		entry := sub.entries[i]
		if entry.version >= sub.minimum[i] {
			item = entry.result
			if item.MergeBaseMismatch != nil {
				facts := *item.MergeBaseMismatch
				item.MergeBaseMismatch = &facts
			}
			item.Checks = append([]SourceCIContext{}, item.Checks...)
			for i := range item.Checks {
				if p := item.Checks[i].CheckRun; p != nil {
					copy := *p
					item.Checks[i].CheckRun = &copy
				}
				if p := item.Checks[i].CommitStatus; p != nil {
					copy := *p
					item.Checks[i].CommitStatus = &copy
				}
			}
			if item.State != "closed" && item.ObservedHeadSHA != "" && (item.ObservedHeadSHA != target.HeadSHA || sub.bases[i] != "" && item.ObservedBaseSHA != sub.bases[i]) {
				item.State, item.Reason = "superseded", "ci_watch_source_changed"
			}
			// CI hints cannot undo fresh closure or source supersession. A
			// metadata hint can represent reopening and needs fresh PR proof.
			refresh := entry.wanted != entry.done && (item.State != "closed" || entry.metadataWanted != entry.metadataDone)
			if refresh && item.State != "superseded" {
				item.State, item.Reason = "pending", "ci_watch_refresh_pending"
			}
		}
		item.CIWatchTarget = target
		if item.State != "pending" || item.Reason != "ci_merge_base_mismatch" {
			item.MergeBaseMismatch = nil
		}
		item.FailureWorkflowIDs = append([]int64(nil), target.FailureWorkflowIDs...)
		rows := item.WorkflowFailures
		observations := item.WorkflowObservations
		basis := item.WorkflowAttentionBasis
		item.WorkflowAttentionBasis = ""
		item.WorkflowFailures, item.WorkflowObservations = nil, nil
		headOnly := target.AllowHeadAttention && basis == reviewprovider.CIWorkflowHeadOnlyBasis && item.Reason == "ci_merge_base_mismatch" && item.MergeBaseMismatch != nil && item.Selection == "" && item.CICommitSHA == ""
		applicable := basis == "" && sha.MatchString(item.CICommitSHA) && (item.Selection == "head" || item.Selection == "test_merge")
		if item.State == "pending" && item.Reason != "ci_watch_refresh_pending" && sha.MatchString(item.ObservedHeadSHA) && sha.MatchString(item.ObservedBaseSHA) && (applicable || headOnly) && len(target.FailureWorkflowIDs) > 0 {
			for _, row := range observations {
				for _, id := range target.FailureWorkflowIDs {
					if row.WorkflowID == id {
						item.WorkflowObservations = append(item.WorkflowObservations, row)
						break
					}
				}
			}
			for _, row := range rows {
				if row.Basis == reviewprovider.CIWorkflowHistoricalBasis && !target.AllowHistoricalAttention {
					continue
				}
				for _, id := range target.FailureWorkflowIDs {
					if row.WorkflowID == id {
						item.WorkflowFailures = append(item.WorkflowFailures, row)
						break
					}
				}
			}
			if headOnly && len(item.WorkflowFailures) > 0 {
				item.WorkflowAttentionBasis = basis
			}
			// Latest-run details cannot describe an earlier substantive execution.
			// Omit this optional group rather than pair conflicting run identities;
			// the failure row retains the exact proven execution for all clients.
			for _, failure := range item.WorkflowFailures {
				for _, observation := range item.WorkflowObservations {
					if failure.WorkflowID == observation.WorkflowID && (failure.RunID != observation.RunID || failure.RunAttempt != observation.RunAttempt) {
						item.WorkflowObservations = nil
						break
					}
				}
			}
		}
		if len(target.FailureWorkflowIDs) > 0 && item.ObservationKey != "" {
			item.ObservationKey = ciWatchWorkflowKey(item.ObservationKey, target.FailureWorkflowIDs, item.WorkflowFailures)
		}
		item.WaitingForChange = (item.State == "failed" || target.AllowPassedRearm && item.State == "passed" || len(item.WorkflowFailures) > 0) && item.ObservationKey != "" && item.ObservationKey == target.AfterObservationKey
		if result.State == "pending" && item.State != "pending" && !item.WaitingForChange {
			result.State = item.State
		}
		if result.State == "pending" && len(item.WorkflowFailures) > 0 && !item.WaitingForChange {
			result.State = "attention"
		}
		result.Targets = append(result.Targets, item)
	}
	return result
}

// claimCIWatch runs under the lifecycle claim guard and shares its enrichment
// reservations. It performs only bounded authorization/configuration reads.
func (e *Engine) claimCIWatch(ctx context.Context, readers map[string]TargetReader, s *lifecycleSchedule) (*lifecycleTarget, error) {
	s.mu.Lock()
	entries := make([]*ciWatchEntry, 0, len(s.watches))
	for _, entry := range s.watches {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].lastAttempt.Equal(entries[j].lastAttempt) {
			return entries[i].lastAttempt.Before(entries[j].lastAttempt)
		}
		if entries[i].key.binding.project != entries[j].key.binding.project {
			return entries[i].key.binding.project < entries[j].key.binding.project
		}
		return entries[i].key.number < entries[j].key.number
	})
	s.mu.Unlock()
	for _, entry := range entries {
		b := entry.key.binding
		now := s.options.Now()
		s.mu.Lock()
		lane := &s.lanes[lifecycleEnrichment]
		// Subscriber freshness belongs to that owner. Only real CI hints
		// invalidate proof already fresh enough for other subscribers.
		needsFresh := false
		for sub := range entry.owners {
			for i, owned := range sub.entries {
				if owned == entry && sub.minimum[i] > entry.version {
					needsFresh = true
				}
			}
		}
		eventDue := (entry.wanted != entry.done || needsFresh) && (entry.lastAttempt.IsZero() || !now.Before(entry.lastAttempt.Add(5*time.Second)))
		eligible := s.watches[entry.key] == entry && !entry.inflight && len(entry.owners) > 0 && (eventDue || !now.Before(entry.next)) && !lane.projects[b.project] && !lane.accounts[b.account] && !now.Before(s.accounts[b.account].until)
		owners := make([]*ciWatchSubscription, 0, len(entry.owners))
		if eligible {
			for sub := range entry.owners {
				owners = append(owners, sub)
			}
		}
		s.mu.Unlock()
		if !eligible {
			continue
		}
		authorized := false
		for _, sub := range owners {
			current, err := e.ciWatchBinding(ctx, sub.scope)
			if err == nil && current == b {
				authorized = true
				break
			}
			select {
			case sub.changed <- struct{}{}:
			default:
			}
		}
		reader := readers[b.capability]
		if _, ok := reader.(ciGateReader); !authorized || !ok {
			continue
		}
		s.mu.Lock()
		// Metadata observations may establish account cooldown while authorization waits.
		now = s.options.Now()
		if s.watches[entry.key] != entry || entry.inflight || len(entry.owners) == 0 || now.Before(s.accounts[b.account].until) || !ciWatchHasDemand(entry, now) {
			s.mu.Unlock()
			continue
		}
		entry.started++
		entry.inflight, entry.lastAttempt = true, now
		readCtx, cancel := context.WithCancel(ctx)
		entry.cancel = cancel
		claim := &ciWatchClaim{entry: entry, generation: entry.wanted, metadataGeneration: entry.metadataWanted, version: entry.started, ctx: readCtx, workflowIDs: ciWatchWorkflowUnion(entry, nil), headWorkflowIDs: ciWatchOptionalWorkflowUnion(entry, false), historicalWorkflowIDs: ciWatchOptionalWorkflowUnion(entry, true)}
		lane.projects[b.project], lane.accounts[b.account] = true, true
		s.mu.Unlock()
		return &lifecycleTarget{project: b.project, account: b.account, repository: b.repository, number: entry.key.number, fingerprint: b.fingerprint, reader: reader, lane: lifecycleEnrichment, watch: claim}, nil
	}
	return nil, nil
}

func (e *Engine) observeCIWatch(ctx context.Context, target lifecycleTarget, s *lifecycleSchedule) error {
	claim := target.watch
	ctx = claim.ctx
	item := CIWatchItem{State: "pending", Reason: "ci_watch_source_unavailable", Checks: []SourceCIContext{}}
	var providerErr error
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		entry := claim.entry
		if ctx.Err() != nil {
			var limit *reviewprovider.Error
			if !errors.As(providerErr, &limit) || limit.RetryAfterSeconds <= 0 {
				providerErr = nil
			}
		}
		s.finishAccountLocked(target, providerErr, s.options.Now())
		canceled := ctx.Err() != nil
		entry.cancel()
		entry.cancel, entry.inflight = nil, false
		if canceled {
			// Cancellation releases capacity without replacing completed facts
			// or consuming demand from hints/subscribers that arrived meanwhile.
			return
		}
		entry.done, entry.version = claim.generation, claim.version
		entry.metadataDone = claim.metadataGeneration
		entry.next = s.options.Now().Add(ciWatchFallbackInterval)
		entry.result = item
		for sub := range entry.owners {
			// Pin the first eligible fresh source observation before another
			// completion can replace it, even if this subscriber reads late.
			for i, owned := range sub.entries {
				if owned == entry && sub.bases[i] == "" && claim.version >= sub.minimum[i] && item.ObservedHeadSHA == sub.targets[i].HeadSHA && sha.MatchString(item.ObservedBaseSHA) {
					sub.bases[i] = item.ObservedBaseSHA
				}
			}
			select {
			case sub.changed <- struct{}{}:
			default:
			}
		}
	}()
	readCtx, cancel := context.WithTimeout(ctx, reviewprovider.CIObservationTimeout+15*time.Second)
	defer cancel()
	repo, err := target.reader.Repository(readCtx, target.repository)
	if err != nil {
		providerErr = err
		item.Reason = sourceCIErrorReason(readCtx, err)
		return nil
	}
	if repo.ID != target.repository || !repository.MatchString(repo.FullName) || repo.DefaultBranch == "" || !safeText(repo.DefaultBranch, 256) {
		item.Reason = "ci_watch_repository_unproven"
		return nil
	}
	pull, err := target.reader.Pull(readCtx, repo.FullName, target.number)
	if err != nil {
		providerErr = err
		item.Reason = sourceCIErrorReason(readCtx, err)
		return nil
	}
	if !sameTarget(pull, target.repository, target.number) || pull.State != "open" && pull.State != "closed" || pull.BaseRef == "" || !safeText(pull.BaseRef, 256) {
		item.Reason = "ci_watch_source_unproven"
		return nil
	}
	item.ObservedHeadSHA, item.ObservedBaseSHA, item.BaseRef, item.GateRef = pull.HeadSHA, pull.BaseSHA, pull.BaseRef, repo.DefaultBranch
	item.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if pull.State == "closed" || pull.Merged {
		item.State, item.Reason = "closed", "ci_watch_pr_closed"
		return nil
	}
	s.mu.Lock()
	interested := false
	for sub := range claim.entry.owners {
		for i, expected := range sub.targets {
			if expected.PullNumber == target.number && expected.HeadSHA == pull.HeadSHA && (sub.bases[i] == "" || sub.bases[i] == pull.BaseSHA) {
				interested = true
			}
		}
	}
	s.mu.Unlock()
	if !interested {
		item.Reason = "ci_watch_source_changed"
		return nil
	}
	source := SourceObservation{RepositoryID: repo.ID, RepositoryName: repo.FullName, PullNumber: pull.Number, HeadSHA: pull.HeadSHA, BaseSHA: pull.BaseSHA, BaseRef: pull.BaseRef, ParentSHA: pull.BaseSHA, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	var observation reviewprovider.CIObservation
	var workflowFailures []reviewprovider.CIWorkflowFailure
	if len(claim.workflowIDs) > 0 {
		if reader, ok := target.reader.(ciGateHeadWorkflowReader); ok && (len(claim.headWorkflowIDs) > 0 || len(claim.historicalWorkflowIDs) > 0) {
			observation, workflowFailures, err = reader.ObserveCIGateWithWorkflowAttention(readCtx, sourceCITarget(source), repo.DefaultBranch, claim.workflowIDs, claim.headWorkflowIDs, claim.historicalWorkflowIDs)
		} else {
			observation, workflowFailures, err = target.reader.(ciGateWorkflowReader).ObserveCIGateWithWorkflows(readCtx, sourceCITarget(source), repo.DefaultBranch, claim.workflowIDs)
		}
	} else {
		observation, err = target.reader.(ciGateReader).ObserveCIGate(readCtx, sourceCITarget(source), repo.DefaultBranch)
	}
	if err != nil {
		var warning *reviewprovider.CIWorkflowAttentionError
		if len(claim.workflowIDs) > 0 && errors.As(err, &warning) && warning.Cause != nil {
			// Optional observation can fail without invalidating proven gate
			// facts. A local attempt cap is not a provider-account failure.
			if !errors.Is(warning.Cause, reviewprovider.ErrCIWorkflowAttemptLimit) {
				providerErr = warning
			}
		} else {
			providerErr = err
			item.Reason = sourceCIErrorReason(readCtx, err)
			return nil
		}
	}
	if !validCIWithAuthorityBranch(observation, source, repo.DefaultBranch, false) || observation.StartedAt.Before(mustLifecycleTime(source.ObservedAt)) || observation.ObservedAt.Before(observation.StartedAt) || observation.ObservedAt.After(time.Now().Add(5*time.Second)) {
		item.Reason = "ci_watch_observation_unproven"
		return nil
	}
	finalRepo, err := target.reader.Repository(readCtx, target.repository)
	if err != nil {
		if providerErr == nil || lifecycleProviderDelay(err, 0) > lifecycleProviderDelay(providerErr, 0) {
			providerErr = err
		}
		item.Reason = sourceCIErrorReason(readCtx, err)
		return nil
	}
	if finalRepo.ID != repo.ID || finalRepo.FullName != repo.FullName || finalRepo.DefaultBranch != repo.DefaultBranch {
		item.Reason = "ci_watch_gate_changed"
		return nil
	}
	summary := summarizeSourceCI(observation)
	if observation.Reason == "ci_merge_base_mismatch" && observation.Candidate != nil && observation.Candidate.MergeBaseMismatch != nil {
		facts := *observation.Candidate.MergeBaseMismatch
		item.MergeBaseMismatch = &facts
	}
	item.Reason, item.ObservedAt, item.CICommitSHA, item.Selection = summary.Reason, summary.ObservedAt, summary.SelectedCommitSHA, summary.Selection
	item.Required, item.Satisfied, item.Failed, item.Pending, item.Unproven = summary.Required, summary.Satisfied, summary.Failed, summary.Pending, summary.Unproven
	item.Checks, err = sourceCIContextDetails(&observation)
	if err != nil {
		item.Checks = []SourceCIContext{}
		item.Required, item.Satisfied, item.Failed, item.Pending, item.Unproven = 0, 0, 0, 0, 0
		item.Reason = "ci_watch_observation_unproven"
		return nil
	}
	switch observation.State {
	case "satisfied":
		item.State = "passed"
	case "failed":
		item.State = "failed"
	case "not_required":
		item.State = "unconfigured"
	}
	if ciWatchRequiredFailure(observation) {
		item.State, item.Reason = "failed", "ci_required_checks_failed"
	}
	item.ObservationKey = ciWatchObservationKey(target.project, observation, item)
	if item.State == "pending" {
		item.WorkflowFailures = ciWatchValidatedWorkflowFailures(observation, claim.workflowIDs, workflowFailures)
		if len(item.WorkflowFailures) > 0 {
			item.WorkflowAttentionBasis = reviewprovider.CIWorkflowAttentionBasis(observation)
		}
		item.WorkflowObservations = ciWatchWorkflowObservations(observation, claim.workflowIDs)
	}
	return nil
}

// Called under the scheduler lock; only explicit capable subscribers opt in.
func ciWatchOptionalWorkflowUnion(entry *ciWatchEntry, historical bool) []int64 {
	wanted := map[int64]bool{}
	for owner := range entry.owners {
		for _, target := range owner.targets {
			if target.PullNumber == entry.key.number && (historical && target.AllowHistoricalAttention || !historical && target.AllowHeadAttention) {
				for _, id := range target.FailureWorkflowIDs {
					wanted[id] = true
				}
			}
		}
	}
	ids := make([]int64, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func ciWatchWorkflowUnsupported() error {
	return core.Fail("ci_watch_workflow_failures_unsupported", "This source adapter cannot observe explicitly selected workflow failures; ordinary required-CI waiting remains available.", 409)
}

// Called under the scheduler lock. The union is per PR, not per subscriber.
func ciWatchWorkflowUnion(entry *ciWatchEntry, additional []int64) []int64 {
	wanted := map[int64]bool{}
	for _, id := range additional {
		wanted[id] = true
	}
	if entry != nil {
		for owner := range entry.owners {
			for _, target := range owner.targets {
				if target.PullNumber == entry.key.number {
					for _, id := range target.FailureWorkflowIDs {
						wanted[id] = true
					}
				}
			}
		}
	}
	ids := make([]int64, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func ciWatchWorkflowKey(base string, ids []int64, failures []reviewprovider.CIWorkflowFailure) string {
	ids = append([]int64(nil), ids...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	rows := append([]reviewprovider.CIWorkflowFailure(nil), failures...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].WorkflowID < rows[j].WorkflowID })
	body, _ := json.Marshal(struct {
		Base     string
		IDs      []int64
		Failures []reviewprovider.CIWorkflowFailure
	}{base, ids, rows})
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func ciWatchValidatedWorkflowFailures(o reviewprovider.CIObservation, ids []int64, rows []reviewprovider.CIWorkflowFailure) []reviewprovider.CIWorkflowFailure {
	if len(rows) == 0 || len(rows) > 16 {
		return nil
	}
	if reviewprovider.CIWorkflowAttentionBasis(o) != reviewprovider.CIWorkflowHeadOnlyBasis && (o.Results == nil || o.Actions == nil || o.Candidate == nil || o.Candidate.State != "complete" || o.Candidate.RepositoryID != o.Target.RepositoryID || o.Candidate.PullNumber != o.Target.PullNumber || o.Candidate.HeadSHA != o.Target.HeadSHA || o.Candidate.BaseSHA != o.Target.BaseSHA || o.Candidate.BaseRef != o.Target.BaseRef || o.SelectedCommitSHA == "" || o.Selection == "" || o.State != o.Assessment.State || o.Reason != o.Assessment.Reason) {
		return nil
	}
	wanted := map[int64]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	seen := map[int64]bool{}
	for _, row := range rows {
		if !wanted[row.WorkflowID] || seen[row.WorkflowID] || row.RunID <= 0 || row.RunAttempt <= 0 || row.JobID <= 0 || row.CheckRunID <= 0 || strings.TrimSpace(row.Name) == "" || !safeText(row.Name, 255) {
			return nil
		}
		switch row.Conclusion {
		case "failure", "timed_out", "action_required", "startup_failure":
		default:
			return nil
		}
		if !reviewprovider.CIWorkflowFailureApplies(o, row) {
			return nil
		}
		seen[row.WorkflowID] = true
	}
	result := append([]reviewprovider.CIWorkflowFailure(nil), rows...)
	sort.Slice(result, func(i, j int) bool { return result[i].WorkflowID < result[j].WorkflowID })
	return result
}

// A change key describes relevant proof, not its collection time or scheduling.
// It cannot authorize CI or provider work; an equal eligible verdict only keeps
// a reader subscribed to the same shared observer while preserving its facts.
func ciWatchObservationKey(project string, observation reviewprovider.CIObservation, item CIWatchItem) string {
	type check struct {
		SourceCIContext
		RunID      int64 `json:"run_id,omitempty"`
		RunAttempt int64 `json:"run_attempt,omitempty"`
	}
	checks := make([]check, 0, len(item.Checks))
	for _, detail := range item.Checks {
		value := check{SourceCIContext: detail}
		for _, assessed := range observation.Assessment.Checks {
			if assessed.Context == detail.Context && assessed.AppID == detail.AppID {
				value.RunID, value.RunAttempt = assessed.RunID, assessed.RunAttempt
				break
			}
		}
		checks = append(checks, value)
	}
	sort.Slice(checks, func(i, j int) bool {
		if checks[i].Context != checks[j].Context {
			return checks[i].Context < checks[j].Context
		}
		return checks[i].AppID < checks[j].AppID
	})
	facts := struct {
		Project                                                       string
		RepositoryID, PullNumber                                      int64
		Head, Base, BaseRef, Gate, CICommit, Selection, State, Reason string
		Checks                                                        []check
		MergeBaseMismatch                                             *reviewprovider.CIMergeBaseMismatch `json:",omitempty"`
	}{project, observation.Target.RepositoryID, observation.Target.PullNumber, observation.Target.HeadSHA, observation.Target.BaseSHA, observation.Target.BaseRef, item.GateRef, item.CICommitSHA, item.Selection, item.State, item.Reason, checks, item.MergeBaseMismatch}
	body, _ := json.Marshal(facts)
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// Webhooks wake only active subscriptions; no unobserved repository inventory
// is synthesized and no watch survives its last caller.
func (s *lifecycleSchedule) hintCIWatches(hint githubhook.Hint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.watches {
		if entry.key.binding.repository != hint.RepositoryID {
			continue
		}
		explicit := false
		for _, n := range hint.PullNumbers {
			explicit = explicit || n == entry.key.number
		}
		sameHead := hint.HeadSHA != "" && strings.EqualFold(entry.result.ObservedHeadSHA, hint.HeadSHA)
		// A new subscriber can pin a head not yet present in the shared cache.
		for sub := range entry.owners {
			for _, target := range sub.targets {
				if target.PullNumber == entry.key.number && hint.HeadSHA != "" && strings.EqualFold(target.HeadSHA, hint.HeadSHA) {
					sameHead = true
				}
			}
		}
		branch := hint.BranchRef != "" && (hint.BranchRef == entry.result.BaseRef || hint.BranchRef == entry.result.GateRef)
		knownCI := hint.HeadSHA != "" && hint.HeadSHA == entry.result.CICommitSHA
		if hint.Metadata && explicit || hint.CI && (sameHead || branch || knownCI || explicit) {
			entry.wanted++
			if hint.Metadata && explicit {
				entry.metadataWanted++
			}
		}
	}
}
