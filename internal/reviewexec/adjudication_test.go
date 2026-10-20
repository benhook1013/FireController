package reviewexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func adjudicationRevision(n int64) *int64 { return &n }
func adjudicationSHA(s string) *string    { return &s }

func adjudicationFixture(t *testing.T, findings int, partial bool) (*attemptFixture, Attempt, string) {
	t.Helper()
	f, a, in := evidenceFixture(t, "cli", true)
	if err := EnsureAdjudicationSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	// Retain literal source text independently of the disposition projection.
	original := in.Review.Findings[0]
	in.Review.Findings = nil
	for i := 0; i < findings; i++ {
		finding := original
		finding.Title = fmt.Sprintf("Immutable finding %d", i)
		in.Review.Findings = append(in.Review.Findings, finding)
	}
	in.Review.ReportedFindings = int64(findings)
	if partial {
		in.Review.Outcome, in.Review.Complete = "failed", false
	}
	r, err := f.e.RecordReviewEvidence(context.Background(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	return f, a, r.ID
}

func adjudicationRead(t *testing.T, f *attemptFixture, id string) ReviewAdjudication {
	t.Helper()
	got, err := f.e.ReadReviewAdjudication(context.Background(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func adjudicationReceipt(t *testing.T, body json.RawMessage, err error, id, state string, revision int64) json.RawMessage {
	t.Helper()
	var r Receipt
	var fields map[string]any
	if err != nil || json.Unmarshal(body, &r) != nil || json.Unmarshal(body, &fields) != nil || len(fields) != 3 || r != (Receipt{ID: id, State: state, Revision: revision}) {
		t.Fatalf("compact receipt: %s %v", body, err)
	}
	return body
}

func adjudicationDecide(t *testing.T, f *attemptFixture, id, key string, revision, finding int64, disposition string) json.RawMessage {
	t.Helper()
	body, err := f.e.DecideReviewFinding(context.Background(), f.scope(t, attemptProject, f.writer, false), key, id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(revision), Finding: finding, Disposition: disposition, Reason: "Explicit reason <script>literal</script>"})
	return adjudicationReceipt(t, body, err, id, "adjudicated", revision+1)
}

func adjudicationComplete(t *testing.T, f *attemptFixture, a Attempt) {
	t.Helper()
	if _, err := f.e.Advance(context.Background(), attemptProject, a.ID, a.Revision, "completed", "test_capture_completed", ""); err != nil {
		t.Fatal(err)
	}
}

func adjudicationState(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var state, changes string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(body_json||revision,''),'') FROM review_adjudication_state`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(evidence_id||revision||actor_id||kind||finding_index||input_json||created_at,''),'') FROM review_adjudication_changes`).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	return evidenceState(t, f, a) + "/" + state + "/" + changes
}

func TestCLIAdjudicationDefaultsDecisionsAndReceiptReplay(t *testing.T) {
	f, a, id := adjudicationFixture(t, 2, false)
	ctx := context.Background()
	before := adjudicationState(t, f, a)
	initial := adjudicationRead(t, f, id)
	if initial.EvidenceID != id || initial.Revision != 0 || initial.Finalized || initial.FrozenCounts != nil || len(initial.Findings) != 2 || initial.Findings[0] != (FindingDecision{Finding: 0, Disposition: "unadjudicated"}) || initial.Findings[1].Finding != 1 || adjudicationState(t, f, a) != before {
		t.Fatal("default view allocated state or fabricated decisions", initial)
	}
	if strings.Contains(encoded(initial), "Immutable finding") || strings.Contains(encoded(initial), "source text") {
		t.Fatal("disposition read leaked original finding text")
	}
	journal := attemptState(t, f, a)
	scope := f.scope(t, attemptProject, f.writer, false)
	r := FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: "accepted", Reason: "Accepted with explicit rationale"}
	body, err := f.e.DecideReviewFinding(ctx, scope, "first-decision", id, r)
	first := adjudicationReceipt(t, body, err, id, "adjudicated", 1)
	state := adjudicationRead(t, f, id)
	if state.Findings[0].ActorID != scope.PrincipalID || state.Findings[0].DecidedAt == "" || state.Findings[0].Reason != r.Reason || state.Finalized {
		t.Fatal("decision attribution missing or implicitly finalized", state)
	}
	var oldAudit string
	if err := f.store.DB().QueryRow(`SELECT actor_id||input_json||created_at FROM review_adjudication_changes WHERE evidence_id=? AND revision=1`, id).Scan(&oldAudit); err != nil {
		t.Fatal(err)
	}
	adjudicationDecide(t, f, id, "second-decision", 1, 1, "rejected")
	settled := adjudicationState(t, f, a)
	replay, err := f.e.DecideReviewFinding(ctx, scope, "first-decision", id, r)
	if err != nil || !bytes.Equal(first, replay) || adjudicationState(t, f, a) != settled {
		t.Fatal("exact decision retry changed state", err)
	}
	r.Reason = "Changed original-key request"
	_, err = f.e.DecideReviewFinding(ctx, scope, "first-decision", id, r)
	attemptCode(t, err, "idempotency_conflict")
	_, err = f.e.DecideReviewFinding(ctx, scope, "stale-new-key", id, r)
	attemptCode(t, err, "review_conflict")
	if adjudicationState(t, f, a) != settled {
		t.Fatal("failed decision changed audit or source")
	}
	// A correction is attributed to its actual current actor; old attribution stays in audit.
	admin := f.scope(t, attemptProject, f.admin, true)
	r.ExpectedRevision, r.Reason = adjudicationRevision(2), "Correction by administrator"
	body, err = f.e.DecideReviewFinding(ctx, admin, "correction", id, r)
	adjudicationReceipt(t, body, err, id, "adjudicated", 3)
	if adjudicationRead(t, f, id).Findings[0].ActorID != admin.PrincipalID {
		t.Fatal("correction forged original actor")
	}
	var retainedAudit string
	if err := f.store.DB().QueryRow(`SELECT actor_id||input_json||created_at FROM review_adjudication_changes WHERE evidence_id=? AND revision=1`, id).Scan(&retainedAudit); err != nil || retainedAudit != oldAudit {
		t.Fatal("correction replaced prior audit", err)
	}
	// Events/receipts change, while capture bytes and the execution journal do not.
	if attemptState(t, f, a) != journal {
		t.Fatal("decision changed execution journal")
	}
	var capture string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, id).Scan(&capture); err != nil || !strings.Contains(capture, "Immutable finding 0") {
		t.Fatal("decision changed original source", err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations WHERE attempt_id=?`, a.ID) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_transitions WHERE attempt_id=?`, a.ID) != 2 {
		t.Fatal("decision settled execution")
	}
}

func TestCLIAdjudicationFinalizeFreezesSourceCounts(t *testing.T) {
	f, a, id := adjudicationFixture(t, 2, false)
	adjudicationDecide(t, f, id, "accepted", 0, 0, "accepted")
	adjudicationDecide(t, f, id, "rejected", 1, 1, "rejected")
	adjudicationComplete(t, f, a)
	journal := attemptState(t, f, a)
	var source string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, id).Scan(&source); err != nil {
		t.Fatal(err)
	}
	scope := f.scope(t, attemptProject, f.writer, false)
	request := FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)}
	body, err := f.e.FinalizeReviewEvidence(context.Background(), scope, "finalize", id, request)
	first := adjudicationReceipt(t, body, err, id, "finalized", 3)
	got := adjudicationRead(t, f, id)
	if !got.Finalized || got.FrozenCounts == nil || *got.FrozenCounts != (FindingCounts{Found: 2, Accepted: 1, Routed: 0}) {
		t.Fatal("finalization lost immutable counts", got)
	}
	adjudicationDecide(t, f, id, "post-finalize-correction", 3, 0, "rejected")
	got = adjudicationRead(t, f, id)
	if got.Findings[0].Disposition != "rejected" || *got.FrozenCounts != (FindingCounts{Found: 2, Accepted: 1}) {
		t.Fatal("later correction rewrote frozen source counts", got)
	}
	before := adjudicationState(t, f, a)
	replay, err := f.e.FinalizeReviewEvidence(context.Background(), scope, "finalize", id, request)
	if err != nil || !bytes.Equal(first, replay) || adjudicationState(t, f, a) != before {
		t.Fatal("finalization retry did not retain original receipt", err)
	}
	_, err = f.e.FinalizeReviewEvidence(context.Background(), scope, "second-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(4)})
	attemptCode(t, err, "review_conflict")
	var retained string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, id).Scan(&retained); err != nil || retained != source || attemptState(t, f, a) != journal || adjudicationState(t, f, a) != before {
		t.Fatal("finalization/correction changed source or journal", err)
	}
}

func TestCLIAdjudicationFinalizationRequiresCompleteSettledDecisions(t *testing.T) {
	for _, name := range []string{"active", "failed", "uncertain", "unknown", "partial", "unadjudicated", "zero"} {
		t.Run(name, func(t *testing.T) {
			n := 1
			if name == "zero" {
				n = 0
			}
			f, a, id := adjudicationFixture(t, n, name == "partial")
			revision := int64(0)
			if n > 0 && name != "unadjudicated" {
				adjudicationDecide(t, f, id, "decision", 0, 0, "rejected")
				revision = 1
			}
			switch name {
			case "failed", "uncertain":
				if _, err := f.e.Advance(context.Background(), attemptProject, a.ID, a.Revision, name, "test_status", ""); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if _, err := f.store.DB().Exec(`UPDATE review_execution_attempts SET state='unknown' WHERE id=?`, a.ID); err != nil {
					t.Fatal(err)
				}
			case "partial", "unadjudicated", "zero":
				adjudicationComplete(t, f, a)
			}
			before := adjudicationState(t, f, a)
			body, err := f.e.FinalizeReviewEvidence(context.Background(), f.scope(t, attemptProject, f.writer, false), "finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(revision)})
			if name == "zero" {
				adjudicationReceipt(t, body, err, id, "finalized", 1)
				got := adjudicationRead(t, f, id)
				if !got.Finalized || *got.FrozenCounts != (FindingCounts{}) || len(got.Findings) != 0 {
					t.Fatal("empty completed capture did not finalize", got)
				}
			} else {
				attemptCode(t, err, "review_conflict")
				if adjudicationState(t, f, a) != before {
					t.Fatal("ineligible finalization changed state")
				}
			}
		})
	}
}

func TestCLIAdjudicationFixProofCorrectionsAndWithdrawalGuard(t *testing.T) {
	f, a, id := adjudicationFixture(t, 2, true) // Partial evidence can retain decisions/proof, but not finalize.
	adjudicationDecide(t, f, id, "accept", 0, 0, "accepted")
	scope := f.scope(t, attemptProject, f.writer, false)
	shaA, shaB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	r := FindingFixRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: shaA, ProofNote: "First explicit proof"}
	body, err := f.e.RecordFindingFix(context.Background(), scope, "first-proof", id, r)
	first := adjudicationReceipt(t, body, err, id, "fix_recorded", 2)
	initial := adjudicationRead(t, f, id).Findings[0]
	if initial.FixSHA != shaA || initial.ProofNote != r.ProofNote || initial.FixedAt == "" || initial.ActorID != scope.PrincipalID {
		t.Fatal("fix lost proof or decision attribution", initial)
	}
	var firstAudit string
	if err := f.store.DB().QueryRow(`SELECT input_json FROM review_adjudication_changes WHERE evidence_id=? AND revision=2`, id).Scan(&firstAudit); err != nil {
		t.Fatal(err)
	}
	before := adjudicationState(t, f, a)
	wrong := r
	wrong.ExpectedRevision = adjudicationRevision(2)
	_, err = f.e.RecordFindingFix(context.Background(), scope, "wrong-expected", id, wrong)
	attemptCode(t, err, "review_conflict")
	_, err = f.e.DecideReviewFinding(context.Background(), scope, "reject-with-proof", id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(2), Finding: 0, Disposition: "rejected", Reason: "Cannot erase proof"})
	attemptCode(t, err, "review_conflict")
	if adjudicationState(t, f, a) != before {
		t.Fatal("proof conflict erased prior proof or audit")
	}
	correction := r
	correction.ExpectedRevision = adjudicationRevision(2)
	correction.ExpectedSHA = adjudicationSHA(shaA)
	correction.FixSHA = shaB
	correction.ProofNote = strings.Repeat("<", 2048)
	body, err = f.e.RecordFindingFix(context.Background(), scope, "correct-proof", id, correction)
	adjudicationReceipt(t, body, err, id, "fix_recorded", 3)
	correction.ExpectedRevision = adjudicationRevision(3)
	correction.ExpectedSHA = adjudicationSHA(shaB)
	correction.ProofNote = "Same SHA, corrected note"
	body, err = f.e.RecordFindingFix(context.Background(), scope, "note-correction", id, correction)
	adjudicationReceipt(t, body, err, id, "fix_recorded", 4)
	adjudicationDecide(t, f, id, "accepted-reason-correction", 4, 0, "accepted")
	got := adjudicationRead(t, f, id)
	if got.Findings[0].FixSHA != shaB || got.Findings[0].ProofNote != correction.ProofNote || got.Finalized || got.FrozenCounts != nil {
		t.Fatal("accepted correction lost proof or finalized partial evidence", got)
	}
	adjudicationDecide(t, f, id, "reject-second", 5, 1, "rejected")
	before = adjudicationState(t, f, a)
	bad := r
	bad.Finding = 1
	bad.ExpectedRevision = adjudicationRevision(6)
	_, err = f.e.RecordFindingFix(context.Background(), scope, "proof-for-rejected", id, bad)
	attemptCode(t, err, "review_conflict")
	replay, err := f.e.RecordFindingFix(context.Background(), scope, "first-proof", id, r)
	if err != nil || !bytes.Equal(replay, first) || adjudicationState(t, f, a) != before {
		t.Fatal("proof replay duplicated correction", err)
	}
	changed := r
	changed.ProofNote = "Different original key note"
	_, err = f.e.RecordFindingFix(context.Background(), scope, "first-proof", id, changed)
	attemptCode(t, err, "idempotency_conflict")
	var retained string
	if err := f.store.DB().QueryRow(`SELECT input_json FROM review_adjudication_changes WHERE evidence_id=? AND revision=2`, id).Scan(&retained); err != nil || retained != firstAudit || !strings.Contains(retained, shaA) {
		t.Fatal("proof correction replaced prior value", err)
	}
}

func TestCLIAdjudicationConcurrentCASAndAtomicFailures(t *testing.T) {
	t.Run("two_writers", func(t *testing.T) {
		f, a, id := adjudicationFixture(t, 1, false)
		scope := f.scope(t, attemptProject, f.writer, false)
		beforeJournal := attemptState(t, f, a)
		receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
		start := make(chan struct{})
		results := make(chan error, 2)
		for i, disposition := range []string{"accepted", "rejected"} {
			go func(i int, d string) {
				<-start
				_, err := f.e.DecideReviewFinding(context.Background(), scope, fmt.Sprintf("writer-%d", i), id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: d, Reason: "Concurrent explicit choice"})
				results <- err
			}(i, disposition)
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
		if winners != 1 || adjudicationRead(t, f, id).Revision != 1 || attemptCount(t, f.store, `SELECT count(*) FROM review_adjudication_changes`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.evidence.adjudicated'`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != receipts+1 || attemptState(t, f, a) != beforeJournal {
			t.Fatal("CAS produced duplicate or partial effects")
		}
	})
	for _, kind := range []string{"decision", "finalize", "fix"} {
		t.Run("rollback_"+kind, func(t *testing.T) {
			f, a, id := adjudicationFixture(t, 1, false)
			if kind != "decision" {
				adjudicationDecide(t, f, id, "initial", 0, 0, "accepted")
			}
			if kind == "finalize" {
				adjudicationComplete(t, f, a)
			}
			if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_adjudication_event BEFORE INSERT ON events WHEN NEW.event_type LIKE 'review.evidence.%' BEGIN SELECT RAISE(ABORT,'synthetic adjudication failure'); END`); err != nil {
				t.Fatal(err)
			}
			before := adjudicationState(t, f, a)
			scope := f.scope(t, attemptProject, f.writer, false)
			var err error
			switch kind {
			case "decision":
				_, err = f.e.DecideReviewFinding(context.Background(), scope, "rolled-back", id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: "accepted", Reason: "Explicit reason"})
			case "finalize":
				_, err = f.e.FinalizeReviewEvidence(context.Background(), scope, "rolled-back", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
			case "fix":
				_, err = f.e.RecordFindingFix(context.Background(), scope, "rolled-back", id, FindingFixRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("a", 40), ProofNote: "Proof"})
			}
			if err == nil || strings.Contains(err.Error(), "synthetic") || adjudicationState(t, f, a) != before {
				t.Fatal("event failure left projection/audit/receipt/source effects or leaked diagnostics", err)
			}
		})
	}
}

func TestCLIAdjudicationAuthorizationAndRestart(t *testing.T) {
	f, a, id := adjudicationFixture(t, 1, false)
	writer := f.scope(t, attemptProject, f.writer, false)
	reader := evidenceReadScope(t, f, attemptProject, f.reader)
	other := f.scope(t, otherAttemptProject, f.other, false)
	r := FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: "accepted", Reason: "Authorized explicit decision"}
	before := adjudicationState(t, f, a)
	for _, scope := range []core.Scope{reader, other} {
		code := "forbidden"
		if scope.ProjectID == otherAttemptProject {
			code = "not_found"
		}
		_, err := f.e.DecideReviewFinding(context.Background(), scope, "denied", id, r)
		attemptCode(t, err, code)
		_, err = f.e.FinalizeReviewEvidence(context.Background(), scope, "denied", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(0)})
		attemptCode(t, err, code)
		_, err = f.e.RecordFindingFix(context.Background(), scope, "denied", id, FindingFixRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("a", 40), ProofNote: "Proof"})
		attemptCode(t, err, code)
	}
	_, err := f.e.ReadReviewAdjudication(context.Background(), other, id)
	attemptCode(t, err, "not_found")
	forged := reader
	forged.ProjectID = otherAttemptProject
	_, err = f.e.ReadReviewAdjudication(context.Background(), forged, id)
	attemptCode(t, err, "forbidden")
	if adjudicationState(t, f, a) != before {
		t.Fatal("unauthorized operation changed state")
	}
	body, err := f.e.DecideReviewFinding(context.Background(), writer, "durable", id, r)
	first := adjudicationReceipt(t, body, err, id, "adjudicated", 1)
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id IN ('exec-reader-cred','exec-writer-cred')`); err != nil {
		t.Fatal(err)
	}
	before = adjudicationState(t, f, a)
	_, err = f.e.DecideReviewFinding(context.Background(), writer, "durable", id, r)
	attemptCode(t, err, "unauthorized")
	_, err = f.e.ReadReviewAdjudication(context.Background(), reader, id)
	attemptCode(t, err, "unauthorized")
	if adjudicationState(t, f, a) != before {
		t.Fatal("revoked replay changed state")
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=0 WHERE id IN ('exec-reader-cred','exec-writer-cred')`); err != nil {
		t.Fatal(err)
	}
	adjudicationComplete(t, f, a)
	body, err = f.e.FinalizeReviewEvidence(context.Background(), writer, "finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, id, "finalized", 2)
	state := adjudicationRead(t, f, id)
	before = adjudicationState(t, f, a)
	var sequence int
	var name, path string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(context.Background(), filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	for i := 0; i < 2; i++ {
		if err := EnsureSchema(context.Background(), store); err != nil {
			t.Fatal(err)
		}
		if err := EnsureEvidenceSchema(context.Background(), store); err != nil {
			t.Fatal(err)
		}
		if err := EnsureAdjudicationSchema(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}
	f.e, err = New(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state, adjudicationRead(t, f, id)) || before != adjudicationState(t, f, a) {
		t.Fatal("restart/schema init rewrote state or audit")
	}
	replay, err := f.e.DecideReviewFinding(context.Background(), f.scope(t, attemptProject, f.writer, false), "durable", id, r)
	if err != nil || !bytes.Equal(first, replay) || adjudicationState(t, f, a) != before {
		t.Fatal("restart lost exact historical receipt", err)
	}
}

func TestCLIAdjudicationValidationAndRevisionOverflow(t *testing.T) {
	f, a, id := adjudicationFixture(t, 1, false)
	scope := f.scope(t, attemptProject, f.writer, false)
	valid := FindingDecisionRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, Disposition: "accepted", Reason: "Reason"}
	cases := map[string]func(*FindingDecisionRequest){
		"missing_revision":  func(r *FindingDecisionRequest) { r.ExpectedRevision = nil },
		"negative_revision": func(r *FindingDecisionRequest) { r.ExpectedRevision = adjudicationRevision(-1) },
		"negative_finding":  func(r *FindingDecisionRequest) { r.Finding = -1 },
		"large_finding":     func(r *FindingDecisionRequest) { r.Finding = 100 },
		"missing_finding":   func(r *FindingDecisionRequest) { r.Finding = 1 },
		"routed":            func(r *FindingDecisionRequest) { r.Disposition = "routed" },
		"case":              func(r *FindingDecisionRequest) { r.Disposition = "Accepted" },
		"empty_reason":      func(r *FindingDecisionRequest) { r.Reason = " \n" },
		"large_reason":      func(r *FindingDecisionRequest) { r.Reason = strings.Repeat("é", 1025) },
		"control":           func(r *FindingDecisionRequest) { r.Reason = "unsafe\x00" },
		"utf8":              func(r *FindingDecisionRequest) { r.Reason = "unsafe\xff" },
	}
	before := adjudicationState(t, f, a)
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := valid
			change(&r)
			_, err := f.e.DecideReviewFinding(context.Background(), scope, "invalid", id, r)
			attemptCode(t, err, "validation_failed")
			if adjudicationState(t, f, a) != before || strings.Contains(err.Error(), "unsafe") {
				t.Fatal("invalid decision changed state or leaked input")
			}
		})
	}
	fix := FindingFixRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("a", 40), ProofNote: "Proof"}
	fixCases := map[string]func(*FindingFixRequest){
		"revision":          func(r *FindingFixRequest) { r.ExpectedRevision = nil },
		"negative_revision": func(r *FindingFixRequest) { r.ExpectedRevision = adjudicationRevision(-1) },
		"expected_missing":  func(r *FindingFixRequest) { r.ExpectedSHA = nil },
		"expected_invalid":  func(r *FindingFixRequest) { r.ExpectedSHA = adjudicationSHA("main") },
		"sha":               func(r *FindingFixRequest) { r.FixSHA = strings.Repeat("A", 40) },
		"empty_note":        func(r *FindingFixRequest) { r.ProofNote = " \n" },
		"large_note":        func(r *FindingFixRequest) { r.ProofNote = strings.Repeat("x", 2049) },
		"control_note":      func(r *FindingFixRequest) { r.ProofNote = "unsafe\x00" },
		"utf8_note":         func(r *FindingFixRequest) { r.ProofNote = "unsafe\xff" },
		"finding":           func(r *FindingFixRequest) { r.Finding = -1 },
	}
	for name, change := range fixCases {
		t.Run("fix_"+name, func(t *testing.T) {
			r := fix
			change(&r)
			_, err := f.e.RecordFindingFix(context.Background(), scope, "invalid", id, r)
			attemptCode(t, err, "validation_failed")
			if adjudicationState(t, f, a) != before {
				t.Fatal("invalid fix changed state")
			}
		})
	}
	for _, revision := range []*int64{nil, adjudicationRevision(-1)} {
		_, err := f.e.FinalizeReviewEvidence(context.Background(), scope, "invalid", id, FinalizeReviewRequest{ExpectedRevision: revision})
		attemptCode(t, err, "validation_failed")
	}
	_, err := f.e.ReadReviewAdjudication(context.Background(), scope, "bad/id")
	attemptCode(t, err, "validation_failed")
	_, err = f.e.DecideReviewFinding(context.Background(), scope, "bad-id", "bad/id", valid)
	attemptCode(t, err, "validation_failed")
	_, err = f.e.DecideReviewFinding(context.Background(), scope, "missing", "review_evidence_missing", valid)
	attemptCode(t, err, "not_found")
	valid.Reason = strings.Repeat("<", 2048)
	body, err := f.e.DecideReviewFinding(context.Background(), scope, "boundary-reason", id, valid)
	adjudicationReceipt(t, body, err, id, "adjudicated", 1)
	state := adjudicationRead(t, f, id)
	state.Revision = math.MaxInt64
	if _, err := f.store.DB().Exec(`UPDATE review_adjudication_state SET revision=?,body_json=? WHERE evidence_id=?`, state.Revision, encoded(state), id); err != nil {
		t.Fatal(err)
	}
	before = adjudicationState(t, f, a)
	valid.ExpectedRevision = adjudicationRevision(math.MaxInt64)
	_, err = f.e.DecideReviewFinding(context.Background(), scope, "overflow", id, valid)
	attemptCode(t, err, "review_conflict")
	_, err = f.e.FinalizeReviewEvidence(context.Background(), scope, "overflow", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(math.MaxInt64)})
	attemptCode(t, err, "review_conflict")
	fix.ExpectedRevision = adjudicationRevision(math.MaxInt64)
	_, err = f.e.RecordFindingFix(context.Background(), scope, "overflow", id, fix)
	attemptCode(t, err, "review_conflict")
	if adjudicationState(t, f, a) != before {
		t.Fatal("revision overflow changed state or allocated receipt")
	}
}

// Read the real scoped project feed around lifecycle mutations, including every
// emitted item. One malformed producer envelope would reject the browser page.
func producerProjectCursor(t *testing.T, f *attemptFixture, scope core.Scope) string {
	t.Helper()
	var cursor string
	if err := f.store.Read(t.Context(), scope, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		cursor, err = f.store.ProjectEventCursor(ctx, tx, scope)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return cursor
}

func assertProducerProjectPage(t *testing.T, f *attemptFixture, scope core.Scope, cursor string, want []core.ProjectEventItem) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	stop := errors.New("bounded producer event page received")
	var page core.ProjectEventPage
	err := f.store.StreamProjectEvents(ctx, scope, cursor, 100, func(value core.ProjectEventPage) error { page = value; return stop }, func() error { return errors.New("producer page missing") })
	if !errors.Is(err, stop) || page.HasMore || len(page.Items) != len(want) {
		t.Fatalf("producer event page: %+v %v", page, err)
	}
	validID := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	seen := map[string]bool{cursor: true}
	remaining := make(map[core.ProjectEventItem]int, len(want))
	for _, item := range want {
		remaining[item]++
	}
	for _, item := range page.Items {
		key := item
		key.EventCursor = ""
		if remaining[key] == 0 || !validID.MatchString(item.ResourceID) || item.Revision < 1 || item.Revision > 9007199254740991 {
			t.Fatalf("browser-incompatible or wrong producer item: %+v", item)
		}
		remaining[key]--
		if item.EventCursor == "" || len(item.EventCursor) > 4096 || strings.ContainsAny(item.EventCursor, " \t\r\n") || seen[item.EventCursor] {
			t.Fatalf("invalid or repeated scoped event cursor: %+v", item)
		}
		seen[item.EventCursor] = true
	}
	if page.EventCursor == cursor || page.EventCursor != producerProjectCursor(t, f, scope) {
		t.Fatal("event page did not advance to current scoped cursor")
	}
	// Resume the returned cursor through the same public feed, without parsing it.
	var resumed core.ProjectEventPage
	err = f.store.StreamProjectEvents(ctx, scope, page.EventCursor, 100, func(value core.ProjectEventPage) error { resumed = value; return stop }, func() error { return errors.New("resume page missing") })
	if !errors.Is(err, stop) || len(resumed.Items) != 0 || resumed.HasMore || resumed.EventCursor != page.EventCursor {
		t.Fatalf("returned producer cursor cannot resume cleanly: %+v %v", resumed, err)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, item := range wire.Items {
		if len(item) != 4 || item["event_type"] == nil || item["resource_id"] == nil || item["revision"] == nil || item["event_cursor"] == nil {
			t.Fatal("project feed exposed non-envelope fields")
		}
	}
}

func TestCLIAdjudicationRealProducerMetadataEnvelope(t *testing.T) {
	f, a, id := adjudicationFixture(t, 1, false)
	adjudicationComplete(t, f, a)
	reader := evidenceReadScope(t, f, attemptProject, f.reader)
	before := producerProjectCursor(t, f, reader)
	adjudicationDecide(t, f, id, "producer-decision", 0, 0, "rejected")
	body, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "producer-finalization", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, body, err, id, "finalized", 2)
	// The original decision replay must not add a second envelope after finalize.
	adjudicationDecide(t, f, id, "producer-decision", 0, 0, "rejected")
	assertProducerProjectPage(t, f, reader, before, []core.ProjectEventItem{
		{EventType: "review.evidence.adjudicated", ResourceID: id, Revision: 1},
		{EventType: "review.evidence.finalized", ResourceID: id, Revision: 2},
	})
}
