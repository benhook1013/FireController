package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type CIWatchTarget struct {
	AllowPassedRearm         bool    `json:"-"`
	AllowHeadAttention       bool    `json:"-"`
	AllowHistoricalAttention bool    `json:"-"`
	PullNumber               int64   `json:"pull_number"`
	HeadSHA                  string  `json:"head_sha"`
	BaseSHA                  string  `json:"base_sha,omitempty"`
	AfterObservationKey      string  `json:"-"`
	FailureWorkflowIDs       []int64 `json:"-"`
}

type CIWatchItem struct {
	CIWatchTarget
	WorkflowAttentionBasis string                              `json:"workflow_attention_basis,omitempty"`
	MergeBaseMismatch      *reviewprovider.CIMergeBaseMismatch `json:"merge_base_mismatch,omitempty"`
	State                  string                              `json:"state"`
	Reason                 string                              `json:"reason"`
	BaseRef                string                              `json:"base_ref,omitempty"`
	GateRef                string                              `json:"gate_ref,omitempty"`
	ObservedHeadSHA        string                              `json:"observed_head_sha,omitempty"`
	ObservedBaseSHA        string                              `json:"observed_base_sha,omitempty"`
	ObservedAt             string                              `json:"observed_at,omitempty"`
	CICommitSHA            string                              `json:"ci_commit_sha,omitempty"`
	Selection              string                              `json:"selection,omitempty"`
	Required               int64                               `json:"required"`
	Satisfied              int64                               `json:"satisfied"`
	Failed                 int64                               `json:"failed"`
	Pending                int64                               `json:"pending"`
	Unproven               int64                               `json:"unproven"`
	Checks                 []SourceCIContext                   `json:"checks"`
	ObservationKey         string                              `json:"observation_key,omitempty"`
	WaitingForChange       bool                                `json:"waiting_for_change,omitempty"`
	WorkflowFailures       []reviewprovider.CIWorkflowFailure  `json:"workflow_failures,omitempty"`
	WorkflowObservations   []CIWorkflowObservation             `json:"workflow_observations,omitempty"`
}

type CIWatchResult struct {
	ProjectID string        `json:"project_id"`
	State     string        `json:"state"`
	TimedOut  bool          `json:"timed_out"`
	Targets   []CIWatchItem `json:"targets"`
}

type ciWatchBinding struct {
	project, capability, fingerprint, account string
	repository                                int64
}

type ciWatchKey struct {
	binding ciWatchBinding
	number  int64
}

type ciWatchSubscription struct {
	scope      core.Scope
	changed    chan struct{}
	targets    []CIWatchTarget
	entries    []*ciWatchEntry
	minimum    []uint64
	bases      []string
	superseded []*CIWatchItem
}

type ciWatchEntry struct {
	key                          ciWatchKey
	owners                       map[*ciWatchSubscription]bool
	wanted, done                 uint64
	metadataWanted, metadataDone uint64
	started, version             uint64
	lastAttempt, next            time.Time
	inflight                     bool
	cancel                       context.CancelFunc
	// Unknown SHA-only hints are retained only for the current bounded read.
	inflightSHAHints    []string
	inflightSHAOverflow bool
	result              CIWatchItem
}

func validCIWatchTargets(targets []CIWatchTarget) bool {
	if len(targets) < 1 || len(targets) > 20 {
		return false
	}
	seen := map[int64]bool{}
	for _, target := range targets {
		if target.PullNumber < 1 || target.PullNumber > 2147483647 || seen[target.PullNumber] || !sha.MatchString(target.HeadSHA) || target.BaseSHA != "" && !sha.MatchString(target.BaseSHA) || target.AfterObservationKey != "" && !hash.MatchString(target.AfterObservationKey) {
			return false
		}
		seen[target.PullNumber] = true
		if !validCIWatchWorkflowIDs(target.FailureWorkflowIDs) {
			return false
		}
	}
	return true
}

func validCIWatchWorkflowIDs(ids []int64) bool {
	if len(ids) > 16 {
		return false
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func (e *Engine) ciWatchBinding(ctx context.Context, scope core.Scope) (ciWatchBinding, error) {
	var binding ciWatchBinding
	err := e.store.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		c, _, err := readConfiguration(ctx, tx, scope.ProjectID)
		if err != nil {
			return err
		}
		cap, ok := e.capabilities[c.CapabilityID]
		if !ok {
			return ciWatchUnavailable()
		}
		binding = ciWatchBinding{project: scope.ProjectID, capability: c.CapabilityID, fingerprint: cap.Fingerprint, account: cap.Provider + "\x00" + cap.Account}
		return tx.QueryRowContext(ctx, `SELECT github_repository_id FROM projects WHERE id=?`, scope.ProjectID).Scan(&binding.repository)
	})
	if err == nil && binding.repository <= 0 {
		err = ciWatchUnavailable()
	}
	return binding, err
}

// WatchCI is an authorized read with a temporary acquisition subscription. It
// never enrolls PRs, changes review controls or retains a watch after return.
func (e *Engine) WatchCI(ctx context.Context, scope core.Scope, targets []CIWatchTarget, waitSeconds int) (CIWatchResult, error) {
	return e.watchCIFrom(ctx, scope, targets, waitSeconds, time.Now())
}

// The HTTP entry point includes scoped authentication and body parsing in its
// selected wait. Direct callers start the same bounded wait at invocation.
func (e *Engine) watchCIFrom(ctx context.Context, scope core.Scope, targets []CIWatchTarget, waitSeconds int, started time.Time) (CIWatchResult, error) {
	if ctx == nil || e == nil || e.store == nil || !validCIWatchTargets(targets) || waitSeconds < 0 || waitSeconds > 900 {
		return CIWatchResult{}, invalid()
	}
	var deadline time.Time
	if waitSeconds > 0 {
		deadline = started.Add(time.Duration(waitSeconds) * time.Second)
	}
	ctx, cleanup := e.resultWaitContext(ctx)
	defer cleanup()
	readBinding := func() (ciWatchBinding, error) {
		var value ciWatchBinding
		err := core.RetryReadAdmission(ctx, deadline, func(readCtx context.Context) error {
			var err error
			value, err = e.ciWatchBinding(readCtx, scope)
			return err
		})
		if ctx.Err() != nil {
			return ciWatchBinding{}, ciWatchRefusal(resultWaitCanceled(ctx, "The CI watch was canceled; its temporary subscriptions were released."), "ci_watch_canceled")
		}
		return value, ciWatchRefusal(err, "ci_watch_binding_read")
	}
	binding, err := readBinding()
	if err != nil {
		return CIWatchResult{}, err
	}
	e.lifecycleMu.RLock()
	s := e.lifecycleSchedule
	if s == nil || !e.lifecycleRunning.Load() {
		e.lifecycleMu.RUnlock()
		return CIWatchResult{}, ciWatchUnavailable()
	}
	// Registration and the running schedule identity share one lifetime fence.
	sub, err := s.registerCIWatch(scope, binding, targets)
	e.lifecycleMu.RUnlock()
	if err != nil {
		return CIWatchResult{}, err
	}
	defer s.unregisterCIWatch(sub)
	remaining := time.Duration(0)
	if waitSeconds > 0 {
		remaining = time.Until(deadline)
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	for {
		fresh, err := readBinding()
		if err != nil {
			return CIWatchResult{}, err
		}
		if fresh != binding {
			return CIWatchResult{}, core.Fail("ci_watch_binding_changed", "The CI watch project source binding changed; start a fresh watch with current targets.", 409)
		}
		value, err := e.currentCIWatchResult(s, sub)
		if err != nil {
			return CIWatchResult{}, err
		}
		if value.State != "pending" {
			return boundedCIWatchResult(value)
		}
		if waitSeconds == 0 {
			// This snapshot was just authorized; no quiet wait occurred that
			// would require a third binding read within the response allowance.
			value.TimedOut = true
			return boundedCIWatchResult(value)
		}
		select {
		case <-ctx.Done():
			return CIWatchResult{}, ciWatchRefusal(resultWaitCanceled(ctx, "The CI watch was canceled; its temporary subscriptions were released."), "ci_watch_canceled")
		case <-timer.C:
			// Reauthorize after a quiet wait, just as after a source notification.
			fresh, err := readBinding()
			if err != nil {
				return CIWatchResult{}, err
			}
			if fresh != binding {
				return CIWatchResult{}, core.Fail("ci_watch_binding_changed", "The CI watch project source binding changed; start a fresh watch with current targets.", 409)
			}
			value, err = e.currentCIWatchResult(s, sub)
			if err != nil {
				return CIWatchResult{}, err
			}
			value.TimedOut = value.State == "pending"
			return boundedCIWatchResult(value)
		case <-sub.changed:
		}
	}
}

// A restarted observer cannot lend its running flag to an old subscription.
// Hold the lifetime fence through the snapshot, including terminal/expiry reads.
func (e *Engine) currentCIWatchResult(s *lifecycleSchedule, sub *ciWatchSubscription) (CIWatchResult, error) {
	e.lifecycleMu.RLock()
	defer e.lifecycleMu.RUnlock()
	if e.lifecycleSchedule != s || !e.lifecycleRunning.Load() {
		return CIWatchResult{}, ciWatchUnavailable()
	}
	return s.ciWatchResult(sub), nil
}

func boundedCIWatchResult(value CIWatchResult) (CIWatchResult, error) {
	body, err := json.Marshal(value)
	if err != nil || len(body) > 512<<10 {
		return CIWatchResult{}, core.Fail("ci_watch_response_limit", "The required CI context details exceed the bounded watch response; watch fewer targets.", 503)
	}
	return value, nil
}

// Annotate only operational refusals after read-admission retry has finished.
// Reasons name a finite local stage, never the underlying storage/provider cause.
func ciWatchRefusal(err error, reason string) error {
	var failure *core.Error
	if !errors.As(err, &failure) || failure.Code != "overloaded" && failure.Code != "storage_unavailable" {
		return err
	}
	details := map[string]any{}
	switch previous := failure.Details.(type) {
	case nil:
	case map[string]string:
		if _, exists := previous["reason"]; exists {
			return err
		}
		for key, value := range previous {
			details[key] = value
		}
	case map[string]any:
		if _, exists := previous["reason"]; exists {
			return err
		}
		for key, value := range previous {
			details[key] = value
		}
	default:
		return err
	}
	copy := *failure
	details["reason"] = reason
	copy.Details = details
	return &copy
}

func ciWatchUnavailable() error {
	return ciWatchRefusal(core.Fail("overloaded", "The configured CI observer is unavailable; no review or queue change was requested.", 503), "ci_watch_observer_unavailable")
}

func ciWatchCapacity() error {
	return ciWatchRefusal(core.Fail("overloaded", "The bounded CI watch capacity is occupied; no extra observation was started.", 503), "ci_watch_capacity")
}
