package reviewexec

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// HostedObserverStatus describes only this process's checks of an admitted
// attempt. It proves neither provider activity nor review completion.
type HostedObserverStatus struct {
	State        string `json:"state"`
	LastCheckAt  string `json:"last_check_at,omitempty"`
	LastSourceAt string `json:"last_source_at,omitempty"`
	NextCheckAt  string `json:"next_check_at,omitempty"`
	Reason       string `json:"reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

type hostedObserverEntry struct {
	project, operation, attempt string
	status                      HostedObserverStatus
	active                      int
	stopped                     bool
	forget                      bool
}

// HostedObserverCheck binds one in-process pass. Its private identity prevents
// a late completion from changing another attempt's current diagnostics.
type hostedObserverKey struct{ capability, attempt string }
type hostedAccountKey struct{ provider, account string }

func (e *Engine) hostedRecoveryAccount(capability string) (hostedAccountKey, bool) {
	if e == nil {
		return hostedAccountKey{}, false
	}
	c, ok := e.capabilities[capability]
	return hostedAccountKey{c.Provider, c.Account}, ok && slices.Contains(c.Channels, "hosted")
}

// HostedRecoveryNotBefore is a process-local provider response deadline shared
// by configured capabilities for that exact account, never inferred from age.
func (e *Engine) HostedRecoveryNotBefore(capability string) time.Time {
	key, ok := e.hostedRecoveryAccount(capability)
	if !ok {
		return time.Time{}
	}
	e.hostedObserverMu.Lock()
	defer e.hostedObserverMu.Unlock()
	return e.hostedAccountNext[key]
}

func (e *Engine) DelayHostedRecoveryAccount(capability string, until time.Time) {
	key, ok := e.hostedRecoveryAccount(capability)
	if !ok || hostedObserverTime(until) == "" {
		return
	}
	e.hostedObserverMu.Lock()
	defer e.hostedObserverMu.Unlock()
	if e.hostedAccountNext == nil {
		e.hostedAccountNext = map[hostedAccountKey]time.Time{}
	}
	if until.After(e.hostedAccountNext[key]) {
		e.hostedAccountNext[key] = until
	}
	for id, entry := range e.hostedObservers {
		account, known := e.hostedRecoveryAccount(id.capability)
		if known && account == key && !entry.stopped && (entry.status.NextCheckAt == "" || until.After(mustWaitTime(entry.status.NextCheckAt))) {
			entry.status.NextCheckAt = hostedObserverTime(e.hostedAccountNext[key])
		}
	}
}

type HostedObserverCheck struct {
	key      hostedObserverKey
	entry    *hostedObserverEntry
	finished bool
}

// BeginHostedObserver is called by the trusted runtime after selecting an
// original attempt. Storage is bounded to the pending admission limit per capability.
func (e *Engine) BeginHostedObserver(capabilityID, project, operation, attempt string) *HostedObserverCheck {
	if e == nil || !identifier.MatchString(project) || !hostedObservationID(operation, "review_operation_") || !hostedObservationID(attempt, "review_attempt_") {
		return nil
	}
	c, ok := e.capabilities[capabilityID]
	if !ok || !slices.Contains(c.Channels, "hosted") {
		return nil
	}
	e.hostedObserverMu.Lock()
	defer e.hostedObserverMu.Unlock()
	if e.hostedObservers == nil {
		e.hostedObservers = make(map[hostedObserverKey]*hostedObserverEntry)
	}
	key := hostedObserverKey{capabilityID, attempt}
	for oldKey, old := range e.hostedObservers {
		if oldKey.capability == capabilityID && old.project == project && old.operation == operation && old.attempt != attempt {
			delete(e.hostedObservers, oldKey)
		}
	}
	entry := e.hostedObservers[key]
	if entry == nil || entry.forget || entry.project != project || entry.operation != operation || entry.attempt != attempt {
		count := 0
		for k := range e.hostedObservers {
			if k.capability == capabilityID {
				count++
			}
		}
		if entry == nil && count >= maxHostedRecoveryCandidates {
			return nil
		}
		entry = &hostedObserverEntry{project: project, operation: operation, attempt: attempt}
		e.hostedObservers[key] = entry
	}
	entry.active++
	entry.stopped = false
	entry.status.State = "checking"
	entry.status.NextCheckAt = ""
	return &HostedObserverCheck{key: key, entry: entry}
}

// FinishHostedObserver records a completed pass. sourceAt is supplied only for
// a validated complete provider collection, before immutable capture dedup.
func (e *Engine) FinishHostedObserver(check *HostedObserverCheck, at, sourceAt time.Time, reason string, failure error) {
	if e == nil || check == nil {
		return
	}
	e.hostedObserverMu.Lock()
	defer e.hostedObserverMu.Unlock()
	if e.hostedObservers[check.key] != check.entry || check.finished {
		return
	}
	check.finished = true
	entry := check.entry
	entry.active--
	if entry.forget && entry.active == 0 {
		delete(e.hostedObservers, check.key)
		return
	}
	if value := hostedObserverTime(at); value != "" {
		entry.status.LastCheckAt = value
	}
	if !sourceAt.IsZero() && entry.status.LastCheckAt != "" {
		entry.status.LastSourceAt = hostedObserverTime(sourceAt)
	}
	entry.status.Reason = hostedObserverReason(reason)
	entry.status.Error = hostedObserverError(failure)
	entry.status.State = "waiting"
	if entry.status.Error != "" {
		entry.status.State = "retrying"
	}
	if entry.stopped {
		entry.status.State = "stopped"
	}
	if entry.active > 0 {
		entry.status.State = "checking"
	}
}

// ScheduleHostedObserver records the actual scheduler's chosen next eligible
// check, not a promised provider start or completion deadline.
func (e *Engine) ScheduleHostedObserver(check *HostedObserverCheck, next time.Time) {
	if e == nil || check == nil {
		return
	}
	e.hostedObserverMu.Lock()
	defer e.hostedObserverMu.Unlock()
	if e.hostedObservers[check.key] == check.entry && !check.entry.stopped {
		if key, known := e.hostedRecoveryAccount(check.key.capability); known && e.hostedAccountNext[key].After(next) {
			next = e.hostedAccountNext[key]
		}
		check.entry.status.NextCheckAt = hostedObserverTime(next)
	}
}

// StopHostedObserver ends this worker's observation intent without erasing its
// last completed check. An overlapping live pass remains visibly checking.
func (e *Engine) StopHostedObserver(check *HostedObserverCheck, failure error) {
	if e == nil || check == nil {
		return
	}
	e.hostedObserverMu.Lock()
	defer e.hostedObserverMu.Unlock()
	if e.hostedObservers[check.key] != check.entry {
		return
	}
	entry := check.entry
	entry.stopped = true
	entry.status.NextCheckAt = ""
	if failure != nil && !errors.Is(failure, context.Canceled) {
		entry.status.Error = hostedObserverError(failure)
	}
	if entry.active == 0 {
		entry.status.State = "stopped"
	}
}

// ForgetHostedObserver retires only this exact ended attempt's local health.
// An overlapping pass may finish, but cannot recreate its retired entry.
func (e *Engine) ForgetHostedObserver(check *HostedObserverCheck) {
	if e == nil || check == nil {
		return
	}
	e.hostedObserverMu.Lock()
	defer e.hostedObserverMu.Unlock()
	if e.hostedObservers[check.key] != check.entry {
		return
	}
	check.entry.forget = true
	if check.entry.active == 0 {
		delete(e.hostedObservers, check.key)
	}
}

func (e *Engine) observerStatus(o Operation) *HostedObserverStatus {
	if e == nil || o.Channel != "hosted" || o.AttemptID == "" || (o.State != "posting" && o.State != "active" && o.State != "uncertain") {
		return nil
	}
	result := &HostedObserverStatus{State: "unknown"}
	e.hostedObserverMu.Lock()
	defer e.hostedObserverMu.Unlock()
	for id, c := range e.capabilities {
		if c.Fingerprint != o.CapabilityFingerprint {
			continue
		}
		entry := e.hostedObservers[hostedObserverKey{id, o.AttemptID}]
		if entry != nil && entry.project == o.ProjectID && entry.operation == o.ID && entry.attempt == o.AttemptID {
			*result = entry.status
			break
		}
	}
	return result
}

func hostedObserverError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "observer_cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "provider_unavailable"
	}
	var provider *reviewprovider.Error
	if errors.As(err, &provider) {
		switch provider.Code {
		case "provider_rate_limited", "provider_unauthorized", "provider_not_found", "provider_invalid_response", "provider_unavailable":
			return provider.Code
		}
	}
	var local *core.Error
	if errors.As(err, &local) && local.Code == "overloaded" {
		return "observer_overloaded"
	}
	return "observer_unavailable"
}

func hostedObserverReason(reason string) string {
	switch reason {
	case "":
		return ""
	case "observation_pending", "already_completed", "trigger_recording_pending", "publication_pending", "hosted_evidence_observed", "settlement_pending", "hosted_evidence_recovered", "observation_unavailable", "hosted_observation_capacity", "source_unavailable", "repository_changed", "observation_incomplete", "trigger_not_proven", "conflicting_trigger_evidence", "incomplete_collection", "exact_trigger_found", "trigger_identity_changed", "publication_not_proven", "invalid_publication_evidence", "hosted_review_rate_limited", "hosted_review_skipped", "conflicting_publication_evidence", "hosted_finished_inventory_unproven", "attribution_window_closed", "missing_publication_batches", "invalid_actionable_count", "root_binding_mismatch", "root_count_mismatch", "publication_inventory_attributed", "comparison_unproven", "standing_baseline_unproven", "standing_run_not_new", "capture_invalid", "finding_inventory_incomplete", "finding_inventory_limit", "evidence_unavailable", "source_changed":
		return reason
	}
	return "observation_unproven"
}

func hostedObserverTime(at time.Time) string {
	at = at.UTC()
	if at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
		return ""
	}
	return at.Format(time.RFC3339Nano)
}
