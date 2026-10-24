package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// LifecycleSyncOptions are trusted process configuration, never browser or
// worker instructions. Clock and wait injection make cadence proof deterministic.
type LifecycleSyncOptions struct {
	Now                                                      func() time.Time
	Wait                                                     func(context.Context, time.Duration) error
	OpenInterval, ClosedInterval, MergedInterval, CIInterval time.Duration
	ScanInterval                                             time.Duration
}

func lifecycleOptions(options []LifecycleSyncOptions) (LifecycleSyncOptions, error) {
	c := LifecycleSyncOptions{Now: time.Now, Wait: lifecycleWait, OpenInterval: time.Minute, ClosedInterval: 5 * time.Minute, MergedInterval: 6 * time.Hour, CIInterval: 5 * time.Minute, ScanInterval: time.Second}
	if len(options) > 1 {
		return c, invalid()
	}
	if len(options) == 1 {
		o := options[0]
		if o.Now != nil {
			c.Now = o.Now
		}
		if o.Wait != nil {
			c.Wait = o.Wait
		}
		for _, p := range []struct {
			dst   *time.Duration
			value time.Duration
		}{{&c.OpenInterval, o.OpenInterval}, {&c.ClosedInterval, o.ClosedInterval}, {&c.MergedInterval, o.MergedInterval}, {&c.CIInterval, o.CIInterval}, {&c.ScanInterval, o.ScanInterval}} {
			if p.value < 0 {
				return c, invalid()
			}
			if p.value > 0 {
				*p.dst = p.value
			}
		}
	}
	return c, nil
}

func lifecycleWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type lifecycleKey struct {
	project string
	number  int64
}
type lifecycleDue struct {
	checkedAt, successAt, ciCheckedAt, hostedCheckedAt   time.Time
	ciError, hostedError                                 string
	baseSHA, baseRef                                     string
	lastErrorCode                                        string
	next, ci, enrichmentNext                             time.Time
	head                                                 string
	ciHead, ciBaseSHA, ciBaseRef                         string
	enrichmentHead, enrichmentBaseSHA, enrichmentBaseRef string
	enrichmentError                                      string
}
type lifecycleAccount struct {
	lastErrorCode string
	until         time.Time
	backoff       time.Duration
}
type lifecycleLane uint8

const (
	lifecycleMetadata lifecycleLane = iota
	lifecycleEnrichment
)

type lifecycleLaneSchedule struct {
	lastProject string
	lastPull    map[string]int64
	projects    map[string]bool
	accounts    map[string]bool
}
type lifecycleSchedule struct {
	startedAt time.Time
	claim     chan struct{}
	mu        sync.Mutex
	lanes     [2]lifecycleLaneSchedule
	due       map[lifecycleKey]lifecycleDue
	accounts  map[string]lifecycleAccount
	options   LifecycleSyncOptions
}
type lifecycleTarget struct {
	project, account   string
	repository, number int64
	configuration      Configuration
	fingerprint        string
	pins               map[int64]reviews.SourceMetadataPin
	reader             TargetReader
	due                lifecycleDue
	lane               lifecycleLane
	comparison         lifecycleComparison
}

type lifecycleComparison struct{ head, baseSHA, baseRef, state string }

func newLifecycleSchedule(c LifecycleSyncOptions) *lifecycleSchedule {
	s := &lifecycleSchedule{startedAt: c.Now(), claim: make(chan struct{}, 1), due: map[lifecycleKey]lifecycleDue{}, accounts: map[string]lifecycleAccount{}, options: c}
	for i := range s.lanes {
		s.lanes[i] = lifecycleLaneSchedule{lastPull: map[string]int64{}, projects: map[string]bool{}, accounts: map[string]bool{}}
	}
	return s
}

// StartLifecycleSync observes only already visible, configured PRs. It creates
// no requests, actor receipts, reviews or allowance observations. Review pause
// deliberately does not participate; the daemon retains its restore hold gate.
func (e *Engine) StartLifecycleSync(ctx context.Context, readers map[string]TargetReader, options ...LifecycleSyncOptions) (<-chan error, error) {
	if ctx == nil || e == nil || e.store == nil {
		return nil, invalid()
	}
	c, err := lifecycleOptions(options)
	if err != nil {
		return nil, err
	}
	if len(readers) == 0 || e.publishAttempt == nil {
		return nil, nil
	}
	owned := make(map[string]TargetReader, len(readers))
	for id, r := range readers {
		if _, ok := e.capabilities[id]; !ok || r == nil || reflect.ValueOf(r).Kind() == reflect.Pointer && reflect.ValueOf(r).IsNil() {
			return nil, invalid()
		}
		owned[id] = r
	}
	if !e.lifecycleRunning.CompareAndSwap(false, true) {
		return nil, conflict()
	}
	s := newLifecycleSchedule(c)
	e.lifecycleMu.Lock()
	e.lifecycleSchedule = s
	e.lifecycleMu.Unlock()
	done := make(chan error, 1)
	go func() {
		workerCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		results := make(chan error, 4)
		// Reserve cheap workers: no number of slow initial CI/history reads can
		// occupy all four slots and prevent lifecycle checks from progressing.
		for i := range 4 {
			lane := lifecycleLane(i / 2)
			go func() { results <- e.lifecycleLoop(workerCtx, owned, s, lane) }()
		}
		var first error
		for range 4 {
			if err := <-results; err != nil && first == nil {
				first = err
				cancel()
			}
		}
		e.lifecycleRunning.Store(false)
		done <- first
		close(done)
	}()
	return done, nil
}

func (e *Engine) lifecycleLoop(ctx context.Context, readers map[string]TargetReader, s *lifecycleSchedule, lane lifecycleLane) error {
	for ctx.Err() == nil {
		var target *lifecycleTarget
		err := retrySourceRefreshLocal(ctx, func() error { var err error; target, err = e.claimLifecycleLane(ctx, readers, s, lane); return err })
		if err == nil && target != nil {
			err = e.observeLifecycle(ctx, *target, s)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return core.Fail("lifecycle_sync_persistence_failed", "Automatic source observation stopped because local state is unavailable.", 503)
		}
		if target == nil {
			if err = s.options.Wait(ctx, s.options.ScanInterval); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
	return nil
}

// The claim guard covers bounded local selection and pins only, never provider
// I/O. Project and account reservations prevent duplicate ticks or slot hogging.
func (e *Engine) claimLifecycle(ctx context.Context, readers map[string]TargetReader, s *lifecycleSchedule) (*lifecycleTarget, error) {
	return e.claimLifecycleLane(ctx, readers, s, lifecycleMetadata)
}

func (e *Engine) claimLifecycleLane(ctx context.Context, readers map[string]TargetReader, s *lifecycleSchedule, lane lifecycleLane) (*lifecycleTarget, error) {
	select {
	case s.claim <- struct{}{}:
		defer func() { <-s.claim }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	last := s.lanes[lane].lastProject
	s.mu.Unlock()
	type project struct {
		id                   string
		revision, repository int64
		config               Configuration
	}
	var projects []project
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT p.id,c.revision,c.body_json,p.github_repository_id FROM projects p JOIN review_execution_config c ON c.project_id=p.id AND c.revision=(SELECT MAX(current.revision) FROM review_execution_config current WHERE current.project_id=p.id) ORDER BY (p.id<=?),p.id LIMIT 64`, last)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p project
			var body string
			if err = rows.Scan(&p.id, &p.revision, &body, &p.repository); err != nil {
				return err
			}
			if len(body) > 8192 || strictjson.Decode([]byte(body), &p.config) != nil || validateConfigurationShape(p.config) != nil {
				return refreshStorageError()
			}
			projects = append(projects, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		s.mu.Lock()
		s.lanes[lane].lastProject = p.id
		s.mu.Unlock()
		cap, ok := e.capabilities[p.config.CapabilityID]
		if !ok || readers[p.config.CapabilityID] == nil || e.validateConfiguration(p.config) != nil {
			continue
		}
		if lane == lifecycleEnrichment {
			_, ci := readers[p.config.CapabilityID].(sourceCIReader)
			_, hosted := readers[p.config.CapabilityID].(ExternalHostedReader)
			if !ci && !hosted {
				continue
			}
		}
		account := cap.Provider + "\x00" + cap.Account
		now := s.options.Now()
		s.mu.Lock()
		a := s.accounts[account]
		busy := s.lanes[lane].projects[p.id] || s.lanes[lane].accounts[account] || now.Before(a.until)
		s.mu.Unlock()
		if busy {
			continue
		}
		var numbers []int64
		err = e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
			args := []any{p.id}
			slots := make([]string, len(p.config.Queue))
			for i, n := range p.config.Queue {
				slots[i] = "?"
				args = append(args, n)
			}
			rows, err := tx.QueryContext(ctx, `SELECT c.number FROM review_current_pulls c WHERE c.project_id=? AND c.number IN (`+strings.Join(slots, ",")+`) AND NOT EXISTS (SELECT 1 FROM review_current_retired r WHERE r.project_id=c.project_id AND r.number=c.number) ORDER BY c.position LIMIT 100`, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var n int64
				if err = rows.Scan(&n); err != nil {
					return err
				}
				numbers = append(numbers, n)
			}
			return rows.Err()
		})
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		for k := range s.due {
			if k.project == p.id && !slices.Contains(numbers, k.number) {
				delete(s.due, k)
			}
		}
		start := slices.Index(numbers, s.lanes[lane].lastPull[p.id]) + 1
		var number int64
		var due lifecycleDue
		for offset := range len(numbers) {
			n := numbers[(start+offset)%len(numbers)]
			d := s.due[lifecycleKey{p.id, n}]
			next := d.next
			changed := false
			if lane == lifecycleEnrichment {
				next = d.enrichmentNext
				changed = d.head != "" && (d.head != d.enrichmentHead || d.baseSHA != d.enrichmentBaseSHA || d.baseRef != d.enrichmentBaseRef)
			}
			if changed || !now.Before(next) {
				number, due = n, d
				break
			}
		}
		s.mu.Unlock()
		if number == 0 {
			continue
		}
		var pins map[int64]reviews.SourceMetadataPin
		var comparison lifecycleComparison
		err = e.store.BackgroundWrite(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			pins, err = reviews.PinSourceMetadata(ctx, tx, p.id, []int64{number})
			if err == nil && len(pins) > 0 {
				comparison, err = readLifecycleComparison(ctx, tx, p.id, number)
				if _, ok := readers[p.config.CapabilityID].(ExternalHostedReader); err == nil && ok && lane == lifecycleEnrichment {
					_, err = reviews.InitializeExternalHostedFrontier(ctx, tx, p.id, p.repository, number)
				}
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		if len(pins) == 0 {
			continue
		}
		s.mu.Lock()
		s.lanes[lane].projects[p.id] = true
		s.lanes[lane].accounts[account] = true
		s.lanes[lane].lastPull[p.id] = number
		s.mu.Unlock()
		return &lifecycleTarget{project: p.id, account: account, repository: p.repository, number: number, configuration: p.config, fingerprint: cap.Fingerprint, pins: pins, reader: readers[p.config.CapabilityID], due: due, lane: lane, comparison: comparison}, nil
	}
	return nil, nil
}

func lifecycleInventoryInconclusive(err error) bool {
	var failure *reviewprovider.Error
	return errors.As(err, &failure) && (failure.Code == "external_hosted_identity_unproven" || failure.Code == "provider_invalid_response") && failure.RetryAfterSeconds <= 0
}

func lifecycleProviderDelay(err error, previous time.Duration) time.Duration {
	if err == nil || lifecycleInventoryInconclusive(err) {
		return 0
	}
	delay := min(max(previous*2, 30*time.Second), 5*time.Minute)
	var failure *reviewprovider.Error
	if errors.As(err, &failure) && failure.RetryAfterSeconds > 0 {
		delay = max(delay, time.Duration(min(failure.RetryAfterSeconds, int64(24*time.Hour/time.Second)))*time.Second)
	}
	return delay
}

func (s *lifecycleSchedule) finish(target lifecycleTarget, state, head string, ci, published bool, providerErr error) {
	now := s.options.Now()
	interval := s.options.OpenInterval
	if state == "closed" {
		interval = s.options.ClosedInterval
	}
	if state == "merged" {
		interval = s.options.MergedInterval
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[target.account]
	delete(s.lanes[target.lane].projects, target.project)
	delete(s.lanes[target.lane].accounts, target.account)
	errorCode := ""
	if providerErr != nil {
		errorCode = "provider_unavailable"
		var failure *reviewprovider.Error
		if errors.As(providerErr, &failure) {
			errorCode = failure.Code
		}
	}
	if lifecycleInventoryInconclusive(providerErr) {
		// Unsupported or malformed inventory is retried at this PR's normal cadence. It is
		// not evidence that the provider account should stop serving other targets.
		// An independently established provider cooldown still takes precedence.
		if !now.Before(a.until) {
			a.lastErrorCode = ""
			a.backoff = 0
			a.until = time.Time{}
		}
	} else if providerErr != nil {
		a.lastErrorCode = errorCode
		a.backoff = lifecycleProviderDelay(providerErr, a.backoff)
		a.until = maxObservationTime(a.until, now.Add(a.backoff))
		interval = max(interval, a.backoff)
	} else if !now.Before(a.until) {
		a.lastErrorCode = ""
		a.backoff = 0
		a.until = time.Time{}
	}
	s.accounts[target.account] = a
	// Merge only fields owned by this lane. The other lane may have completed
	// after this target was claimed; copying its old due snapshot would lose it.
	key := lifecycleKey{target.project, target.number}
	due := s.due[key]
	if target.lane == lifecycleMetadata {
		due.checkedAt, due.lastErrorCode, due.next = now, errorCode, now.Add(interval)
		if published {
			due.successAt = target.due.successAt
			due.head, due.baseSHA, due.baseRef = head, target.due.baseSHA, target.due.baseRef
		}
	} else {
		due.enrichmentNext, due.enrichmentError = now.Add(interval), errorCode
		if published && head != "" {
			due.enrichmentHead, due.enrichmentBaseSHA, due.enrichmentBaseRef = head, target.due.baseSHA, target.due.baseRef
		}
		if published && ci {
			due.ci = now.Add(s.options.CIInterval)
			due.ciHead, due.ciBaseSHA, due.ciBaseRef = head, target.due.baseSHA, target.due.baseRef
			due.ciCheckedAt, due.ciError = target.due.ciCheckedAt, target.due.ciError
		}
		if published && !target.due.hostedCheckedAt.IsZero() {
			due.hostedCheckedAt, due.hostedError = target.due.hostedCheckedAt, target.due.hostedError
		}
	}
	s.due[key] = due
}

func readLifecycleComparison(ctx context.Context, tx *sql.Tx, project string, number int64) (lifecycleComparison, error) {
	var c lifecycleComparison
	err := tx.QueryRowContext(ctx, `SELECT v.head_sha,v.base_sha,v.base_ref,v.state FROM review_current_pulls p JOIN review_pull_memberships m ON m.project_id=p.project_id AND m.number=p.number AND m.start_revision=p.start_revision JOIN review_pull_versions v ON v.project_id=m.project_id AND v.number=m.number AND v.version_revision=m.version_revision WHERE p.project_id=? AND p.number=?`, project, number).Scan(&c.head, &c.baseSHA, &c.baseRef, &c.state)
	return c, err
}
