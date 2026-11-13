package core

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"
)

const (
	workerContactLimit          = 8192
	workerContactPrincipalLimit = 256
	workerContactRetention      = time.Hour
)

type workerContactKey struct{ project, worker, principal, client string }
type workerContactEntry struct {
	lastSeen time.Time
	attached int
}
type workerContactState struct {
	mu                 sync.Mutex
	nextExpiry         time.Time
	since              time.Time
	entries            map[workerContactKey]*workerContactEntry
	principals         map[string]int
	incomplete, closed bool
}

func newWorkerContactState(now time.Time) workerContactState {
	return workerContactState{since: now.UTC(), entries: make(map[workerContactKey]*workerContactEntry), principals: make(map[string]int)}
}

func validClientSession(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func contactKey(scope Scope) (workerContactKey, bool) {
	return workerContactKey{scope.ProjectID, scope.WorkerID, scope.PrincipalID, scope.clientSession}, scope.ProjectID != "" && scope.WorkerID != "" && scope.PrincipalID != "" && scope.sessionProject == "" && !scope.readOnly && validClientSession(scope.clientSession)
}

func (p *workerContactState) remove(key workerContactKey) {
	delete(p.entries, key)
	p.principals[key.principal]--
	if p.principals[key.principal] == 0 {
		delete(p.principals, key.principal)
	}
}
func (p *workerContactState) expire(now time.Time) {
	if p.nextExpiry.After(now) {
		return
	}
	p.nextExpiry = now.Add(workerContactRetention)
	for key, entry := range p.entries {
		if entry.attached == 0 && !entry.lastSeen.After(now.Add(-workerContactRetention)) {
			p.remove(key)
		} else if expires := entry.lastSeen.Add(workerContactRetention); expires.After(now) && expires.Before(p.nextExpiry) {
			p.nextExpiry = expires
		}
	}
}

// Admission only follows successful authentication and current project/worker
// authorization. A capacity refusal never refuses the actual coordination call.
func (s *Store) observeWorkerContact(ctx context.Context, scope Scope) {
	key, ok := contactKey(scope)
	if !ok || ctx.Err() != nil {
		return
	}
	p := &s.workerContact
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	p.expire(now)
	entry := p.entries[key]
	if entry == nil {
		// An exhausted principal must reclaim one of its own inactive keys;
		// reclaiming someone else's would not admit the requested observation.
		if len(p.entries) >= workerContactLimit || p.principals[key.principal] >= workerContactPrincipalLimit {
			var oldest workerContactKey
			var candidate *workerContactEntry
			for existing, e := range p.entries {
				if e.attached != 0 || p.principals[key.principal] >= workerContactPrincipalLimit && existing.principal != key.principal {
					continue
				}
				if candidate == nil || e.lastSeen.Before(candidate.lastSeen) {
					oldest, candidate = existing, e
				}
			}
			if candidate == nil {
				p.incomplete = true
				p.mu.Unlock()
				return
			}
			// Inactive keys contribute only their worker's latest contact, not
			// an exported connection count. Preserve completeness if that same
			// project/worker timestamp remains covered after reclamation.
			covered := oldest.project == key.project && oldest.worker == key.worker && !now.Before(candidate.lastSeen)
			if !covered {
				for existing, retained := range p.entries {
					if existing != oldest && existing.project == oldest.project && existing.worker == oldest.worker && !retained.lastSeen.Before(candidate.lastSeen) {
						covered = true
						break
					}
				}
			}
			if !covered {
				p.incomplete = true
			}
			p.remove(oldest)
		}
		entry = &workerContactEntry{}
		p.entries[key] = entry
		p.principals[key.principal]++
	}
	entry.lastSeen = now
	entry.attached++
	p.mu.Unlock()
	// AfterFunc makes request cancellation nonblocking. Entry identity protects
	// a replacement generation from a delayed callback for an evicted entry.
	context.AfterFunc(ctx, func() { p.release(key, entry) })
}

func (p *workerContactState) release(key workerContactKey, entry *workerContactEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[key] == entry && entry.attached > 0 {
		entry.attached--
		if entry.attached == 0 && entry.lastSeen.Add(workerContactRetention).Before(p.nextExpiry) {
			p.nextExpiry = entry.lastSeen.Add(workerContactRetention)
		}
	}
}

func (s *Store) touchWorkerContact(scope Scope) {
	key, ok := contactKey(scope)
	if !ok {
		return
	}
	p := &s.workerContact
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry := p.entries[key]; entry != nil && entry.attached > 0 {
		entry.lastSeen = time.Now().UTC()
	}
}

func (s *Store) closeWorkerContact() {
	p := &s.workerContact
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	clear(p.entries)
	clear(p.principals)
}

type WorkerPresenceItem struct {
	WorkerID   string     `json:"worker_id"`
	Attached   bool       `json:"attached"`
	LastSeenAt *time.Time `json:"last_seen_at"`
}
type WorkerPresence struct {
	ProjectID     string               `json:"project_id"`
	EvaluatedAt   time.Time            `json:"evaluated_at"`
	ObservedSince time.Time            `json:"observed_since"`
	Complete      bool                 `json:"complete"`
	Items         []WorkerPresenceItem `json:"items"`
}

// ValidateWorkerPresenceIDs is shared by HTTP admission and the owning read.
// IDs are local worker identities, never client/principal connection labels.
func ValidateWorkerPresenceIDs(ids []string) error {
	invalid := Fail("validation_failed", "Select 1-100 distinct valid worker identities.", 400)
	if len(ids) < 1 || len(ids) > 100 {
		return invalid
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !strings.HasPrefix(id, "worker_") || !validClientSession(strings.TrimPrefix(id, "worker_")) || seen[id] {
			return invalid
		}
		seen[id] = true
	}
	return nil
}

// ReadWorkerPresence resolves all requested memberships in one bounded query
// inside one current-authorized read snapshot, before inspecting ephemeral data.
func (s *Store) ReadWorkerPresence(ctx context.Context, scope Scope, ids []string) (WorkerPresence, error) {
	if err := ValidateWorkerPresenceIDs(ids); err != nil {
		return WorkerPresence{}, err
	}
	ids = append([]string(nil), ids...)
	err := s.Read(ctx, scope, func(ctx context.Context, tx *sql.Tx) error {
		args := make([]any, 1, len(ids)+1)
		args[0] = scope.ProjectID
		for _, id := range ids {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, "SELECT id FROM workers WHERE project_id=? AND id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", args...)
		if err != nil {
			return storageError(err)
		}
		defer rows.Close()
		count := 0
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return storageError(err)
			}
			count++
		}
		if err = rows.Err(); err != nil {
			return storageError(err)
		}
		if count != len(ids) {
			return Fail("not_found", "A selected worker is unavailable in this project.", 404)
		}
		return nil
	})
	if err != nil {
		return WorkerPresence{}, err
	}
	p := &s.workerContact
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UTC()
	p.expire(now)
	result := WorkerPresence{ProjectID: scope.ProjectID, EvaluatedAt: now, ObservedSince: p.since, Complete: !p.incomplete, Items: make([]WorkerPresenceItem, len(ids))}
	positions := make(map[string]int, len(ids))
	for i, id := range ids {
		positions[id] = i
		result.Items[i].WorkerID = id
	}
	for key, entry := range p.entries {
		index, selected := positions[key.worker]
		if key.project != scope.ProjectID || !selected {
			continue
		}
		item := &result.Items[index]
		item.Attached = item.Attached || entry.attached > 0
		if item.LastSeenAt == nil || entry.lastSeen.After(*item.LastSeenAt) {
			at := entry.lastSeen
			item.LastSeenAt = &at
		}
	}
	return result, nil
}
