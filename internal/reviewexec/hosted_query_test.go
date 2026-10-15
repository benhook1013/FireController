package reviewexec

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedQueryFixture(t *testing.T) (*attemptFixture, Attempt, Anchor, reviewprovider.Author, reviewprovider.Author) {
	t.Helper()
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "query-request", "hosted", 2, 1))
	return f, a, attemptAnchor(attemptProject, 2), reviewprovider.Author{ID: 12, Login: "poster", Kind: "User"}, reviewprovider.Author{ID: 30, Login: "coderabbitai[bot]", Kind: "Bot"}
}

func hostedQueryState(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var rows string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(id||project_id||attempt_id||operation_id||capability_fingerprint||revision||body_json,''),'') FROM review_hosted_allowance_queries`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows + evidenceState(t, f, a)
}

func hostedQueryComment(t *testing.T, q HostedAllowanceQuery) reviewprovider.IssueComment {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, q.EarliestPosting)
	if err != nil {
		t.Fatal(err)
	}
	// GitHub timestamps have second precision; the boundary must not exclude a
	// comment posted during the same second as the durable local intent.
	at = at.Truncate(time.Second)
	return reviewprovider.IssueComment{ID: 91, Body: q.Intent, Author: q.PostingActor, CreatedAt: at, UpdatedAt: at}
}

func TestHostedQueryMigrationConcurrentBeginAndRetainedReplay(t *testing.T) {
	f, a, anchor, poster, provider := hostedQueryFixture(t)
	originalAttempt := attemptState(t, f, a)
	original := evidenceState(t, f, a)
	admissionSQL(t, f, `DROP TABLE review_hosted_preflight`)
	admissionSQL(t, f, `DROP TABLE review_hosted_allowance_queries`)
	admissionSQL(t, f, `UPDATE metadata SET value='5' WHERE key='schema:review_execution'`)
	cliAllowanceReopen(t, f)
	var version string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='schema:review_execution'`).Scan(&version); err != nil || version != "6" || evidenceState(t, f, a) != original {
		t.Fatal("v5 upgrade changed retained state", version, err)
	}
	type result struct {
		q       HostedAllowanceQuery
		created bool
		err     error
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			<-start
			q, created, err := f.e.BeginHostedAllowanceQuery(context.Background(), attemptProject, a.ID, 1, anchor, poster, provider)
			results <- result{q, created, err}
		}()
	}
	close(start)
	var query HostedAllowanceQuery
	createdCount := 0
	for i := 0; i < 8; i++ {
		got := <-results
		if got.err != nil || query.ID != "" && !reflect.DeepEqual(query, got.q) {
			t.Fatal("concurrent query did not converge", got.err)
		}
		query = got.q
		if got.created {
			createdCount++
		}
	}
	if createdCount != 1 || query.Revision != 1 || query.Comment != nil || query.Anchor != anchor || query.PostingActor != poster || query.ProviderActor != provider || query.ProjectID != attemptProject || query.AttemptID != a.ID || query.OperationID != a.OperationID || attemptState(t, f, a) != originalAttempt || attemptCount(t, f.store, `SELECT count(*) FROM review_hosted_allowance_queries`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.hosted_query_started'`) != 1 {
		t.Fatal("query changed admission or duplicated post opportunity", query, createdCount)
	}
	var actor, payload string
	if err := f.store.DB().QueryRow(`SELECT actor_id,payload_json FROM events WHERE event_type='review.hosted_query_started'`).Scan(&actor, &payload); err != nil || actor != "exec-writer-principal" || len(payload) > 1024 || !strings.Contains(payload, a.ID) || strings.Contains(payload, query.Intent) || strings.Contains(payload, anchor.Repository) {
		t.Fatal("query event leaked content or lost original identity", payload, err)
	}
	before := hostedQueryState(t, f, a)
	read, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
	if err != nil || !reflect.DeepEqual(read, query) || hostedQueryState(t, f, a) != before {
		t.Fatal("read changed or lost query", err)
	}
	read.Anchor.Repository = "caller/changed"
	if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, 1, "cancelled", "user_cancelled", ""); err != nil {
		t.Fatal(err)
	}
	admissionPolicy(t, f, 1, "hosted")
	f.configure(t, attemptProject, "other-account", 1, true)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	cliAllowanceReopen(t, f)
	f.e, err = New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	before = hostedQueryState(t, f, a)
	replay, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
	if err != nil || created || !reflect.DeepEqual(replay, query) || hostedQueryState(t, f, a) != before {
		t.Fatal("retained replay evaluated current eligibility", err)
	}
	comment := hostedQueryComment(t, query)
	comment.Author.Login = "renamed-poster"
	confirmed, err := f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, comment)
	if err != nil || confirmed.Revision != 2 || confirmed.Comment == nil || *confirmed.Comment != comment {
		t.Fatal("confirmation after cancellation/revocation lost posted fact", err)
	}
	cliAllowanceReopen(t, f)
	before = hostedQueryState(t, f, a)
	confirmed.Comment.Body = "changed caller copy"
	read, err = f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
	if err != nil || read.Comment == nil || *read.Comment != comment {
		t.Fatal("caller alias changed retained query", err)
	}
	again, err := f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, comment)
	if err != nil || !reflect.DeepEqual(read, again) || hostedQueryState(t, f, a) != before || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.hosted_query_posted'`) != 1 {
		t.Fatal("exact confirmation replay repeated effect", err)
	}
	if _, created, err = f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider); err != nil || created || hostedQueryState(t, f, a) != before {
		t.Fatal("confirmed intent replay grants another post", err)
	}
	_, err = f.e.ReadHostedAllowanceQuery(t.Context(), otherAttemptProject, a.ID)
	attemptCode(t, err, "not_found")
	changed := anchor
	changed.ObservedAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
	_, created, err = f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, changed, poster, provider)
	attemptCode(t, err, "review_conflict")
	if created || hostedQueryState(t, f, a) != before {
		t.Fatal("changed replay had effect")
	}
}

func TestHostedQueryReplayKeepsOriginalAnchorAfterSeparateAdmission(t *testing.T) {
	f, a, anchor, poster, provider := hostedQueryFixture(t)
	query, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
	if err != nil {
		t.Fatal(err)
	}
	refreshed := anchor
	refreshed.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	body, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: anchor.CandidateSHA})
	if err != nil {
		t.Fatal(err)
	}
	admitted, err := f.e.Admit(t.Context(), attemptProject, a.ID, 1, refreshed, body)
	if err != nil {
		t.Fatal(err)
	}
	before := hostedQueryState(t, f, admitted)
	got, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
	if err != nil || created || !reflect.DeepEqual(got, query) || hostedQueryState(t, f, admitted) != before {
		t.Fatal("later admission anchor rewrote preflight", err)
	}
	_, _, err = f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 2, anchor, poster, provider)
	attemptCode(t, err, "review_conflict")
}

func TestHostedQueryRejectsNewIneligibleInputWithoutEffects(t *testing.T) {
	for name, test := range map[string]struct {
		change func(*testing.T, *attemptFixture, *Attempt, *Anchor, *reviewprovider.Author, *reviewprovider.Author)
		code   string
	}{
		"wrong revision": {func(_ *testing.T, _ *attemptFixture, a *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			a.Revision = 2
		}, "review_conflict"},
		"wrong head": {func(_ *testing.T, _ *attemptFixture, _ *Attempt, a *Anchor, _, _ *reviewprovider.Author) {
			a.CandidateSHA = strings.Repeat("f", 40)
		}, "review_conflict"},
		"wrong repository": {func(_ *testing.T, _ *attemptFixture, _ *Attempt, a *Anchor, _, _ *reviewprovider.Author) {
			a.RepositoryID++
		}, "review_conflict"},
		"stale source": {func(_ *testing.T, _ *attemptFixture, _ *Attempt, a *Anchor, _, _ *reviewprovider.Author) {
			a.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
		}, "validation_failed"},
		"future source": {func(_ *testing.T, _ *attemptFixture, _ *Attempt, a *Anchor, _, _ *reviewprovider.Author) {
			a.ObservedAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
		}, "validation_failed"},
		"noncanonical source": {func(_ *testing.T, _ *attemptFixture, _ *Attempt, a *Anchor, _, _ *reviewprovider.Author) {
			a.ObservedAt = "2026-01-01T00:00:00+00:00"
		}, "validation_failed"},
		"bad actor":        {func(_ *testing.T, _ *attemptFixture, _ *Attempt, _ *Anchor, p, _ *reviewprovider.Author) { p.ID = 0 }, "validation_failed"},
		"identical actors": {func(_ *testing.T, _ *attemptFixture, _ *Attempt, _ *Anchor, p, b *reviewprovider.Author) { *b = *p }, "validation_failed"},
		"revoked": {func(t *testing.T, f *attemptFixture, _ *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
		}, "forbidden"},
		"policy paused": {func(t *testing.T, f *attemptFixture, _ *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			admissionPolicy(t, f, 1, "hosted")
		}, "review_conflict"},
		"configuration paused": {func(t *testing.T, f *attemptFixture, _ *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			f.configure(t, attemptProject, "test-capability", 1, true)
		}, "review_conflict"},
		"capability changed": {func(t *testing.T, f *attemptFixture, _ *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			f.configure(t, attemptProject, "other-account", 1, false)
		}, "review_conflict"},
		"installed fingerprint changed": {func(t *testing.T, f *attemptFixture, _ *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			capability := attemptCapabilities[0]
			capability.Fingerprint = strings.Repeat("f", 64)
			var err error
			f.e, err = New(f.store, []Capability{capability})
			if err != nil {
				t.Fatal(err)
			}
		}, "review_conflict"},
		"policy revised": {func(t *testing.T, f *attemptFixture, _ *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			admissionPolicy(t, f, 1)
		}, "review_conflict"},
		"no current capability": {func(t *testing.T, f *attemptFixture, _ *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			var err error
			f.e, err = New(f.store, nil)
			if err != nil {
				t.Fatal(err)
			}
		}, "review_conflict"},
		"cancelled": {func(t *testing.T, f *attemptFixture, a *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			if _, err := f.e.Advance(t.Context(), attemptProject, a.ID, 1, "cancelled", "cancelled", ""); err != nil {
				t.Fatal(err)
			}
		}, "review_conflict"},
		"CLI": {func(t *testing.T, f *attemptFixture, a *Attempt, _ *Anchor, _, _ *reviewprovider.Author) {
			admissionSQL(t, f, `UPDATE review_operations SET channel='cli' WHERE id=?`, a.OperationID)
		}, "review_conflict"},
	} {
		t.Run(name, func(t *testing.T) {
			f, a, anchor, poster, provider := hostedQueryFixture(t)
			test.change(t, f, &a, &anchor, &poster, &provider)
			before := hostedQueryState(t, f, a)
			q, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, a.Revision, anchor, poster, provider)
			attemptCode(t, err, test.code)
			if q.ID != "" || created || hostedQueryState(t, f, a) != before {
				t.Fatal("invalid begin had effects")
			}
		})
	}
}

func TestHostedQueryConfirmationValidationAndReplayConflict(t *testing.T) {
	f, a, anchor, poster, provider := hostedQueryFixture(t)
	q, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
	if err != nil {
		t.Fatal(err)
	}
	valid := hostedQueryComment(t, q)
	before := hostedQueryState(t, f, a)
	for name, change := range map[string]func(*reviewprovider.IssueComment){
		"missing ID":    func(c *reviewprovider.IssueComment) { c.ID = 0 },
		"other body":    func(c *reviewprovider.IssueComment) { c.Body += "\n" },
		"foreign actor": func(c *reviewprovider.IssueComment) { c.Author.ID++ },
		"wrong kind":    func(c *reviewprovider.IssueComment) { c.Author.Kind = "Bot" },
		"bad login":     func(c *reviewprovider.IssueComment) { c.Author.Login = "bad\nlogin" },
		"edited":        func(c *reviewprovider.IssueComment) { c.UpdatedAt = c.CreatedAt.Add(time.Second) },
		"before intent": func(c *reviewprovider.IssueComment) {
			c.CreatedAt = c.CreatedAt.Add(-time.Second)
			c.UpdatedAt = c.CreatedAt
		},
		"future": func(c *reviewprovider.IssueComment) {
			c.CreatedAt = time.Now().Add(time.Minute).UTC()
			c.UpdatedAt = c.CreatedAt
		},
		"nonUTC": func(c *reviewprovider.IssueComment) {
			c.CreatedAt = c.CreatedAt.In(time.FixedZone("same-offset", 0))
			c.UpdatedAt = c.CreatedAt
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := valid
			change(&c)
			_, err := f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, c)
			attemptCode(t, err, "review_conflict")
			if hostedQueryState(t, f, a) != before {
				t.Fatal("invalid confirmation had effect")
			}
		})
	}
	_, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 2, valid)
	attemptCode(t, err, "review_conflict")
	if _, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, valid); err != nil {
		t.Fatal(err)
	}
	before = hostedQueryState(t, f, a)
	changed := valid
	changed.ID++
	_, err = f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, changed)
	attemptCode(t, err, "review_conflict")
	if hostedQueryState(t, f, a) != before {
		t.Fatal("different posted comment replaced original")
	}
	// Aged retained source is no longer fresh enough to start a new query, but
	// exact replay of this original intent remains valid without a clock wait.
	read, err := f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	read.Anchor.ObservedAt = time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET body_json=? WHERE id=?`, encoded(read), q.ID)
	_, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, read.Anchor, poster, provider)
	if err != nil || created {
		t.Fatal("aged exact query replay rechecked source freshness", err)
	}
}

func TestHostedQueryCorruptRetainedRowsFailClosed(t *testing.T) {
	for name, change := range map[string]func(*HostedAllowanceQuery){
		"ID":          func(q *HostedAllowanceQuery) { q.ID = "review_hosted_query_" + strings.Repeat("0", 32) },
		"project":     func(q *HostedAllowanceQuery) { q.ProjectID = otherAttemptProject },
		"operation":   func(q *HostedAllowanceQuery) { q.OperationID = "review_operation_" + strings.Repeat("0", 32) },
		"fingerprint": func(q *HostedAllowanceQuery) { q.CapabilityFingerprint = strings.Repeat("0", 64) },
		"intent":      func(q *HostedAllowanceQuery) { q.Intent += "\n" },
		"repository":  func(q *HostedAllowanceQuery) { q.Anchor.RepositoryID++ },
		"head":        func(q *HostedAllowanceQuery) { q.Anchor.CandidateSHA = strings.Repeat("f", 40) },
		"revision":    func(q *HostedAllowanceQuery) { q.Revision = 2 },
		"time":        func(q *HostedAllowanceQuery) { q.EarliestPosting = "2026-01-01T00:00:00+00:00" },
		"actors":      func(q *HostedAllowanceQuery) { q.ProviderActor = q.PostingActor },
	} {
		t.Run(name, func(t *testing.T) {
			f, a, anchor, poster, provider := hostedQueryFixture(t)
			q, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
			if err != nil {
				t.Fatal(err)
			}
			change(&q)
			admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET body_json=? WHERE attempt_id=?`, encoded(q), a.ID)
			before := hostedQueryState(t, f, a)
			_, err = f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
			attemptCode(t, err, "storage_unavailable")
			_, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
			attemptCode(t, err, "storage_unavailable")
			if created || hostedQueryState(t, f, a) != before {
				t.Fatal("corrupt query was repaired or overwrote retained row")
			}
		})
	}
}

func TestHostedQueryStrictStoredJSONCancellationAndAtomicEventFailure(t *testing.T) {
	f, a, anchor, poster, provider := hostedQueryFixture(t)
	before := hostedQueryState(t, f, a)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if q, created, err := f.e.BeginHostedAllowanceQuery(ctx, attemptProject, a.ID, 1, anchor, poster, provider); err == nil || q.ID != "" || created || hostedQueryState(t, f, a) != before {
		t.Fatal("cancelled begin persisted intent", err)
	}
	admissionSQL(t, f, `CREATE TRIGGER fail_query_event BEFORE INSERT ON events WHEN NEW.event_type='review.hosted_query_started' BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if q, created, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider); err == nil || q.ID != "" || created || hostedQueryState(t, f, a) != before {
		t.Fatal("failed event left post authorization", err)
	}
	admissionSQL(t, f, `DROP TRIGGER fail_query_event`)
	q, _, err := f.e.BeginHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, anchor, poster, provider)
	if err != nil {
		t.Fatal(err)
	}
	body := encoded(q)
	for _, bad := range []string{strings.TrimSuffix(body, "}") + `,"unknown":true}`, strings.Replace(body, `"revision":1`, `"revision":1,"Revision":1`, 1), strings.Repeat(" ", hostedQueryLimit+1)} {
		admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET body_json=? WHERE id=?`, bad, q.ID)
		_, err = f.e.ReadHostedAllowanceQuery(t.Context(), attemptProject, a.ID)
		attemptCode(t, err, "storage_unavailable")
	}
	admissionSQL(t, f, `UPDATE review_hosted_allowance_queries SET body_json=? WHERE id=?`, body, q.ID)
	before = hostedQueryState(t, f, a)
	admissionSQL(t, f, `CREATE TRIGGER fail_query_event BEFORE INSERT ON events WHEN NEW.event_type='review.hosted_query_posted' BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if got, err := f.e.ConfirmHostedAllowanceQuery(t.Context(), attemptProject, a.ID, 1, hostedQueryComment(t, q)); err == nil || got.ID != "" || hostedQueryState(t, f, a) != before {
		t.Fatal("failed event retained partial confirmation", err)
	}
	// Private canonical records remain fully decodable by the standard owned DTO.
	var decoded HostedAllowanceQuery
	if err := json.Unmarshal([]byte(body), &decoded); err != nil || !reflect.DeepEqual(decoded, q) {
		t.Fatal("canonical DTO lost fields", err)
	}
}
