package reviewexec

import (
	"bytes"
	"fmt"
	"github.com/benhook1013/FireController/internal/core"
	"strings"
	"testing"
)

func TestOperationalFixAuthenticatedReplayAndReadiness(t *testing.T) {
	f, a, id, current := coverageFixture(t, 1, false)
	adjudicationDecide(t, f, id, "accept", 0, 0, "accepted")
	writer := f.scope(t, attemptProject, f.writer, false)
	r := OperationalFixRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, ExpectedDigest: adjudicationSHA(""), ProofNote: "Verified obsolete repository credential entry absent; issuer revocation is separate."}
	for _, scope := range []core.Scope{evidenceReadScope(t, f, attemptProject, f.reader), f.scope(t, otherAttemptProject, f.other, false)} {
		_, err := f.e.RecordOperationalFix(t.Context(), scope, "denied", id, r)
		if err == nil {
			t.Fatal("unauthorized proof accepted")
		}
	}
	before := adjudicationState(t, f, a)
	bad := r
	bad.ExpectedDigest = adjudicationSHA(strings.Repeat("a", 64))
	if _, err := f.e.RecordOperationalFix(t.Context(), writer, "wrong-digest", id, bad); err == nil {
		t.Fatal("wrong digest accepted")
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("failed guard mutated state")
	}
	first, err := f.e.RecordOperationalFix(t.Context(), writer, "operation-1", id, r)
	adjudicationReceipt(t, first, err, id, "operational_fix_recorded", 2)
	second, err := f.e.RecordOperationalFix(t.Context(), writer, "operation-1", id, r)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("replay: %s %v", second, err)
	}
	bad = r
	bad.ProofNote = "different action"
	if _, err := f.e.RecordOperationalFix(t.Context(), writer, "operation-1", id, bad); err == nil {
		t.Fatal("key body mismatch accepted")
	}
	if _, err := f.e.RecordOperationalFix(t.Context(), writer, "stale", id, r); err == nil {
		t.Fatal("stale revision accepted")
	}
	state := adjudicationRead(t, f, id)
	got := state.Findings[0]
	if got.FixSHA != "" || got.ProofNote != "" || got.FixedAt != "" || got.OperationalProof == nil ||
		got.OperationalProof.Digest != operationalDigest(r.ProofNote) || got.OperationalProof.ActorID != writer.PrincipalID ||
		!validOperationalDecision(got) {
		t.Fatalf("invalid operational readback: %+v", got)
	}
	if _, err := f.e.RecordFindingFix(t.Context(), writer, "cannot-replace-with-code", id, FindingFixRequest{ExpectedRevision: adjudicationRevision(2), Finding: 0, ExpectedSHA: adjudicationSHA(""), FixSHA: strings.Repeat("a", 40), ProofNote: "code"}); err == nil {
		t.Fatal("code silently replaced operational proof")
	}
	if _, err := f.e.DecideReviewFinding(t.Context(), writer, "cannot-reject", id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(2), Finding: 0, Disposition: "rejected", Reason: "discard"}); err == nil {
		t.Fatal("fixed finding rejected")
	}
	coverageRead(t, f, a, current, false)
	coverageFinalize(t, f, a, id, 2)
	coverageRead(t, f, a, current, true)
	r.ExpectedRevision = adjudicationRevision(3)
	r.ExpectedDigest = adjudicationSHA(got.OperationalProof.Digest)
	r.ProofNote = "Correction: exact repository API verified all three entries absent."
	body, err := f.e.RecordOperationalFix(t.Context(), writer, "correction", id, r)
	adjudicationReceipt(t, body, err, id, "operational_fix_recorded", 4)
	state = adjudicationRead(t, f, id)
	if state.FrozenCounts.Accepted != 1 || state.Findings[0].OperationalProof.Note != r.ProofNote {
		t.Fatal("correction lost frozen counts or proof")
	}
	coverageRead(t, f, a, current, true)
	var changes int
	if err := f.store.DB().QueryRow("SELECT count(*) FROM review_adjudication_changes WHERE evidence_id=? AND kind='operational_fix_recorded'", id).Scan(&changes); err != nil || changes != 2 {
		t.Fatalf("audit history %d %v", changes, err)
	}
}

func TestOperationalFixRejectsInvalidInputAndWrongDisposition(t *testing.T) {
	f, _, id := adjudicationFixture(t, 1, false)
	writer := f.scope(t, attemptProject, f.writer, false)
	r := OperationalFixRequest{ExpectedRevision: adjudicationRevision(0), Finding: 0, ExpectedDigest: adjudicationSHA(""), ProofNote: "verified"}
	if _, err := f.e.RecordOperationalFix(t.Context(), writer, "undecided", id, r); err == nil {
		t.Fatal("unaccepted finding fixed")
	}
	adjudicationDecide(t, f, id, "accept", 0, 0, "accepted")
	r.ExpectedRevision = adjudicationRevision(1)
	cases := []OperationalFixRequest{r, r, r, r, r, r, r}
	cases[0].ProofNote = ""
	cases[1].ProofNote = "unsafe\x00"
	cases[2].ExpectedDigest = nil
	cases[3].ExpectedRevision = nil
	cases[4].Finding = -1
	cases[5].ProofNote = strings.Repeat("x", 2049)
	cases[6].ExpectedDigest = adjudicationSHA(strings.Repeat("A", 64))
	for _, bad := range cases {
		if _, err := f.e.RecordOperationalFix(t.Context(), writer, "invalid", id, bad); err == nil {
			t.Fatal("invalid operational proof accepted")
		}
	}
}

func TestOperationalFixHTTPRejectsClientSuppliedAttribution(t *testing.T) {
	f, _, id, path := evidenceHTTPFixture(t, 1)
	adjudicationDecide(t, f, id, "accept", 0, 0, "accepted")
	good := `{"expected_revision":1,"finding":0,"expected_digest":"","proof_note":"Verified external state"}`
	for _, bad := range []string{
		`{"expected_revision":1,"finding":0,"expected_digest":"","proof_note":"verified","actor_id":"principal_fake"}`,
		`{"expected_revision":1,"finding":0,"expected_digest":"","proof_note":"verified","recorded_at":"2026-01-01T00:00:00Z"}`,
		`{"expected_revision":1,"finding":0,"proof_note":"verified"}`,
	} {
		got := f.request("POST", path+"/operational-fixes", f.writer, "invalid", bad)
		if got.Code != 400 {
			t.Fatalf("invalid body status %d: %s", got.Code, got.Body.String())
		}
	}
	got := f.request("POST", path+"/operational-fixes", f.reader, "denied", good)
	if got.Code != 403 {
		t.Fatalf("reader write status %d", got.Code)
	}
	got = f.request("POST", path+"/operational-fixes", f.writer, "http-proof", good)
	evidenceHTTPReceipt(t, got, id, "operational_fix_recorded", 2)
}

func TestOperationalFixNativeProjectionReadback(t *testing.T) {
	f, a, evidence := cliPublicationFixture(t, 1, "completed")
	id := evidence.ID
	if _, err := f.e.PublishEvidence(t.Context(), attemptProject, id, cliPublicationPull(evidence.Anchor.PullNumber)); err != nil {
		t.Fatal(err)
	}
	adjudicationDecide(t, f, id, "accept", 0, 0, "accepted")
	writer := f.scope(t, attemptProject, f.writer, false)
	r := OperationalFixRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, ExpectedDigest: adjudicationSHA(""), ProofNote: "Verified operational remediation"}
	body, err := f.e.RecordOperationalFix(t.Context(), writer, "operational", id, r)
	adjudicationReceipt(t, body, err, id, "operational_fix_recorded", 2)
	adjudicationComplete(t, f, a)
	body, err = f.e.FinalizeReviewEvidence(t.Context(), writer, "finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, body, err, id, "finalized", 3)
	detail := cliPublicationDetail(t, f, evidence.Anchor.PullNumber, id)
	if detail.Run.Fixed != 1 || detail.Run.Accepted != 1 || detail.Findings[0].FixSHA != "" || detail.Findings[0].OperationalProof == nil {
		t.Fatalf("incorrect projection %+v", detail)
	}
}

func TestOperationalFixAttentionExcludesFinalizedReceipt(t *testing.T) {
	f, a, evidence := cliPublicationFixture(t, 1, "completed")
	id := evidence.ID
	adjudicationDecide(t, f, id, "accept-no-proof", 0, 0, "accepted")
	adjudicationComplete(t, f, a)
	pending := listOperationPage(t, f, "?status=attention&channel=cli")
	if len(pending.Items) != 1 || pending.Items[0].ID != a.OperationID {
		t.Fatalf("missing real pending operation: %+v", pending)
	}
	writer := f.scope(t, attemptProject, f.writer, false)
	r := OperationalFixRequest{ExpectedRevision: adjudicationRevision(1), Finding: 0, ExpectedDigest: adjudicationSHA(""), ProofNote: "Verified operational remediation"}
	body, err := f.e.RecordOperationalFix(t.Context(), writer, "attention-operational", id, r)
	adjudicationReceipt(t, body, err, id, "operational_fix_recorded", 2)
	stillPending := listOperationPage(t, f, "?status=attention&channel=cli")
	if len(stillPending.Items) != 1 || stillPending.Items[0].ID != a.OperationID {
		t.Fatalf("unfinalized operation must stay in attention: %+v", stillPending)
	}
	body, err = f.e.FinalizeReviewEvidence(t.Context(), writer, "attention-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(2)})
	adjudicationReceipt(t, body, err, id, "finalized", 3)
	if page := listOperationPage(t, f, "?status=attention&channel=cli"); len(page.Items) != 0 || page.HasMore {
		t.Fatalf("settled receipt was not excluded: %+v", page)
	}
	if page := listOperationPage(t, f, "?status=all&channel=cli"); len(page.Items) != 1 || page.Items[0].ID != a.OperationID {
		t.Fatalf("settled history disappeared: %+v", page)
	}
	// A changed ledger invalidates the ready proof; malformed operational evidence
	// must still enter the bounded validation path and fail closed.
	if _, err := f.store.DB().Exec(`UPDATE review_adjudication_state SET body_json=json_set(body_json,'$.findings[0].operational_proof.digest',?) WHERE evidence_id=?`, strings.Repeat("0", 64), id); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/projects/%s/reviews/operations?status=attention&channel=cli", attemptProject)
	if w := f.request("GET", path, f.reader, "", ""); w.Code != 500 {
		t.Fatalf("malformed receipt hidden: %d %s", w.Code, w.Body.String())
	}
}
