package reviewexec

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedSettlementFixture(t *testing.T, knownTrigger bool) (*attemptFixture, Attempt, HostedObservationInput) {
	t.Helper()
	f, a, input := hostedObservationFixture(t)
	var err error
	if knownTrigger {
		a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "active", "trigger_recorded", "101")
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err = f.e.Advance(t.Context(), attemptProject, a.ID, a.Revision, "uncertain", "dispatch_interrupted", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range hostedObservationLimit {
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprint("controlled retained reference ", i))))
		input.CaptureSHA256 = sum
		input.CaptureID = "review_capture_" + sum[:32]
		if _, err := f.e.RecordHostedObservation(t.Context(), attemptProject, a.ID, input); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	return f, a, input
}
func hostedSettlementPath(project string, a Attempt) string {
	return "/api/v1/projects/" + project + "/reviews/attempts/" + a.ID + "/settlement"
}
func hostedSettlementBody(a Attempt) map[string]any {
	return map[string]any{
		"expected_revision": a.Revision, "expected_repository_id": a.Anchor.RepositoryID, "expected_head_sha": a.Anchor.CandidateSHA, "expected_external_id": a.ExternalID, "external_action_stopped": true, "reason": "Operator independently verified the controlled external action stopped.\nNo executor will continue it.", "evidence_reference": "operator-record:controlled-stop",
	}
}
func hostedSettlementCall(t *testing.T, f *attemptFixture, a Attempt, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return f.request("POST", hostedSettlementPath(attemptProject, a), f.admin, key, encoded(body))
}
func TestHostedSettlementPublicRouteReleasesOnlyStoppedCapacityAttempt(t *testing.T) {
	f, a, _ := hostedSettlementFixture(t, true)
	f.configure(t, otherAttemptProject, "other-account", 0, false)
	other := f.start(t, otherAttemptProject, f.enqueue(t, otherAttemptProject, "other-reserved", "hosted", 4, 1))
	otherBefore := attemptState(t, f, other)
	observations, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || len(observations) != 200 {
		t.Fatal(err)
	}
	r := hostedSettlementCall(t, f, a, "operator-settlement", hostedSettlementBody(a))
	if r.Code != 200 {
		t.Fatalf("settlement route: %d %s", r.Code, r.Body.String())
	}
	var receipt struct {
		AttemptID   string `json:"attempt_id"`
		OperationID string `json:"operation_id"`
		Revision    int64  `json:"revision"`
		State       string `json:"state"`
		Outcome     string `json:"outcome"`
	}
	if json.Unmarshal(r.Body.Bytes(), &receipt) != nil || receipt.AttemptID != a.ID || receipt.OperationID != a.OperationID || receipt.Revision != a.Revision+1 || receipt.State != "cancelled" || receipt.Outcome != "settled" {
		t.Fatal("invalid compact settlement receipt", r.Body.String())
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_operations WHERE state='completed'`) != 0 {
		t.Fatal("settlement retained reservation or invented final credit")
	}
	after, err := f.e.ReadHostedObservations(t.Context(), attemptProject, a.ID)
	if err != nil || !reflect.DeepEqual(observations, after) {
		t.Fatal("settlement changed original observations", err)
	}
	before := hostedObservationState(t, f, a)
	replay := hostedSettlementCall(t, f, a, "operator-settlement", hostedSettlementBody(a))
	if replay.Code != 200 || replay.Body.String() != r.Body.String() || hostedObservationState(t, f, a) != before {
		t.Fatal("exact replay changed settlement", replay.Body.String())
	}
	if strings.Contains(r.Body.String(), "Operator") || strings.Contains(r.Body.String(), "controlled-stop") {
		t.Fatal("receipt echoed audit content")
	}
	if attemptState(t, f, other) != otherBefore {
		t.Fatal("settlement changed another account/target reservation")
	}
	var actor, payload, intent, state, reason string
	if err := f.store.DB().QueryRow(`SELECT actor_id,payload_json FROM events WHERE event_type='review.hosted_settled' AND resource_id=?`, a.ID).Scan(&actor, &payload); err != nil {
		t.Fatal(err)
	}
	var audit hostedSettlementAudit
	if json.Unmarshal([]byte(payload), &audit) != nil || actor != f.scope(t, attemptProject, f.admin, true).PrincipalID || audit.Anchor != *a.Anchor || audit.ExpectedRevision != a.Revision || audit.ExternalID != a.ExternalID || !audit.ExternalActionStopped || audit.Reason != hostedSettlementBody(a)["reason"].(string) || audit.EvidenceReference != "operator-record:controlled-stop" {
		t.Fatal("administrator audit lost original target or exact assertion")
	}
	if err := f.store.DB().QueryRow(`SELECT a.intent,o.state,o.reason FROM review_execution_attempts a JOIN review_operations o ON o.id=a.operation_id WHERE a.id=?`, a.ID).Scan(&intent, &state, &reason); err != nil || intent != a.Intent || state != "cancelled" || reason != "operator_verified_external_stop" {
		t.Fatal("settlement rewrote intent or omitted normal cancellation", err)
	}
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{RepositoryID: 801, Number: 2, State: "open", HeadSHA: a.Anchor.CandidateSHA, BaseSHA: a.Anchor.ParentSHA, BaseRef: "main", ChangedFiles: 1}, ParentSHA: a.Anchor.ParentSHA, ObservedAt: stamp()}
	covered, err := f.e.Coverage(t.Context(), attemptProject, plan, "hosted")
	if err != nil || covered {
		t.Fatal("operator assertion granted coverage", covered, err)
	}
	// Later work and an ordinary reopen cannot cause the original key to act again.
	next := f.start(t, attemptProject, f.enqueue(t, attemptProject, "later-request", "hosted", 2, 1))
	nextBefore := attemptState(t, f, next)
	cliAllowanceReopen(t, f)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	before = hostedObservationState(t, f, a)
	replay = hostedSettlementCall(t, f, a, "operator-settlement", hostedSettlementBody(a))
	if replay.Code != 200 || replay.Body.String() != r.Body.String() || hostedObservationState(t, f, a) != before || attemptState(t, f, next) != nextBefore {
		t.Fatal("restart replay changed later work", replay.Body.String())
	}
	changed := hostedSettlementBody(a)
	changed["reason"] = "changed explanation"
	settlementError(t, hostedSettlementCall(t, f, a, "operator-settlement", changed), 409, "idempotency_conflict")
}

func settlementError(t *testing.T, r *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if r.Code != status || !strings.Contains(r.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("want%d/%s got%d %s", status, code, r.Code, r.Body.String())
	}
}
func TestHostedSettlementStrictInputAndAuditBudgetAreAtomic(t *testing.T) {
	f, a, _ := hostedSettlementFixture(t, false)
	before := hostedObservationState(t, f, a)
	for _, field := range []string{"expected_revision", "expected_repository_id", "expected_head_sha", "expected_external_id", "external_action_stopped", "reason", "evidence_reference"} {
		for _, null := range []bool{false, true} {
			body := hostedSettlementBody(a)
			if null {
				body[field] = nil
			} else {
				delete(body, field)
			}
			settlementError(t, hostedSettlementCall(t, f, a, "invalid", body), 400, "validation_failed")
		}
	}
	for _, change := range []map[string]any{
		{"expected_revision": 0}, {"expected_revision": math.MaxInt64}, {"expected_revision": 1.5}, {"expected_repository_id": 0}, {"expected_head_sha": strings.Repeat("A", 40)}, {"expected_external_id": "with whitespace"}, {"external_action_stopped": false}, {"external_action_stopped": "true"}, {"reason": " "}, {"reason": "bad\u0085"}, {"reason": "bad\rtext"}, {"reason": strings.Repeat("x", 1025)}, {"evidence_reference": "bad\nreference"}, {"evidence_reference": "bad\tref"}, {"evidence_reference": strings.Repeat("x", 1025)}, {"unknown": "field"},
	} {
		body := hostedSettlementBody(a)
		for key, value := range change {
			body[key] = value
		}
		settlementError(t, hostedSettlementCall(t, f, a, "invalid", body), 400, "validation_failed")
	}
	raw := encoded(hostedSettlementBody(a))
	for _, body := range []string{strings.Replace(raw, `"external_action_stopped":true`, `"external_action_stopped":true,"external_action_stopped":true`, 1), strings.Replace(raw, `"reason":`, `"Reason":`, 1), strings.Replace(raw, `"reason":`, `"reason":"bad\ud800","other":`, 1)} {
		settlementError(t, f.request("POST", hostedSettlementPath(attemptProject, a), f.admin, "invalid", body), 400, "validation_failed")
	}
	settlementError(t, f.request("POST", hostedSettlementPath(attemptProject, a)+"?force=true", f.admin, "invalid", raw), 400, "validation_failed")
	settlementError(t, f.request("POST", hostedSettlementPath(attemptProject, a), f.admin, "", raw), 400, "idempotency_key_required")
	large := hostedSettlementBody(a)
	large["reason"] = strings.Repeat("<", 1024)
	large["evidence_reference"] = strings.Repeat("&", 1024)
	settlementError(t, hostedSettlementCall(t, f, a, "audit-budget", large), 400, "settlement_audit_too_large")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("invalid arguments/audit budget changed ledger or reservations")
	}
	// Exercise the exact canonical event limit without weakening raw text bounds.
	base := hostedSettlementAudit{AttemptID: a.ID, OperationID: a.OperationID, ExpectedRevision: a.Revision, Provider: a.Provider, Account: a.Account, Anchor: *a.Anchor, ExternalID: a.ExternalID, ExternalActionStopped: true, EvidenceReference: "operator:stop"}
	remaining := 4096 - len(encoded(base))
	text := strings.Repeat("<", remaining/6) + strings.Repeat("x", remaining%6)
	if len(text) > 1024 || remaining < 1 {
		t.Fatal("unexpected fixture audit size")
	}
	valid := hostedSettlementBody(a)
	valid["reason"] = text
	valid["evidence_reference"] = "operator:stop"
	valid["reason"] = text + "x"
	settlementError(t, hostedSettlementCall(t, f, a, "audit-budget", valid), 400, "settlement_audit_too_large")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("one-byte audit overflow changed retained state")
	}
	valid["reason"] = text
	r := hostedSettlementCall(t, f, a, "audit-budget", valid)
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	var payload string
	if err := f.store.DB().QueryRow(`SELECT payload_json FROM events WHERE event_type='review.hosted_settled'`).Scan(&payload); err != nil || len(payload) != 4096 {
		t.Fatal("canonical boundary audit lost content", len(payload), err)
	}
}

func TestHostedSettlementGuardTargetAndRetainedBindingFailures(t *testing.T) {
	f, a, _ := hostedSettlementFixture(t, true)
	before := hostedObservationState(t, f, a)
	stale := hostedSettlementBody(a)
	stale["expected_revision"] = a.Revision - 1
	r := hostedSettlementCall(t, f, a, "stale", stale)
	settlementError(t, r, 409, "review_attempt_revision_conflict")
	if !strings.Contains(r.Body.String(), fmt.Sprintf(`"current_revision":%d`, a.Revision)) {
		t.Fatal("stale attempt guard lost current revision")
	}
	for _, change := range []map[string]any{{"expected_repository_id": 802}, {"expected_head_sha": strings.Repeat("f", 40)}, {"expected_external_id": "102"}} {
		body := hostedSettlementBody(a)
		for k, v := range change {
			body[k] = v
		}
		settlementError(t, hostedSettlementCall(t, f, a, "mismatch", body), 409, "review_settlement_unavailable")
	}
	settlementError(t, f.request("POST", hostedSettlementPath(otherAttemptProject, a), f.admin, "wrong-project", encoded(hostedSettlementBody(a))), 404, "not_found")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("stale or wrong-target request changed journal")
	}
	for _, change := range []struct {
		name, mutate, restore string
		args                  []any
	}{
		{"CLI", `UPDATE review_operations SET channel='cli' WHERE id=?`, `UPDATE review_operations SET channel='hosted' WHERE id=?`, []any{a.OperationID}},
		{"live", `UPDATE review_operations SET state='active' WHERE id=?`, `UPDATE review_operations SET state='uncertain' WHERE id=?`, []any{a.OperationID}},
		{"link", `UPDATE review_operations SET attempt_id='' WHERE id=?`, `UPDATE review_operations SET attempt_id=? WHERE id=?`, []any{a.OperationID}},
		{"intent", `UPDATE review_execution_attempts SET intent='changed' WHERE id=?`, `UPDATE review_execution_attempts SET intent=? WHERE id=?`, []any{a.ID}},
		{"account reservation", `UPDATE review_execution_reservations SET account='changed' WHERE attempt_id=?`, `UPDATE review_execution_reservations SET account=? WHERE attempt_id=?`, []any{a.ID}},
		{"target reservation", `UPDATE review_execution_reservations SET pull_number=4 WHERE attempt_id=?`, `UPDATE review_execution_reservations SET pull_number=2 WHERE attempt_id=?`, []any{a.ID}},
		{"missing reservation", `DELETE FROM review_execution_reservations WHERE attempt_id=?`, `INSERT INTO review_execution_reservations(provider,account,action_class,repository_id,pull_number,attempt_id) VALUES(?,?,'hosted',?,?,?)`, []any{a.ID}},
		{"operation overflow", `UPDATE review_operations SET revision=9223372036854775807 WHERE id=?`, `UPDATE review_operations SET revision=? WHERE id=?`, []any{a.OperationID}},
		{"missing admission", `UPDATE review_execution_transitions SET reason='changed' WHERE attempt_id=? AND revision=2`, `UPDATE review_execution_transitions SET reason='admitted' WHERE attempt_id=? AND revision=2`, []any{a.ID}},
	} {
		t.Run(change.name, func(t *testing.T) {
			var originalRevision int64
			if err := f.store.DB().QueryRow(`SELECT revision FROM review_operations WHERE id=?`, a.OperationID).Scan(&originalRevision); err != nil {
				t.Fatal(err)
			}
			admissionSQL(t, f, change.mutate, change.args...)
			corrupt := hostedObservationState(t, f, a)
			settlementError(t, hostedSettlementCall(t, f, a, "retained-mismatch", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
			if hostedObservationState(t, f, a) != corrupt {
				t.Fatal("binding failure changed corrupt source")
			}
			args := change.args
			switch change.name {
			case "link":
				args = []any{a.ID, a.OperationID}
			case "intent":
				args = []any{a.Intent, a.ID}
			case "account reservation":
				args = []any{a.Account, a.ID}
			case "operation overflow":
				args = []any{originalRevision, a.OperationID}
			case "missing reservation":
				args = []any{a.Provider, a.Account, a.Anchor.RepositoryID, a.Anchor.PullNumber, a.ID}
			}
			admissionSQL(t, f, change.restore, args...)
		})
	}
	var originalBody string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_observations WHERE attempt_id=? AND position=100`, a.ID).Scan(&originalBody); err != nil {
		t.Fatal(err)
	}
	admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json=body_json||' ' WHERE attempt_id=? AND position=100`, a.ID)
	before = hostedObservationState(t, f, a)
	settlementError(t, hostedSettlementCall(t, f, a, "corrupt-journal", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("noncanonical journal was rewritten or released")
	}
	admissionSQL(t, f, `UPDATE review_hosted_observations SET body_json=? WHERE attempt_id=? AND position=100`, originalBody, a.ID)
	// A contiguous199-reference journal remains unavailable, rather than extending or
	// rewriting the bounded raw-reference journal during settlement.
	admissionSQL(t, f, `DELETE FROM review_hosted_observations WHERE attempt_id=? AND position=200`, a.ID)
	before = hostedObservationState(t, f, a)
	settlementError(t, hostedSettlementCall(t, f, a, "journal-hole", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("journal hole was repaired or released")
	}
}

type settlementBodyReader struct {
	io.Reader
	once   sync.Once
	before func()
}

func (r *settlementBodyReader) Read(p []byte) (int, error) {
	r.once.Do(r.before)
	return r.Reader.Read(p)
}
func TestHostedSettlementRequiresCurrentAdminIncludingReplayAndTransaction(t *testing.T) {
	f, a, _ := hostedSettlementFixture(t, false)
	before := hostedObservationState(t, f, a)
	for _, token := range []string{f.writer, f.reader, f.other, ""} {
		settlementError(t, f.request("POST", hostedSettlementPath(attemptProject, a), token, "denied", encoded(hostedSettlementBody(a))), map[bool]int{true: 401, false: 403}[token == ""], map[bool]string{true: "unauthorized", false: "forbidden"}[token == ""])
	}
	create := httptest.NewRequest("POST", "http://127.0.0.1/", nil)
	create.SetPathValue("project_id", attemptProject)
	create.Header.Set("Authorization", "Bearer "+f.admin)
	create.Header.Set("X-FC-Instance", f.store.InstanceID())
	session, cookie, err := f.store.CreateBrowserSession(create, strings.Repeat("a", 32), false)
	if err != nil {
		t.Fatal(err)
	}
	browser := httptest.NewRequest("POST", "http://127.0.0.1"+hostedSettlementPath(attemptProject, a), strings.NewReader(encoded(hostedSettlementBody(a))))
	browser.Header.Set("X-FC-Browser-Session", session.SessionID)
	browser.Header.Set("X-FC-Instance", f.store.InstanceID())
	browser.AddCookie(cookie)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, browser)
	settlementError(t, w, 403, "forbidden")
	wrong := httptest.NewRequest("POST", hostedSettlementPath(attemptProject, a), strings.NewReader(encoded(hostedSettlementBody(a))))
	wrong.Header.Set("Authorization", "Bearer "+f.admin)
	wrong.Header.Set("X-FC-Instance", "different-instance")
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, wrong)
	settlementError(t, w, 409, "instance_mismatch")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("denied authority changed journal")
	}
	reader := &settlementBodyReader{Reader: strings.NewReader(encoded(hostedSettlementBody(a))), before: func() { admissionSQL(t, f, `UPDATE credentials SET is_admin=0 WHERE is_admin=1`) }}
	req := httptest.NewRequest("POST", hostedSettlementPath(attemptProject, a), reader)
	req.Header.Set("Authorization", "Bearer "+f.admin)
	req.Header.Set("Idempotency-Key", "admin-demoted-after-auth")
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)
	settlementError(t, w, 403, "forbidden")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("transaction failed to recheck administrator")
	}
	admissionSQL(t, f, `UPDATE credentials SET is_admin=1 WHERE is_admin=0 AND id NOT IN ('exec-writer-cred','exec-reader-cred','exec-other-cred')`)
	r := hostedSettlementCall(t, f, a, "current-admin", hostedSettlementBody(a))
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	before = hostedObservationState(t, f, a)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE is_admin=1`)
	settlementError(t, hostedSettlementCall(t, f, a, "current-admin", hostedSettlementBody(a)), 401, "unauthorized")
	if hostedObservationState(t, f, a) != before {
		t.Fatal("revoked exact replay revived authority")
	}
}

func hostedSettlementFinalInput(a Attempt, input HostedObservationInput) ReviewEvidenceInput {
	return ReviewEvidenceInput{CaptureID: input.CaptureID, CaptureSHA256: input.CaptureSHA256, CaptureBytes: input.CaptureBytes, StartedAt: input.StartedAt, FinishedAt: input.FinishedAt, ExpectedAnchor: *a.Anchor, Review: reviewprovider.ParsedReview{Outcome: "completed", Complete: true, ReviewedFiles: []string{}, Findings: []reviewprovider.ParsedFinding{}}, Hosted: &HostedEvidenceMetadata{PublicationID: "11111111-1111-1111-1111-111111111111", ProviderAttemptID: "22222222-2222-2222-2222-222222222222", TriggerCommentID: 101, ReviewIDs: []int64{201}, ObservedBaseSHA: a.Anchor.ParentSHA, Sources: []HostedFindingSource{}}}
}
func TestHostedSettlementFinalEvidenceAndConcurrentGuards(t *testing.T) {
	t.Run("retained final evidence", func(t *testing.T) {
		f, a, input := hostedSettlementFixture(t, true)
		if _, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, hostedSettlementFinalInput(a, input)); err != nil {
			t.Fatal(err)
		}
		before := hostedObservationState(t, f, a)
		settlementError(t, hostedSettlementCall(t, f, a, "already-captured", hostedSettlementBody(a)), 409, "review_settlement_unavailable")
		if hostedObservationState(t, f, a) != before {
			t.Fatal("settlement changed retained final capture")
		}
	})
	t.Run("two operators", func(t *testing.T) {
		f, a, _ := hostedSettlementFixture(t, false)
		start := make(chan struct{})
		results := make(chan *httptest.ResponseRecorder, 2)
		for i := range 2 {
			go func() {
				<-start
				results <- hostedSettlementCall(t, f, a, fmt.Sprint("operator-", i), hostedSettlementBody(a))
			}()
		}
		close(start)
		success := 0
		for range 2 {
			r := <-results
			if r.Code == 200 {
				success++
			} else {
				settlementError(t, r, 409, "review_attempt_revision_conflict")
			}
		}
		if success != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.hosted_settled'`) != 1 {
			t.Fatal("concurrent operators repeated effect")
		}
	})
	t.Run("capture racing operator", func(t *testing.T) {
		f, a, input := hostedSettlementFixture(t, true)
		start := make(chan struct{})
		capture := make(chan error, 1)
		settle := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			<-start
			_, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, hostedSettlementFinalInput(a, input))
			capture <- err
		}()
		go func() { <-start; settle <- hostedSettlementCall(t, f, a, "capture-race", hostedSettlementBody(a)) }()
		close(start)
		err, r := <-capture, <-settle
		if r.Code == 200 {
			attemptCode(t, err, "review_conflict")
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 {
				t.Fatal("final capture appeared after settlement")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			settlementError(t, r, 409, "review_settlement_unavailable")
			if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 {
				t.Fatal("capture race released reservation")
			}
		}
	})
}

func TestHostedSettlementEventFailureRollsBackAndClockFloorPreservesHistory(t *testing.T) {
	f, a, _ := hostedSettlementFixture(t, false)
	before := hostedObservationState(t, f, a)
	admissionSQL(t, f, `CREATE TRIGGER settlement_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.hosted_settled' BEGIN SELECT RAISE(FAIL,'synthetic audit unavailable'); END`)
	r := hostedSettlementCall(t, f, a, "retry-after-rollback", hostedSettlementBody(a))
	if r.Code < 500 {
		t.Fatal("failed audit was acknowledged", r.Body.String())
	}
	if hostedObservationState(t, f, a) != before {
		t.Fatal("failed audit partially changed attempt/operation/transition/reservation/receipt")
	}
	admissionSQL(t, f, `DROP TRIGGER settlement_event_failure`)
	var previous string
	if err := f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, a.ID, a.Revision).Scan(&previous); err != nil {
		t.Fatal(err)
	}
	f.e.attemptNow = func() time.Time { return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC) }
	r = hostedSettlementCall(t, f, a, "retry-after-rollback", hostedSettlementBody(a))
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	var transition, operation string
	if err := f.store.DB().QueryRow(`SELECT t.created_at,o.updated_at FROM review_execution_transitions t JOIN review_execution_attempts a ON a.id=t.attempt_id JOIN review_operations o ON o.id=a.operation_id WHERE a.id=? AND t.revision=?`, a.ID, a.Revision+1).Scan(&transition, &operation); err != nil || transition != previous || operation != previous {
		t.Fatal("settlement bypassed existing chronology floor", transition, operation, previous, err)
	}
}
