package reviewruntime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/benhook1013/FireController/internal/core"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type terminalRecoveryFixture struct {
	f       *runtimeAllowanceFixture
	record  HostedCaptureRecord
	ref     CaptureReference
	request map[string]any
	mux     *http.ServeMux
}

func terminalHostedFixture(t *testing.T) terminalRecoveryFixture {
	t.Helper()
	f, record, ref, _ := retainedHostedRecoveryFixture(t)
	if _, err := f.f.store.DB().Exec(`UPDATE review_execution_transitions SET reason='trigger_recorded' WHERE attempt_id=? AND state='active'`, f.a.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(reviewexec.HostedIncompleteGrace + 5*time.Second)
	engine, err := reviewexec.New(f.f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection(), reviewexec.WithExecutionClock(func() time.Time { return now }), reviewexec.WithHostedRetainedDecoder(HostedRetainedDecoder(f.captures)))
	if err != nil {
		t.Fatal(err)
	}
	f.f.engine = engine
	// Model the prior decoder's audited release. The release implementation has
	// its own grace/atomicity tests; this fixture retains its exact journal form.
	var first string
	if err = f.f.store.DB().QueryRow(`SELECT created_at FROM review_execution_transitions WHERE attempt_id=? AND revision=?`, f.a.ID, f.a.Revision).Scan(&first); err != nil {
		t.Fatal(err)
	}
	at := now.Format(time.RFC3339Nano)
	err = f.f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		f.a.State = "failed"
		f.a.Revision++
		if _, err := tx.ExecContext(ctx, `UPDATE review_execution_attempts SET state='failed',revision=? WHERE id=?`, f.a.Revision, f.a.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_operations SET state='failed',revision=revision+1,reason=?,updated_at=? WHERE id=?`, reviewexec.HostedFinishedIncompleteReason, at, f.a.OperationID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_execution_transitions(attempt_id,revision,state,reason,external_id,created_at,source_refusal_json) VALUES(?,?,'failed',?,?,?,'')`, f.a.ID, f.a.Revision, reviewexec.HostedFinishedIncompleteReason, f.a.ExternalID, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM review_execution_reservations WHERE attempt_id=?`, f.a.ID); err != nil {
			return err
		}
		return core.AppendEvent(ctx, tx, core.Scope{ProjectID: f.f.project, PrincipalID: "synthetic-runtime-admin"}, "review.hosted_incomplete_released", f.a.ID, f.a.Revision, map[string]any{"operation_id": f.a.OperationID, "external_id": f.a.ExternalID, "capture_id": ref.ID, "capture_sha256": ref.SHA256, "first_observed_at": first, "released_at": at})
	})
	if err != nil {
		t.Fatal(err)
	}
	// Production already retains the failed archive version. The recovery must
	// append a narrowly authorized completed version without rewriting it.
	if _, err = engine.PublishAttempt(t.Context(), f.f.project, f.a.ID, nil); err != nil {
		t.Fatal("publish prior failed archive", err)
	}
	mux := http.NewServeMux()
	engine.Register(mux)
	req := map[string]any{"expected_revision": f.a.Revision, "operation_id": f.a.OperationID, "capability_id": "runtime-capability", "repository_id": f.a.Anchor.RepositoryID, "head_sha": f.a.Anchor.CandidateSHA, "external_id": f.a.ExternalID, "capture_sha256": ref.SHA256}
	return terminalRecoveryFixture{f, record, ref, req, mux}
}
func (f terminalRecoveryFixture) call(t *testing.T, token, key string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(f.request)
	r := httptest.NewRequest("POST", "/api/v1/projects/"+f.f.f.project+"/reviews/attempts/"+f.f.a.ID+"/retained-recovery", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	return w
}
func TestTerminalHostedRecoveryOriginalEvidenceAndReplay(t *testing.T) {
	f := terminalHostedFixture(t)
	path := filepath.Join(f.f.captures.root, hostedCaptureFilename(f.f.a.ID, f.ref))
	before, _ := os.ReadFile(path)
	result := f.call(t, f.f.f.admin, "original-repair")
	if result.Code != 200 {
		t.Fatalf("%d %s", result.Code, result.Body)
	}
	var receipt struct {
		EvidenceID string `json:"evidence_id"`
		AttemptID  string `json:"attempt_id"`
		State      string `json:"state"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.State != "completed" || receipt.AttemptID != f.f.a.ID || receipt.EvidenceID == "" {
		t.Fatal(receipt)
	}
	evidence, err := f.f.f.engine.ReadReviewEvidence(t.Context(), f.f.f.scopeWithAccess(t, f.f.f.reader, false, "read"), receipt.EvidenceID)
	if err != nil || evidence.Capture.CaptureSHA256 != f.ref.SHA256 || evidence.Capture.CaptureBytes != f.ref.Bytes || evidence.OperationID != f.f.a.OperationID || evidence.Capture.FinishedAt != f.record.Snapshot.IssueComments[2].UpdatedAt.Format(time.RFC3339Nano) || len(evidence.Capture.Review.Findings) != 0 {
		t.Fatal("changed source", evidence, err)
	}
	var linked string
	if err = f.f.f.store.DB().QueryRow(`SELECT source_run_id FROM review_attempts a JOIN review_workspace w ON w.project_id=a.project_id AND w.snapshot_id=a.snapshot_id WHERE a.project_id=? AND a.source_attempt_id=?`, f.f.f.project, f.f.a.ID).Scan(&linked); err != nil || linked != receipt.EvidenceID {
		t.Fatal("completed attempt not linked to evidence", linked, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) || recoveryRuntimeCount(t, f.f.f, "review_hosted_observations") != 2 || recoveryRuntimeCount(t, f.f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f.f, "review_execution_reservations") != 0 || recoveryRuntimeCount(t, f.f.f, "review_round_completions") != 0 {
		t.Fatal("invented source or credit")
	}
	events := recoveryRuntimeCount(t, f.f.f, "events")
	// Receipt replay works even if raw bytes later become unavailable. It does not
	// need a provider or re-decode source which was already committed atomically.
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	replay := f.call(t, f.f.f.admin, "original-repair")
	if replay.Code != 200 || replay.Body.String() != result.Body.String() || recoveryRuntimeCount(t, f.f.f, "events") != events {
		t.Fatal("non-idempotent replay", replay.Code, replay.Body)
	}
	f.request["capture_sha256"] = strings.Repeat("f", 64)
	if changed := f.call(t, f.f.f.admin, "original-repair"); changed.Code != 409 {
		t.Fatal("changed replay accepted", changed.Code, changed.Body)
	}
}
func TestTerminalHostedRecoveryRefusesWithoutMutation(t *testing.T) {
	for _, kind := range []string{"worker", "wrong revision", "wrong operation", "wrong head", "wrong capture", "wrong capability", "missing release", "wrong release capture", "wrong release trigger", "wrong transition", "corrupt capture", "corrupt baseline", "missing baseline", "latest source mismatch", "orphan reservation", "pending replacement", "unsupported inventory", "event rollback", "projection rollback", "missing current metadata"} {
		t.Run(kind, func(t *testing.T) {
			f := terminalHostedFixture(t)
			token := f.f.f.admin
			switch kind {
			case "worker":
				token = f.f.f.writer
			case "wrong revision":
				f.request["expected_revision"] = int64(99)
			case "wrong operation":
				f.request["operation_id"] = "review_operation_" + strings.Repeat("f", 32)
			case "wrong head":
				f.request["head_sha"] = strings.Repeat("f", 40)
			case "wrong capture":
				f.request["capture_sha256"] = strings.Repeat("f", 64)
			case "wrong capability":
				f.request["capability_id"] = "missing-capability"
			case "missing release":
				if _, err := f.f.f.store.DB().Exec(`DELETE FROM events WHERE event_type='review.hosted_incomplete_released'`); err != nil {
					t.Fatal(err)
				}
			case "wrong release capture", "wrong release trigger":
				field := "capture_sha256"
				value := strings.Repeat("f", 64)
				if kind == "wrong release trigger" {
					field = "external_id"
					value = "99"
				}
				if _, err := f.f.f.store.DB().Exec(`UPDATE events SET payload_json=json_set(payload_json,?,?) WHERE event_type='review.hosted_incomplete_released'`, "$."+field, value); err != nil {
					t.Fatal(err)
				}
			case "wrong transition":
				if _, err := f.f.f.store.DB().Exec(`UPDATE review_execution_transitions SET reason='other_failure' WHERE attempt_id=? AND revision=?`, f.f.a.ID, f.f.a.Revision); err != nil {
					t.Fatal(err)
				}
			case "corrupt capture":
				if err := os.WriteFile(filepath.Join(f.f.captures.root, hostedCaptureFilename(f.f.a.ID, f.ref)), []byte("corrupt"), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt baseline", "missing baseline":
				observations, err := f.f.f.engine.ReadHostedObservations(t.Context(), f.f.f.project, f.f.a.ID)
				if err != nil {
					t.Fatal(err)
				}
				in := observations[0].Input
				path := filepath.Join(f.f.captures.root, hostedCaptureFilename(f.f.a.ID, CaptureReference{ID: in.CaptureID, SHA256: in.CaptureSHA256, Bytes: in.CaptureBytes}))
				if kind == "corrupt baseline" {
					err = os.WriteFile(path, []byte("corrupt baseline"), 0600)
				} else {
					err = os.Rename(path, path+".held")
				}
				if err != nil {
					t.Fatal(err)
				}
			case "latest source mismatch":
				if _, err := f.f.f.store.DB().Exec(`UPDATE review_hosted_observations SET capture_sha256=? WHERE attempt_id=? AND position=2`, strings.Repeat("f", 64), f.f.a.ID); err != nil {
					t.Fatal(err)
				}
			case "orphan reservation":
				if _, err := f.f.f.store.DB().Exec(`INSERT INTO review_execution_reservations(attempt_id,provider,account,action_class,repository_id,pull_number) VALUES(?,'coderabbit','runtime-account','hosted',?,?)`, f.f.a.ID, f.f.a.Anchor.RepositoryID, f.f.a.Anchor.PullNumber); err != nil {
					t.Fatal(err)
				}
			case "pending replacement":
				if _, err := f.f.f.engine.Enqueue(t.Context(), f.f.f.scope(t, f.f.f.writer, false), "pending-replacement", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.f.f.headSHA}); err != nil {
					t.Fatal(err)
				}
			case "unsupported inventory":
				r := f.record
				r.Snapshot.IssueComments[1].Body = "Unsupported inventory"
				ref, err := f.f.captures.Save(t.Context(), f.f.a.ID, r)
				if err != nil {
					t.Fatal(err)
				}
				f.request["capture_sha256"] = ref.SHA256
				var observationBody string
				if err = f.f.f.store.DB().QueryRow(`SELECT body_json FROM review_hosted_observations WHERE attempt_id=? AND position=2`, f.f.a.ID).Scan(&observationBody); err != nil {
					t.Fatal(err)
				}
				var observation reviewexec.HostedObservation
				if err = json.Unmarshal([]byte(observationBody), &observation); err != nil {
					t.Fatal(err)
				}
				observation.Input.CaptureID = ref.ID
				observation.Input.CaptureSHA256 = ref.SHA256
				observation.Input.CaptureBytes = ref.Bytes
				encoded, _ := json.Marshal(observation)
				if _, err = f.f.f.store.DB().Exec(`UPDATE review_hosted_observations SET capture_sha256=?,capture_bytes=?,body_json=? WHERE attempt_id=? AND position=2`, ref.SHA256, ref.Bytes, string(encoded), f.f.a.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = f.f.f.store.DB().Exec(`UPDATE events SET payload_json=json_set(payload_json,'$.capture_id',?,'$.capture_sha256',?) WHERE event_type='review.hosted_incomplete_released'`, ref.ID, ref.SHA256); err != nil {
					t.Fatal(err)
				}
			case "missing current metadata":
				if _, err := f.f.f.store.DB().Exec(`DELETE FROM review_workspace WHERE project_id=?`, f.f.f.project); err != nil {
					t.Fatal(err)
				}
			case "projection rollback":
				if _, err := f.f.f.store.DB().Exec(`CREATE TRIGGER fail_repair_projection BEFORE INSERT ON review_run_versions WHEN NEW.source='native-executor' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "event rollback":
				if _, err := f.f.f.store.DB().Exec(`CREATE TRIGGER fail_repair BEFORE INSERT ON events WHEN NEW.event_type='review.hosted_retained_recovered' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			evidence := recoveryRuntimeCount(t, f.f.f, "review_execution_evidence")
			events := recoveryRuntimeCount(t, f.f.f, "events")
			w := f.call(t, token, "refused-repair")
			if w.Code == 200 || recoveryRuntimeCount(t, f.f.f, "review_execution_evidence") != evidence || recoveryRuntimeCount(t, f.f.f, "events") != events {
				t.Fatal("unsafe recovery mutated", kind, w.Code, w.Body)
			}
		})
	}
}

func grantTerminalFixture(t *testing.T, f terminalRecoveryFixture) {
	t.Helper()
	zero := int64(0)
	one := int64(1)
	_, err := f.f.f.engine.ConfigureRoundControl(t.Context(), f.f.f.scope(t, f.f.f.admin, true), "fixture-grant", 2, "hosted", reviewexec.RoundControlRequest{ExpectedRevision: &zero, Action: "grant", Minimum: &zero, Maximum: &one})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.f.f.store.DB().Exec(`INSERT INTO review_round_operation_bindings(operation_id,project_id,pull_number,channel,epoch,grant_revision) VALUES(?,?,2,'hosted',1,1)`, f.f.a.OperationID, f.f.f.project); err != nil {
		t.Fatal(err)
	}
}
func TestTerminalHostedRecoveryFinalizesOriginalRoundOnce(t *testing.T) {
	f := terminalHostedFixture(t)
	grantTerminalFixture(t, f)
	w := f.call(t, f.f.f.admin, "one-original-recovery")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var receipt struct {
		EvidenceID string `json:"evidence_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	for range 2 {
		if _, err := f.f.f.engine.FinalizeReviewEvidence(t.Context(), f.f.f.scope(t, f.f.f.writer, false), "original-finalize", receipt.EvidenceID, reviewexec.FinalizeReviewRequest{ExpectedRevision: &zero}); err != nil {
			t.Fatal(err)
		}
	}
	control, err := f.f.f.engine.ReadRoundControl(t.Context(), f.f.f.scopeWithAccess(t, f.f.f.reader, false, "read"), 2, "hosted")
	if err != nil || control.Used != 1 || control.Maximum != 1 || recoveryRuntimeCount(t, f.f.f, "review_round_completions") != 1 {
		t.Fatal("not one original credit", control, err)
	}
	if again := f.call(t, f.f.f.admin, "one-original-recovery"); again.Code != 200 || again.Body.String() != w.Body.String() {
		t.Fatal(again.Code, again.Body)
	}
	if other := f.call(t, f.f.f.admin, "different-recovery-key"); other.Code == 200 {
		t.Fatal("terminal was newly repaired again")
	}
	if recoveryRuntimeCount(t, f.f.f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f.f.f, "review_round_completions") != 1 {
		t.Fatal("duplicate credit")
	}
}
func TestTerminalHostedRecoveryCurrentRoundFence(t *testing.T) {
	for _, kind := range []string{"exhausted", "missing binding"} {
		t.Run(kind, func(t *testing.T) {
			f := terminalHostedFixture(t)
			grantTerminalFixture(t, f)
			var query string
			switch kind {
			case "exhausted":
				query = `UPDATE review_round_controls SET used=1`
			case "changed epoch":
				query = `UPDATE review_round_controls SET epoch=2`
			case "missing binding":
				query = `DELETE FROM review_round_operation_bindings`
			case "changed original grant":
				query = `UPDATE review_round_operation_bindings SET grant_revision=2`
			}
			if _, err := f.f.f.store.DB().Exec(query); err != nil {
				t.Fatal(err)
			}
			w := f.call(t, f.f.f.admin, "fenced-repair")
			if w.Code == 200 || recoveryRuntimeCount(t, f.f.f, "review_execution_evidence") != 0 {
				t.Fatal(w.Code, w.Body)
			}
		})
	}
}

func TestTerminalHostedRecoveryPendingFinalizationOccupiesRemainingSlot(t *testing.T) {
	f := terminalHostedFixture(t)
	grantTerminalFixture(t, f)
	e := f.f.f.engine
	raw, err := e.Enqueue(t.Context(), f.f.f.scope(t, f.f.f.writer, false), "replacement-review", reviewexec.Request{ExpectedConfigurationRevision: 1, PullNumber: 2, Channel: "hosted", HeadSHA: f.f.f.headSHA})
	if err != nil {
		t.Fatal("replacement enqueue", err)
	}
	var receipt reviewexec.Receipt
	if err = json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	a, err := e.StartAttempt(t.Context(), f.f.f.project, receipt.ID)
	if err != nil {
		t.Fatal("start", err)
	}
	command, err := reviewprovider.HostedTriggerBody(reviewprovider.HostedTriggerIntent{OperationID: a.OperationID, AttemptID: a.ID, HeadSHA: f.f.f.headSHA})
	if err != nil {
		t.Fatal(err)
	}
	a, err = e.Admit(t.Context(), f.f.f.project, a.ID, a.Revision, f.f.anchor, command)
	if err != nil {
		t.Fatal("admit", err)
	}
	a, err = e.Advance(t.Context(), f.f.f.project, a.ID, a.Revision, "active", "trigger_recorded", "200")
	if err != nil {
		t.Fatal(err)
	}
	input, err := HostedRetainedDecoder(f.f.captures)(t.Context(), e, f.f.f.project, f.f.a.ID, "runtime-capability", f.ref.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	input.ExpectedAnchor = *a.Anchor
	input.CaptureSHA256 = strings.Repeat("e", 64)
	input.CaptureID = "review_capture_" + strings.Repeat("e", 32)
	input.Hosted.TriggerCommentID = 200
	input.Hosted.StandingSummary.ProviderRunID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	captured, err := e.RecordReviewEvidence(t.Context(), f.f.f.project, a.ID, input)
	if err != nil {
		t.Fatal("capture", err)
	}
	if _, err = e.PublishEvidence(t.Context(), f.f.f.project, captured.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Advance(t.Context(), f.f.f.project, a.ID, a.Revision, "completed", "hosted_evidence_observed", a.ExternalID); err != nil {
		t.Fatal(err)
	}
	control, err := e.ReadRoundControl(t.Context(), f.f.f.scopeWithAccess(t, f.f.f.reader, false, "read"), 2, "hosted")
	if err != nil || control.Used != 0 {
		t.Fatal(control, err)
	}
	w := f.call(t, f.f.f.admin, "must-not-overbook")
	if w.Code != 409 || recoveryRuntimeCount(t, f.f.f, "review_execution_evidence") != 1 {
		t.Fatal("unfinalized replacement slot was reused", w.Code, w.Body)
	}
}

func TestTerminalHostedRecoveryConcurrentKeys(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "same key", false: "different keys"}[same], func(t *testing.T) {
			f := terminalHostedFixture(t)
			start := make(chan struct{})
			results := make(chan *httptest.ResponseRecorder, 2)
			for i := range 2 {
				go func() {
					<-start
					key := "one-recovery"
					if !same && i == 1 {
						key = "other-recovery"
					}
					results <- f.call(t, f.f.f.admin, key)
				}()
			}
			close(start)
			success := 0
			var body string
			for range 2 {
				r := <-results
				if r.Code == 200 {
					success++
					if body != "" && body != r.Body.String() {
						t.Fatal("receipt changed")
					}
					body = r.Body.String()
				} else if r.Code != 409 {
					t.Fatal(r.Code, r.Body)
				}
			}
			want := 1
			if same {
				want = 2
			}
			if success != want || recoveryRuntimeCount(t, f.f.f, "review_execution_evidence") != 1 {
				t.Fatal("concurrent duplicate/missing recovery", success)
			}
		})
	}
}
func TestTerminalHostedRecoveryRevokedAdminCannotReplay(t *testing.T) {
	f := terminalHostedFixture(t)
	w := f.call(t, f.f.f.admin, "admin-repair")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if _, err := f.f.f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE is_admin=1`); err != nil {
		t.Fatal(err)
	}
	replay := f.call(t, f.f.f.admin, "admin-repair")
	if replay.Code != 401 && replay.Code != 403 {
		t.Fatal("revoked admin replayed", replay.Code, replay.Body)
	}
}
