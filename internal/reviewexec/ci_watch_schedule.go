package reviewexec

import (
	"context"
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

const ciWatchFallbackInterval = time.Minute

type ciWatchClaim struct {
	entry      *ciWatchEntry
	generation uint64
	version    uint64
	ctx        context.Context
}

func (s *lifecycleSchedule) registerCIWatch(scope core.Scope, binding ciWatchBinding, targets []CIWatchTarget) (*ciWatchSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.readers[binding.capability].(ciGateReader); !ok || len(s.watchers) >= 64 || s.watchPrincipals[scope.PrincipalID] >= 8 {
		return nil, ciWatchUnavailable()
	}
	additional := 0
	for _, target := range targets {
		if s.watches[ciWatchKey{binding, target.PullNumber}] == nil {
			additional++
		}
	}
	if len(s.watches)+additional > 256 {
		return nil, ciWatchUnavailable()
	}
	if s.watches == nil {
		s.watches = map[ciWatchKey]*ciWatchEntry{}
		s.watchers = map[*ciWatchSubscription]bool{}
		s.watchPrincipals = map[string]int{}
	}
	sub := &ciWatchSubscription{scope: scope, changed: make(chan struct{}, 1), targets: append([]CIWatchTarget(nil), targets...)}
	for _, target := range targets {
		key := ciWatchKey{binding, target.PullNumber}
		entry := s.watches[key]
		if entry == nil {
			entry = &ciWatchEntry{key: key, owners: map[*ciWatchSubscription]bool{}}
			s.watches[key] = entry
		}
		entry.owners[sub] = true
		entry.wanted++
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
			if entry.cancel != nil {
				entry.cancel()
			}
		}
	}
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
			if sub.bases[i] == "" && item.ObservedBaseSHA != "" {
				sub.bases[i] = item.ObservedBaseSHA
			}
			if item.State != "closed" && item.ObservedHeadSHA != "" && (item.ObservedHeadSHA != target.HeadSHA || sub.bases[i] != "" && item.ObservedBaseSHA != sub.bases[i]) {
				item.State, item.Reason = "superseded", "ci_watch_source_changed"
			}
			if entry.wanted != entry.done {
				item.State, item.Reason = "pending", "ci_watch_refresh_pending"
			}
		}
		item.CIWatchTarget = target
		if result.State == "pending" && item.State != "pending" {
			result.State = item.State
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
		eventDue := entry.wanted != entry.done && (entry.lastAttempt.IsZero() || !now.Before(entry.lastAttempt.Add(5*time.Second)))
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
		if s.watches[entry.key] != entry || entry.inflight || len(entry.owners) == 0 {
			s.mu.Unlock()
			continue
		}
		entry.started++
		entry.inflight, entry.lastAttempt = true, now
		readCtx, cancel := context.WithCancel(ctx)
		entry.cancel = cancel
		claim := &ciWatchClaim{entry: entry, generation: entry.wanted, version: entry.started, ctx: readCtx}
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
			providerErr = nil
		}
		s.finishAccountLocked(target, providerErr, s.options.Now())
		entry.cancel()
		entry.cancel, entry.inflight = nil, false
		entry.done, entry.version = claim.generation, claim.version
		entry.next = s.options.Now().Add(ciWatchFallbackInterval)
		entry.result = item
		for sub := range entry.owners {
			select {
			case sub.changed <- struct{}{}:
			default:
			}
		}
	}()
	readCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
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
	observation, err := target.reader.(ciGateReader).ObserveCIGate(readCtx, sourceCITarget(source), repo.DefaultBranch)
	if err != nil {
		providerErr = err
		item.Reason = sourceCIErrorReason(readCtx, err)
		return nil
	}
	if !validCIWithAuthorityBranch(observation, source, repo.DefaultBranch, false) || observation.StartedAt.Before(mustLifecycleTime(source.ObservedAt)) || observation.ObservedAt.Before(observation.StartedAt) || observation.ObservedAt.After(time.Now().Add(5*time.Second)) {
		item.Reason = "ci_watch_observation_unproven"
		return nil
	}
	finalRepo, err := target.reader.Repository(readCtx, target.repository)
	if err != nil {
		providerErr = err
		item.Reason = sourceCIErrorReason(readCtx, err)
		return nil
	}
	if finalRepo.ID != repo.ID || finalRepo.FullName != repo.FullName || finalRepo.DefaultBranch != repo.DefaultBranch {
		item.Reason = "ci_watch_gate_changed"
		return nil
	}
	summary := summarizeSourceCI(observation)
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
	return nil
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
		head := entry.result.ObservedHeadSHA
		if head == "" {
			for sub := range entry.owners {
				for _, t := range sub.targets {
					if t.PullNumber == entry.key.number && t.HeadSHA == hint.HeadSHA {
						head = t.HeadSHA
					}
				}
			}
		}
		sameHead := hint.HeadSHA != "" && strings.EqualFold(head, hint.HeadSHA)
		branch := hint.BranchRef != "" && (hint.BranchRef == entry.result.BaseRef || hint.BranchRef == entry.result.GateRef)
		knownCI := hint.HeadSHA != "" && hint.HeadSHA == entry.result.CICommitSHA
		if hint.Metadata && explicit || hint.CI && (sameHead || branch || knownCI || explicit) {
			entry.wanted++
		}
	}
}
