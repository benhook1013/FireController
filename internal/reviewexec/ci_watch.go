package reviewexec

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

type CIWatchTarget struct {
	PullNumber int64  `json:"pull_number"`
	HeadSHA    string `json:"head_sha"`
	BaseSHA    string `json:"base_sha,omitempty"`
}

type CIWatchItem struct {
	CIWatchTarget
	State           string            `json:"state"`
	Reason          string            `json:"reason"`
	BaseRef         string            `json:"base_ref,omitempty"`
	GateRef         string            `json:"gate_ref,omitempty"`
	ObservedHeadSHA string            `json:"observed_head_sha,omitempty"`
	ObservedBaseSHA string            `json:"observed_base_sha,omitempty"`
	ObservedAt      string            `json:"observed_at,omitempty"`
	CICommitSHA     string            `json:"ci_commit_sha,omitempty"`
	Selection       string            `json:"selection,omitempty"`
	Required        int64             `json:"required"`
	Satisfied       int64             `json:"satisfied"`
	Failed          int64             `json:"failed"`
	Pending         int64             `json:"pending"`
	Unproven        int64             `json:"unproven"`
	Checks          []SourceCIContext `json:"checks"`
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
	scope   core.Scope
	changed chan struct{}
	targets []CIWatchTarget
	entries []*ciWatchEntry
	minimum []uint64
	bases   []string
}

type ciWatchEntry struct {
	key               ciWatchKey
	owners            map[*ciWatchSubscription]bool
	wanted, done      uint64
	started, version  uint64
	lastAttempt, next time.Time
	inflight          bool
	cancel            context.CancelFunc
	result            CIWatchItem
}

func validCIWatchTargets(targets []CIWatchTarget) bool {
	if len(targets) < 1 || len(targets) > 20 {
		return false
	}
	seen := map[int64]bool{}
	for _, target := range targets {
		if target.PullNumber < 1 || target.PullNumber > 2147483647 || seen[target.PullNumber] || !sha.MatchString(target.HeadSHA) || target.BaseSHA != "" && !sha.MatchString(target.BaseSHA) {
			return false
		}
		seen[target.PullNumber] = true
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
	if ctx == nil || e == nil || e.store == nil || !validCIWatchTargets(targets) || waitSeconds < 0 || waitSeconds > 900 {
		return CIWatchResult{}, invalid()
	}
	ctx, cleanup := e.resultWaitContext(ctx)
	defer cleanup()
	binding, err := e.ciWatchBinding(ctx, scope)
	if err != nil {
		return CIWatchResult{}, err
	}
	e.lifecycleMu.RLock()
	s := e.lifecycleSchedule
	e.lifecycleMu.RUnlock()
	if s == nil || !e.lifecycleRunning.Load() {
		return CIWatchResult{}, ciWatchUnavailable()
	}
	sub, err := s.registerCIWatch(scope, binding, targets)
	if err != nil {
		return CIWatchResult{}, err
	}
	defer s.unregisterCIWatch(sub)
	timer := time.NewTimer(time.Duration(waitSeconds) * time.Second)
	defer timer.Stop()
	for {
		fresh, err := e.ciWatchBinding(ctx, scope)
		if err != nil {
			return CIWatchResult{}, err
		}
		if fresh != binding {
			return CIWatchResult{}, core.Fail("ci_watch_binding_changed", "The CI watch project source binding changed; start a fresh watch with current targets.", 409)
		}
		if !e.lifecycleRunning.Load() {
			return CIWatchResult{}, ciWatchUnavailable()
		}
		value := s.ciWatchResult(sub)
		if value.State != "pending" {
			return boundedCIWatchResult(value)
		}
		select {
		case <-ctx.Done():
			return CIWatchResult{}, resultWaitCanceled(ctx, "The CI watch was canceled; its temporary subscriptions were released.")
		case <-timer.C:
			// Reauthorize after a quiet wait, just as after a source notification.
			fresh, err := e.ciWatchBinding(ctx, scope)
			if err != nil {
				return CIWatchResult{}, err
			}
			if fresh != binding {
				return CIWatchResult{}, core.Fail("ci_watch_binding_changed", "The CI watch project source binding changed; start a fresh watch with current targets.", 409)
			}
			if !e.lifecycleRunning.Load() {
				return CIWatchResult{}, ciWatchUnavailable()
			}
			value = s.ciWatchResult(sub)
			value.TimedOut = value.State == "pending"
			return boundedCIWatchResult(value)
		case <-sub.changed:
		}
	}
}

func boundedCIWatchResult(value CIWatchResult) (CIWatchResult, error) {
	body, err := json.Marshal(value)
	if err != nil || len(body) > 512<<10 {
		return CIWatchResult{}, core.Fail("ci_watch_response_limit", "The required CI context details exceed the bounded watch response; watch fewer targets.", 503)
	}
	return value, nil
}

func ciWatchUnavailable() error {
	return core.Fail("overloaded", "CI watch observation is unavailable or at capacity; no review or queue change was requested.", 503)
}
