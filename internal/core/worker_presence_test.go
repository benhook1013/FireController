package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func presenceID(n int) string { return fmt.Sprintf("worker_%032x", n) }
func presenceRequest(ctx context.Context, worker, session string) *http.Request {
	r := httptest.NewRequest("GET", "http://synthetic/", nil).WithContext(ctx)
	r.SetPathValue("project_id", "project")
	r.Header.Set("Authorization", "Bearer synthetic-core-stream-token-1234567")
	if worker != "" {
		r.Header.Set("X-FC-Worker", worker)
	}
	if session != "" {
		r.Header.Set("X-FC-Session", session)
	}
	return r
}
func workerPresenceFixture(t *testing.T) (*Store, Scope) {
	t.Helper()
	s, _, _ := streamFixture(t)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"UPDATE workers SET id=?", []any{presenceID(1)}},
		{"INSERT INTO workers VALUES('project',?,'second')", []any{presenceID(2)}},
		{"INSERT INTO projects VALUES('other','other',2,'synthetic/other')", nil},
		{"INSERT INTO workers VALUES('other',?,'other')", []any{presenceID(1)}},
		{"INSERT INTO grants VALUES('credential','other',1,1)", nil},
	} {
		if _, err := s.DB().Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := s.Scope(presenceRequest(context.Background(), "", ""), "read")
	if err != nil {
		t.Fatal(err)
	}
	return s, scope
}
func presenceRead(t *testing.T, s *Store, scope Scope, ids ...string) WorkerPresence {
	t.Helper()
	v, err := s.ReadWorkerPresence(context.Background(), scope, ids)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func presenceEventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("contact callback did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}
func presenceAttached(s *Store) int {
	p := &s.workerContact
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, e := range p.entries {
		n += e.attached
	}
	return n
}

// Entire durable fixture snapshot, not just counters: contact must not create
// receipts/events or alter identities, grants, messages or acknowledgments.
func presenceDurableHash(t *testing.T, s *Store) [32]byte {
	t.Helper()
	rows, err := s.DB().Query("SELECT name,sql FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var records []any
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		records = append(records, []string{name, ddl})
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for _, name := range names {
		rows, err := s.DB().Query(`SELECT * FROM "` + name + `" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			args := make([]any, len(columns))
			for i := range values {
				args[i] = &values[i]
			}
			if err := rows.Scan(args...); err != nil {
				t.Fatal(err)
			}
			records = append(records, []any{name, values})
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func TestWorkerPresenceOverlappingLifetimesIsolationAndNoDurableWrites(t *testing.T) {
	s, reader := workerPresenceFixture(t)
	before := presenceDurableHash(t, s)
	initial := presenceRead(t, s, reader, presenceID(2), presenceID(1))
	if !initial.Complete || initial.ObservedSince.IsZero() || initial.EvaluatedAt.Before(initial.ObservedSince) || initial.Items[0].WorkerID != presenceID(2) || initial.Items[1].LastSeenAt != nil {
		t.Fatal("initial unknown/order/window")
	}
	c1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	c2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	c3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	for i, ctx := range []context.Context{c1, c2, c3} {
		session := strings.Repeat("a", 32)
		if i == 2 {
			session = strings.Repeat("b", 32)
		}
		if _, err := s.Scope(presenceRequest(ctx, presenceID(1), session), "read"); err != nil {
			t.Fatal(err)
		}
	}
	v := presenceRead(t, s, reader, presenceID(2), presenceID(1))
	if v.Items[0].Attached || v.Items[0].LastSeenAt != nil || !v.Items[1].Attached || v.Items[1].LastSeenAt == nil || presenceAttached(s) != 3 {
		t.Fatal("overlap/contact attribution")
	}
	other := reader
	other.ProjectID = "other"
	if presenceRead(t, s, other, presenceID(1)).Items[0].LastSeenAt != nil {
		t.Fatal("cross-project leak")
	}
	raw, _ := json.Marshal(v)
	for _, secret := range []string{"principal", strings.Repeat("a", 32), strings.Repeat("b", 32), reader.tokenHash} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("private contact attribution leaked")
		}
	}
	cancel1()
	presenceEventually(t, func() bool { return presenceAttached(s) == 2 })
	if !presenceRead(t, s, reader, presenceID(1)).Items[0].Attached {
		t.Fatal("one cancel removed overlapping client")
	}
	cancel2()
	presenceEventually(t, func() bool { return presenceAttached(s) == 1 })
	cancel3()
	presenceEventually(t, func() bool { return presenceAttached(s) == 0 })
	last := presenceRead(t, s, reader, presenceID(1))
	if last.Items[0].Attached || !last.Items[0].LastSeenAt.Equal(*v.Items[1].LastSeenAt) {
		t.Fatal("cancellation fabricated contact or lost history")
	}
	if before != presenceDurableHash(t, s) {
		t.Fatal("contact/read changed durable state")
	}
}

func TestWorkerPresenceHeadersAuthorizationAndBrowserExclusion(t *testing.T) {
	s, reader := workerPresenceFixture(t)
	before := presenceDurableHash(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Scope(presenceRequest(ctx, presenceID(1), ""), "read"); err != nil {
		t.Fatal(err)
	}
	for _, values := range [][]string{{""}, {"a"}, {strings.Repeat("A", 32)}, {strings.Repeat("g", 32)}, {strings.Repeat("a", 32) + "," + strings.Repeat("b", 32)}, {strings.Repeat("a", 32), strings.Repeat("a", 32)}} {
		r := presenceRequest(ctx, presenceID(1), "")
		r.Header["X-Fc-Session"] = values
		_, err := s.Scope(r, "read")
		browserCoreCode(t, err, "validation_failed")
	}
	invalid := presenceRequest(ctx, presenceID(1), strings.Repeat("a", 32))
	invalid.Header.Set("Authorization", "Bearer unknown")
	_, err := s.Scope(invalid, "read")
	browserCoreCode(t, err, "unauthorized")
	missing := presenceRequest(ctx, presenceID(3), strings.Repeat("a", 32))
	_, err = s.Scope(missing, "read")
	browserCoreCode(t, err, "not_found")
	if len(s.workerContact.entries) != 0 || presenceRead(t, s, reader, presenceID(1)).Items[0].LastSeenAt != nil {
		t.Fatal("old/invalid requests claimed coverage")
	}
	if before != presenceDurableHash(t, s) {
		t.Fatal("invalid requests changed durable state")
	}
	// Existing browser fixture chooses worker 'worker'; select our canonical worker.
	id, cookie := browserCoreSession(t, s)
	r := browserCoreRequest(s, id, cookie)
	r.Header.Set("X-FC-Worker", presenceID(1))
	browserScope, err := s.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadWorkerPresence(context.Background(), browserScope, []string{presenceID(1)}); err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-FC-Session", strings.Repeat("a", 32))
	_, err = s.Scope(r, "read")
	browserCoreCode(t, err, "validation_failed")
	if len(s.workerContact.entries) != 0 {
		t.Fatal("browser claimed contact")
	}
	// Current read authorization is rechecked, independent of cached Scope.
	if _, err = s.DB().Exec("UPDATE grants SET can_read=0 WHERE credential_id='credential'"); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReadWorkerPresence(context.Background(), reader, []string{presenceID(1)})
	browserCoreCode(t, err, "forbidden")
	if _, err = s.DB().Exec("UPDATE credentials SET revoked=1 WHERE id='credential'"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Scope(presenceRequest(ctx, presenceID(1), strings.Repeat("a", 32)), "read")
	browserCoreCode(t, err, "unauthorized")
	_, err = s.ReadWorkerPresence(context.Background(), reader, []string{presenceID(1)})
	browserCoreCode(t, err, "unauthorized")
}

func TestWorkerPresenceReadBoundsAndCurrentMembership(t *testing.T) {
	s, reader := workerPresenceFixture(t)
	for _, ids := range [][]string{nil, {}, {""}, {"worker"}, {" worker_" + strings.Repeat("a", 32)}, {"worker_" + strings.Repeat("A", 32)}, {presenceID(1), presenceID(1)}, make([]string, 101)} {
		_, err := s.ReadWorkerPresence(context.Background(), reader, ids)
		browserCoreCode(t, err, "validation_failed")
	}
	_, err := s.ReadWorkerPresence(context.Background(), reader, []string{presenceID(99)})
	browserCoreCode(t, err, "not_found")
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = presenceID(i + 100)
		if _, err = s.DB().Exec("INSERT INTO workers VALUES('project',?,?)", ids[i], fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	before := presenceDurableHash(t, s)
	v := presenceRead(t, s, reader, ids...)
	if len(v.Items) != 100 || v.Items[99].WorkerID != ids[99] || before != presenceDurableHash(t, s) {
		t.Fatal("maximum bound/read-only order")
	}
}

func TestWorkerPresenceQuietStreamHeartbeatAndRevocation(t *testing.T) {
	s, reader := workerPresenceFixture(t)
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()
	authCtx, cancelAuth := context.WithCancel(streamCtx)
	scope, err := s.ScopeWithLifetime(presenceRequest(authCtx, presenceID(1), strings.Repeat("c", 32)), "read", streamCtx)
	if err != nil {
		t.Fatal(err)
	}
	cancelAuth()
	initial := presenceRead(t, s, reader, presenceID(1))
	if !initial.Items[0].Attached {
		t.Fatal("auth context cancellation ended stream lease")
	}
	before := presenceDurableHash(t, s)
	heartbeats := 0
	cursor := s.Cursor(scope, messageFeed+scope.WorkerID, "0")
	err = s.streamMessageEvents(streamCtx, scope, cursor, 1, func(EventPage) error { return nil }, func() error {
		heartbeats++
		if heartbeats == 2 {
			now := presenceRead(t, s, reader, presenceID(1))
			if !now.Items[0].Attached || !now.Items[0].LastSeenAt.After(*initial.Items[0].LastSeenAt) {
				t.Error("successful authorized heartbeat did not maintain contact")
			}
			if before != presenceDurableHash(t, s) {
				t.Error("quiet wait/heartbeat changed durable state")
			}
			_, err := s.DB().Exec("UPDATE grants SET can_read=0 WHERE credential_id='credential'")
			return err
		}
		return nil
	}, 5*time.Millisecond)
	browserCoreCode(t, err, "forbidden")
	if heartbeats != 2 {
		t.Fatal("revoked stream remained open")
	}
	cancelStream()
	presenceEventually(t, func() bool { return presenceAttached(s) == 0 })
}

func completedPresenceContact(t *testing.T, s *Store, project, worker string, client int) (workerContactKey, time.Time) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := presenceRequest(ctx, worker, fmt.Sprintf("%032x", client))
	req.SetPathValue("project_id", project)
	scope, err := s.Scope(req, "read")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := contactKey(scope)
	p := &s.workerContact
	p.mu.Lock()
	entry := p.entries[key]
	if entry == nil {
		p.mu.Unlock()
		t.Fatal("completed request contact was not admitted")
	}
	at := entry.lastSeen
	p.mu.Unlock()
	cancel()
	presenceEventually(t, func() bool { return presenceAttached(s) == 0 })
	return key, at
}

func TestWorkerPresenceCompletedClientChurnPreservesLatestContact(t *testing.T) {
	s, reader := workerPresenceFixture(t)
	before := presenceDurableHash(t, s)
	var latest time.Time
	for i := 0; i < workerContactPrincipalLimit+32; i++ {
		_, latest = completedPresenceContact(t, s, "project", presenceID(1), i)
	}
	v := presenceRead(t, s, reader, presenceID(1))
	if !v.Complete || v.Items[0].Attached || v.Items[0].LastSeenAt == nil || !v.Items[0].LastSeenAt.Equal(latest) {
		t.Fatal("completed CLI key churn lost contact coverage or latest timestamp")
	}
	if len(s.workerContact.entries) != workerContactPrincipalLimit || s.workerContact.principals[reader.PrincipalID] != workerContactPrincipalLimit {
		t.Fatal("completed CLI key churn exceeded the principal budget")
	}
	if before != presenceDurableHash(t, s) {
		t.Fatal("completed CLI key churn changed durable state")
	}
}

func TestWorkerPresenceEvictionRequiresRetainedWorkerCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, otherProject string
		retainedSameWorker bool
		equalTimestamp     bool
	}{
		{name: "newer retained observation", otherProject: "project", retainedSameWorker: true},
		{name: "equal retained observation", otherProject: "project", retainedSameWorker: true, equalTimestamp: true},
		{name: "newest contact for another worker is lost", otherProject: "project"},
		{name: "same worker ID in another project cannot cover", otherProject: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, reader := workerPresenceFixture(t)
			before := presenceDurableHash(t, s)
			oldest, _ := completedPresenceContact(t, s, "project", presenceID(1), 0)
			incomingWorker := presenceID(2)
			if tc.otherProject == "other" {
				incomingWorker = presenceID(1)
			}
			var covering workerContactKey
			for i := 1; i < workerContactPrincipalLimit; i++ {
				worker := incomingWorker
				if tc.retainedSameWorker && i == 1 {
					worker = presenceID(1)
				}
				key, _ := completedPresenceContact(t, s, tc.otherProject, worker, i)
				if i == 1 {
					covering = key
				}
			}
			p := &s.workerContact
			p.mu.Lock()
			at := time.Now().UTC().Add(-30 * time.Minute)
			p.entries[oldest].lastSeen = at
			if tc.retainedSameWorker {
				p.entries[covering].lastSeen = at
				if !tc.equalTimestamp {
					p.entries[covering].lastSeen = at.Add(time.Minute)
				}
			}
			p.mu.Unlock()
			prior := presenceRead(t, s, reader, presenceID(1))
			completedPresenceContact(t, s, tc.otherProject, incomingWorker, workerContactPrincipalLimit)
			v := presenceRead(t, s, reader, presenceID(1))
			if v.Complete != tc.retainedSameWorker || v.Items[0].Attached {
				t.Fatal("eviction completeness did not match exact worker/project coverage")
			}
			if tc.retainedSameWorker {
				if v.Items[0].LastSeenAt == nil || !v.Items[0].LastSeenAt.Equal(*prior.Items[0].LastSeenAt) {
					t.Fatal("covered eviction changed the exported latest contact")
				}
			} else if v.Items[0].LastSeenAt != nil {
				t.Fatal("unrelated worker or project fabricated retained contact")
			}
			nextWorker := incomingWorker
			if tc.retainedSameWorker {
				nextWorker = presenceID(1)
			}
			completedPresenceContact(t, s, tc.otherProject, nextWorker, workerContactPrincipalLimit+1)
			if presenceRead(t, s, reader, presenceID(1)).Complete != tc.retainedSameWorker {
				t.Fatal("later covered admission forgot a genuine sticky gap")
			}
			if len(p.entries) != workerContactPrincipalLimit || p.principals[reader.PrincipalID] != workerContactPrincipalLimit || before != presenceDurableHash(t, s) {
				t.Fatal("eviction changed memory bounds or durable state")
			}
		})
	}
}

func TestWorkerPresencePrincipalCapacityEvictionExpiryAndGenerations(t *testing.T) {
	s, reader := workerPresenceFixture(t)
	p := &s.workerContact
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// In-memory admission unit exercises exact production bounds without SQL or
	// forged HTTP credentials: scope originates from the authorized fixture.
	scope := reader
	scope.WorkerID = presenceID(1)
	for i := 0; i < workerContactPrincipalLimit; i++ {
		scope.clientSession = fmt.Sprintf("%032x", i)
		s.observeWorkerContact(ctx, scope)
	}
	if len(p.entries) != 256 || p.principals[reader.PrincipalID] != 256 {
		t.Fatal("principal bound")
	}
	scope.clientSession = fmt.Sprintf("%032x", 256)
	s.observeWorkerContact(ctx, scope)
	if len(p.entries) != 256 || presenceRead(t, s, reader, presenceID(1)).Complete {
		t.Fatal("active eviction or false complete")
	}
	// Another principal with identical connection labels remains independent.
	scope.PrincipalID = "second-principal"
	s.observeWorkerContact(ctx, scope)
	if len(p.entries) != 257 || p.principals[scope.PrincipalID] != 1 {
		t.Fatal("principal isolation")
	}
	cancel()
	presenceEventually(t, func() bool { return presenceAttached(s) == 0 })
	p.mu.Lock()
	oldKey := workerContactKey{"project", presenceID(1), reader.PrincipalID, fmt.Sprintf("%032x", 0)}
	old := p.entries[oldKey]
	old.lastSeen = time.Now().Add(-30 * time.Minute)
	p.mu.Unlock()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	scope.PrincipalID = reader.PrincipalID
	s.observeWorkerContact(ctx2, scope)
	p.mu.Lock()
	_, oldRemains := p.entries[oldKey]
	count := len(p.entries)
	p.mu.Unlock()
	if oldRemains || count != 257 {
		t.Fatal("oldest inactive reclamation or principal accounting")
	}
	p.mu.Lock()
	for _, entry := range p.entries {
		if entry.attached == 0 {
			entry.lastSeen = time.Now().Add(-2 * time.Hour)
		}
	}
	p.nextExpiry = time.Time{}
	p.mu.Unlock()
	expired := presenceRead(t, s, reader, presenceID(1))
	if !expired.Items[0].Attached || expired.Complete {
		t.Fatal("expiry evicted active or forgot sticky gap")
	}
	p.mu.Lock()
	count = len(p.entries)
	p.mu.Unlock()
	if count != 1 {
		t.Fatal("expired inactive keys retained")
	}
	// Stale cancellation callback cannot modify a replacement generation.
	key, _ := contactKey(scope)
	p.mu.Lock()
	oldGeneration := p.entries[key]
	p.entries[key] = &workerContactEntry{lastSeen: time.Now(), attached: 1}
	replacement := p.entries[key]
	p.mu.Unlock()
	cancel2()
	// Deterministically invoke the actual release path with the stale identity;
	// correctness does not depend on whether AfterFunc was scheduled first.
	p.release(key, oldGeneration)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries[key] != replacement || replacement.attached != 1 {
		t.Fatal("old generation callback changed replacement")
	}
}

func TestWorkerPresenceGlobalCapacityAndShutdownRestart(t *testing.T) {
	s, reader := workerPresenceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope := reader
	scope.WorkerID = presenceID(1)
	for i := 0; i < workerContactLimit; i++ {
		scope.PrincipalID = fmt.Sprintf("principal-%d", i/workerContactPrincipalLimit)
		scope.clientSession = fmt.Sprintf("%032x", i)
		s.observeWorkerContact(ctx, scope)
	}
	scope.PrincipalID = "overflow"
	scope.clientSession = strings.Repeat("f", 32)
	s.observeWorkerContact(ctx, scope)
	if len(s.workerContact.entries) != 8192 || presenceRead(t, s, reader, presenceID(1)).Complete {
		t.Fatal("global active bound/coverage")
	}
	since := s.workerContact.since
	s.closeWorkerContact()
	if len(s.workerContact.entries) != 0 || presenceAttached(s) != 0 {
		t.Fatal("shutdown retained leases")
	}
	s.observeWorkerContact(ctx, scope)
	if len(s.workerContact.entries) != 0 {
		t.Fatal("closed store accepts observation")
	}
	fresh, err := Open(context.Background(), filepath.Join(t.TempDir(), "new-private"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if !fresh.workerContact.since.After(since) || fresh.workerContact.incomplete || len(fresh.workerContact.entries) != 0 {
		t.Fatal("restart observation boundary")
	}
}

func TestWorkerPresenceFailedTransportDoesNotTouch(t *testing.T) {
	s, reader := workerPresenceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope, err := s.Scope(presenceRequest(ctx, presenceID(1), strings.Repeat("d", 32)), "read")
	if err != nil {
		t.Fatal(err)
	}
	initial := presenceRead(t, s, reader, presenceID(1))
	failed := errors.New("synthetic transport failure")
	err = s.streamMessageEvents(ctx, scope, s.Cursor(scope, messageFeed+scope.WorkerID, "0"), 1, func(EventPage) error { return nil }, func() error { return failed }, time.Millisecond)
	if !errors.Is(err, failed) {
		t.Fatal("failed heartbeat ignored")
	}
	if !presenceRead(t, s, reader, presenceID(1)).Items[0].LastSeenAt.Equal(*initial.Items[0].LastSeenAt) {
		t.Fatal("failed transport fabricated contact")
	}
}

func TestWorkerPresenceAuthenticatedPrincipalAndProjectKeys(t *testing.T) {
	s, reader := workerPresenceFixture(t)
	secondToken := "synthetic-second-presence-credential"
	if _, err := s.DB().Exec("INSERT INTO credentials VALUES('credential2','principal2',?,0,0)", tokenHash(secondToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("INSERT INTO grants VALUES('credential2','project',1,0)"); err != nil {
		t.Fatal(err)
	}
	before := presenceDurableHash(t, s)
	contexts := make([]context.Context, 3)
	cancels := make([]context.CancelFunc, 3)
	for i := range contexts {
		contexts[i], cancels[i] = context.WithCancel(context.Background())
		defer cancels[i]()
	}
	for i, ctx := range contexts {
		req := presenceRequest(ctx, presenceID(1), strings.Repeat("e", 32))
		if i == 1 {
			req.Header.Set("Authorization", "Bearer "+secondToken)
		}
		if i == 2 {
			req.SetPathValue("project_id", "other")
		}
		if _, err := s.Scope(req, "read"); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.workerContact.entries) != 3 {
		t.Fatal("principal/project key collision")
	}
	for _, project := range []string{"project", "other"} {
		scope := reader
		scope.ProjectID = project
		if !presenceRead(t, s, scope, presenceID(1)).Items[0].Attached {
			t.Fatal("authorized project contact missing")
		}
	}
	cancels[0]()
	cancels[1]()
	presenceEventually(t, func() bool { return presenceAttached(s) == 1 })
	if presenceRead(t, s, reader, presenceID(1)).Items[0].Attached {
		t.Fatal("other project kept worker attached")
	}
	if before != presenceDurableHash(t, s) {
		t.Fatal("principal/project observation changed durable state")
	}
}
