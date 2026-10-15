package reviewexec

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func policyFixture(t *testing.T) *attemptFixture {
	t.Helper()
	f := newAttemptFixture(t)
	if err := EnsurePolicySchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	var err error
	f.e, err = New(f.store, nil) // Policy storage is independent of provider setup.
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func policyInput(revision int64) PolicyConfiguration {
	return PolicyConfiguration{ExpectedRevision: &revision, RequiredChannels: []string{"hosted", "cli"}, PausedChannels: []string{}, MaxReviewFiles: 0}
}

func policyConfigure(t *testing.T, f *attemptFixture, scope core.Scope, key string, input PolicyConfiguration) json.RawMessage {
	t.Helper()
	body, err := f.e.ConfigurePolicy(t.Context(), scope, key, input)
	var receipt Receipt
	var fields map[string]any
	if err != nil || json.Unmarshal(body, &receipt) != nil || json.Unmarshal(body, &fields) != nil || len(fields) != 3 || receipt != (Receipt{ID: scope.ProjectID, State: "policy_configured", Revision: *input.ExpectedRevision + 1}) {
		t.Fatalf("policy compact receipt: %s %v", body, err)
	}
	return body
}

func policyRead(t *testing.T, f *attemptFixture, project, token string) PolicyRecord {
	t.Helper()
	record, err := f.e.ReadPolicy(t.Context(), evidenceReadScope(t, f, project, token))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func policyState(t *testing.T, f *attemptFixture) string {
	t.Helper()
	var rows, events, receipts string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(project_id||revision||body_json||actor_id||created_at,''),'') FROM review_policy_config`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(sequence||project_id||event_type||actor_id||payload_json,''),'') FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(principal_id||project_id||operation||target||key||fingerprint||response_json,''),'') FROM receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	return rows + "/" + events + "/" + receipts
}

func TestPolicyExplicitConfigurationReplayAndImmutableVersions(t *testing.T) {
	f := policyFixture(t)
	reader := evidenceReadScope(t, f, attemptProject, f.reader)
	_, err := f.e.ReadPolicy(t.Context(), reader)
	attemptCode(t, err, "reviews_policy_unconfigured")
	if attemptCount(t, f.store, `SELECT count(*) FROM review_policy_config`) != 0 {
		t.Fatal("unconfigured read allocated defaults")
	}
	admin := f.scope(t, attemptProject, f.admin, true)
	input := policyInput(0)
	first := policyConfigure(t, f, admin, "first-policy", input)
	var originalBody, actor, recorded string
	if err := f.store.DB().QueryRow(`SELECT body_json,actor_id,created_at FROM review_policy_config WHERE project_id=? AND revision=1`, attemptProject).Scan(&originalBody, &actor, &recorded); err != nil {
		t.Fatal(err)
	}
	got := policyRead(t, f, attemptProject, f.reader)
	if got.ProjectID != attemptProject || got.Revision != 1 || got.ActorID != admin.PrincipalID || actor != admin.PrincipalID || got.RecordedAt != recorded || !reflect.DeepEqual(got.Policy.RequiredChannels, []string{"hosted", "cli"}) || got.Policy.PausedChannels == nil || len(got.Policy.PausedChannels) != 0 || got.Policy.MaxReviewFiles != 0 || strings.Contains(encoded(got), "expected_revision") || strings.Contains(originalBody, "expected_revision") {
		t.Fatal("policy state changed order/zero/shape/attribution", got)
	}
	var shape map[string]any
	if json.Unmarshal([]byte(encoded(got)), &shape) != nil || len(shape) != 5 || len(shape["policy"].(map[string]any)) != 3 {
		t.Fatal("policy record exposes request-only fields")
	}
	if at, err := time.Parse(time.RFC3339Nano, recorded); err != nil || at.IsZero() {
		t.Fatal("policy recorded time missing")
	}
	second := policyInput(1)
	second.RequiredChannels = []string{"cli"}
	second.PausedChannels = []string{"hosted"}
	second.MaxReviewFiles = 100000
	policyConfigure(t, f, admin, "second-policy", second)
	current := policyRead(t, f, attemptProject, f.writer)
	if current.Revision != 2 || current.Policy.MaxReviewFiles != 100000 || !reflect.DeepEqual(current.Policy.PausedChannels, []string{"hosted"}) || !reflect.DeepEqual(current.Policy.RequiredChannels, []string{"cli"}) {
		t.Fatal("optional paused channel or explicit ceiling changed", current)
	}
	before := policyState(t, f)
	replay, err := f.e.ConfigurePolicy(t.Context(), admin, "first-policy", input)
	if err != nil || !bytes.Equal(first, replay) || policyState(t, f) != before {
		t.Fatal("exact replay changed current policy", err)
	}
	changed := policyInput(0)
	changed.MaxReviewFiles = 1
	_, err = f.e.ConfigurePolicy(t.Context(), admin, "first-policy", changed)
	attemptCode(t, err, "idempotency_conflict")
	_, err = f.e.ConfigurePolicy(t.Context(), admin, "stale-new-key", input)
	attemptCode(t, err, "review_conflict")
	var retained string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_policy_config WHERE project_id=? AND revision=1`, attemptProject).Scan(&retained); err != nil || retained != originalBody || policyState(t, f) != before {
		t.Fatal("update/conflict changed historical body", err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_policy_config`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='reviews.policy_configured' AND actor_id=?`, admin.PrincipalID) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM receipts WHERE operation='reviews.policy.configure'`) != 2 {
		t.Fatal("policy revisions lost actor/event/receipt")
	}
	for _, table := range []string{"review_execution_config", "review_operations", "review_execution_attempts", "review_execution_reservations", "review_execution_transitions", "workers"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("policy storage changed execution/coordination", table)
		}
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM projects`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM grants`) != 3 {
		t.Fatal("policy configuration changed project/grant state")
	}
}

func TestPolicyCurrentAuthorizationAndProjectIsolation(t *testing.T) {
	f := policyFixture(t)
	input := policyInput(0)
	admin := f.scope(t, attemptProject, f.admin, true)
	writer := f.scope(t, attemptProject, f.writer, false)
	reader := evidenceReadScope(t, f, attemptProject, f.reader)
	before := policyState(t, f)
	for _, scope := range []core.Scope{writer, reader} {
		_, err := f.e.ConfigurePolicy(t.Context(), scope, "denied", input)
		attemptCode(t, err, "forbidden")
	}
	if policyState(t, f) != before {
		t.Fatal("write/read grant configured admin policy")
	}
	// Direct callers need not have obtained AdminScope; role is checked in Mutate.
	directAdmin := f.scope(t, attemptProject, f.admin, false)
	first := policyConfigure(t, f, directAdmin, "direct-admin", input)
	otherScope := evidenceReadScope(t, f, otherAttemptProject, f.other)
	_, err := f.e.ReadPolicy(t.Context(), otherScope)
	attemptCode(t, err, "reviews_policy_unconfigured")
	forged := reader
	forged.ProjectID = otherAttemptProject
	_, err = f.e.ReadPolicy(t.Context(), forged)
	attemptCode(t, err, "forbidden")
	otherAdmin := f.scope(t, otherAttemptProject, f.admin, true)
	other := policyInput(0)
	other.RequiredChannels = []string{"hosted"}
	other.PausedChannels = []string{"cli"}
	other.MaxReviewFiles = 1
	policyConfigure(t, f, otherAdmin, "other-policy", other)
	if policyRead(t, f, attemptProject, f.reader).Policy.MaxReviewFiles != 0 || policyRead(t, f, otherAttemptProject, f.other).Policy.MaxReviewFiles != 1 {
		t.Fatal("policy crossed project boundary")
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.ReadPolicy(t.Context(), reader)
	attemptCode(t, err, "unauthorized")
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE is_admin=1`); err != nil {
		t.Fatal(err)
	}
	before = policyState(t, f)
	for _, scope := range []core.Scope{admin, directAdmin} {
		_, err = f.e.ConfigurePolicy(t.Context(), scope, "direct-admin", input)
		attemptCode(t, err, "unauthorized")
	}
	if policyState(t, f) != before || len(first) == 0 {
		t.Fatal("revoked exact retry changed policy")
	}
}

// Deadline is first consulted by Mutate after request fingerprinting. Holding
// that boundary proves the callback owns its input while caller memory changes.
type policyGateContext struct {
	context.Context
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (ctx *policyGateContext) Deadline() (time.Time, bool) {
	ctx.once.Do(func() { close(ctx.reached) })
	select {
	case <-ctx.release:
	case <-ctx.Context.Done():
	}
	return ctx.Context.Deadline()
}

func TestPolicyOwnsInputBeforeConcurrentWork(t *testing.T) {
	f := policyFixture(t)
	admin := f.scope(t, attemptProject, f.admin, true)
	base, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	gate := &policyGateContext{Context: base, reached: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	input := policyInput(0)
	input.PausedChannels = []string{"hosted"}
	type result struct {
		body json.RawMessage
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := f.e.ConfigurePolicy(gate, admin, "owned-policy", input)
		done <- result{body, err}
	}()
	select {
	case <-gate.reached:
	case <-base.Done():
		t.Fatal("policy mutation did not reach its bounded transaction")
	}
	*input.ExpectedRevision = 9
	input.RequiredChannels[0] = "cli"
	input.PausedChannels[0] = "cli"
	// An independent project's mutation remains usable while this caller waits.
	policyConfigure(t, f, f.scope(t, otherAttemptProject, f.admin, true), "independent-project", policyInput(0))
	release.Do(func() { close(gate.release) })
	var got result
	select {
	case got = <-done:
	case <-base.Done():
		t.Fatal("owned policy mutation hung")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	record := policyRead(t, f, attemptProject, f.reader)
	if record.Revision != 1 || !reflect.DeepEqual(record.Policy.RequiredChannels, []string{"hosted", "cli"}) || !reflect.DeepEqual(record.Policy.PausedChannels, []string{"hosted"}) {
		t.Fatal("caller mutated the journal input", record)
	}
	original := policyInput(0)
	original.PausedChannels = []string{"hosted"}
	before := policyState(t, f)
	replay, err := f.e.ConfigurePolicy(t.Context(), admin, "owned-policy", original)
	if err != nil || !bytes.Equal(replay, got.body) || policyState(t, f) != before {
		t.Fatal("stored policy and receipt fingerprint describe different inputs", err)
	}
}

func TestPolicyConcurrentCASAndAtomicEventFailure(t *testing.T) {
	f := policyFixture(t)
	scope := f.scope(t, attemptProject, f.admin, true)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_policy_event BEFORE INSERT ON events WHEN NEW.event_type='reviews.policy_configured' BEGIN SELECT RAISE(ABORT,'private policy failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := policyState(t, f)
	body, err := f.e.ConfigurePolicy(t.Context(), scope, "rollback", policyInput(0))
	if err == nil || len(body) != 0 || strings.Contains(err.Error(), "private") || policyState(t, f) != before {
		t.Fatal("event failure left policy/receipt or leaked SQL diagnostics", err)
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER reject_policy_event`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, key := range []string{"writer-one", "writer-two"} {
		go func(key string) {
			<-start
			_, err := f.e.ConfigurePolicy(t.Context(), scope, key, policyInput(0))
			results <- err
		}(key)
	}
	close(start)
	winners := 0
	for i := 0; i < 2; i++ {
		if err := <-results; err == nil {
			winners++
		} else {
			attemptCode(t, err, "review_conflict")
		}
	}
	if winners != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_policy_config`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != 1 {
		t.Fatal("same-revision writers duplicated policy effects")
	}
}

func TestPolicyValidationAndNoSilentDefaults(t *testing.T) {
	f := policyFixture(t)
	scope := f.scope(t, attemptProject, f.admin, true)
	for name, change := range map[string]func(*PolicyConfiguration){
		"revision_missing":   func(c *PolicyConfiguration) { c.ExpectedRevision = nil },
		"revision_negative":  func(c *PolicyConfiguration) { c.ExpectedRevision = adjudicationRevision(-1) },
		"revision_overflow":  func(c *PolicyConfiguration) { c.ExpectedRevision = adjudicationRevision(math.MaxInt64) },
		"required_missing":   func(c *PolicyConfiguration) { c.RequiredChannels = nil },
		"required_empty":     func(c *PolicyConfiguration) { c.RequiredChannels = []string{} },
		"required_duplicate": func(c *PolicyConfiguration) { c.RequiredChannels = []string{"cli", "cli"} },
		"required_large":     func(c *PolicyConfiguration) { c.RequiredChannels = []string{"cli", "hosted", "cli"} },
		"required_unknown":   func(c *PolicyConfiguration) { c.RequiredChannels = []string{"manual"} },
		"required_case":      func(c *PolicyConfiguration) { c.RequiredChannels = []string{"CLI"} },
		"required_padded":    func(c *PolicyConfiguration) { c.RequiredChannels = []string{" cli"} },
		"required_control":   func(c *PolicyConfiguration) { c.RequiredChannels = []string{"cli\x00"} },
		"required_utf8":      func(c *PolicyConfiguration) { c.RequiredChannels = []string{"cli\xff"} },
		"paused_missing":     func(c *PolicyConfiguration) { c.PausedChannels = nil },
		"paused_duplicate":   func(c *PolicyConfiguration) { c.PausedChannels = []string{"hosted", "hosted"} },
		"paused_unknown":     func(c *PolicyConfiguration) { c.PausedChannels = []string{"manual"} },
		"paused_large":       func(c *PolicyConfiguration) { c.PausedChannels = []string{"cli", "hosted", "cli"} },
		"size_negative":      func(c *PolicyConfiguration) { c.MaxReviewFiles = -1 },
		"size_large":         func(c *PolicyConfiguration) { c.MaxReviewFiles = 100001 },
	} {
		t.Run(name, func(t *testing.T) {
			input := policyInput(0)
			change(&input)
			before := policyState(t, f)
			_, err := f.e.ConfigurePolicy(t.Context(), scope, "invalid", input)
			attemptCode(t, err, "validation_failed")
			if policyState(t, f) != before {
				t.Fatal("invalid policy changed state")
			}
		})
	}
	for _, key := range []string{"", " padded "} {
		_, err := f.e.ConfigurePolicy(t.Context(), scope, key, policyInput(0))
		attemptCode(t, err, "validation_failed")
	}
	if policyState(t, f) != "//" {
		t.Fatal("invalid policy created defaults or receipts")
	}
	valid := policyInput(0)
	valid.RequiredChannels = []string{"cli"}
	valid.PausedChannels = []string{"cli", "hosted"}
	valid.MaxReviewFiles = 1
	policyConfigure(t, f, scope, "explicit-overlap", valid)
	got := policyRead(t, f, attemptProject, f.reader)
	if !reflect.DeepEqual(got.Policy.RequiredChannels, valid.RequiredChannels) || !reflect.DeepEqual(got.Policy.PausedChannels, valid.PausedChannels) {
		t.Fatal("valid pause outside requirements was normalized or refused")
	}
	// A saturated durable row cannot wrap into a negative or reused revision.
	if _, err := f.store.DB().Exec(`INSERT INTO review_policy_config(project_id,revision,body_json,actor_id,created_at) VALUES(?,?,?,?,?)`, attemptProject, int64(math.MaxInt64), encoded(got.Policy), scope.PrincipalID, stamp()); err != nil {
		t.Fatal(err)
	}
	before := policyState(t, f)
	next := policyInput(math.MaxInt64 - 1)
	_, err := f.e.ConfigurePolicy(t.Context(), scope, "overflow-current", next)
	attemptCode(t, err, "review_conflict")
	if policyState(t, f) != before {
		t.Fatal("saturated revision changed state")
	}
}

func TestPolicyRestartRetainsHistoryAndReceipts(t *testing.T) {
	f := policyFixture(t)
	scope := f.scope(t, attemptProject, f.admin, true)
	first := policyConfigure(t, f, scope, "durable-first", policyInput(0))
	next := policyInput(1)
	next.RequiredChannels = []string{"hosted"}
	next.PausedChannels = []string{"cli"}
	next.MaxReviewFiles = 300
	policyConfigure(t, f, scope, "durable-second", next)
	current := policyRead(t, f, attemptProject, f.reader)
	before := policyState(t, f)
	var seq int
	var name, path string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	for i := 0; i < 2; i++ {
		if err := EnsurePolicySchema(t.Context(), store); err != nil {
			t.Fatal(err)
		}
	}
	f.e, err = New(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if instance != store.InstanceID() || !reflect.DeepEqual(current, policyRead(t, f, attemptProject, f.reader)) || before != policyState(t, f) {
		t.Fatal("restart/schema initialization rewrote policy history")
	}
	replay, err := f.e.ConfigurePolicy(t.Context(), f.scope(t, attemptProject, f.admin, true), "durable-first", policyInput(0))
	if err != nil || !bytes.Equal(first, replay) || policyState(t, f) != before {
		t.Fatal("restart lost exact receipt", err)
	}
}
