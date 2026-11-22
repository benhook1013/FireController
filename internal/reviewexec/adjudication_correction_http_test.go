package reviewexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviews"
)

// Use the registered handlers and fixture project credentials for all decisions,
// commit proof and corrections; only trusted capture/publication setup is direct.
func finalizedFixCorrectionHTTPFixture(t *testing.T) (*attemptFixture, Attempt, string, reviews.RunDetailResponse) {
	t.Helper()
	f, a, evidence := projectionFixture(t)
	adjudicationComplete(t, f, a)
	run := projectionRun(evidence)
	projectionPublish(t, f, reviews.NativePublication{Pull: projectionPull(2), Run: &run})
	path := "/api/v1/projects/" + attemptProject + "/reviews/evidence/" + evidence.ID
	evidenceHTTPReceipt(t, f.request(http.MethodPost, path+"/decisions", f.writer, "correction-accept", `{"expected_revision":0,"finding":0,"disposition":"accepted","reason":"Verified original finding"}`), evidence.ID, "adjudicated", 1)
	evidenceHTTPReceipt(t, f.request(http.MethodPost, path+"/decisions", f.writer, "correction-reject-sibling", `{"expected_revision":1,"finding":1,"disposition":"rejected","reason":"Original sibling rejection"}`), evidence.ID, "adjudicated", 2)
	fix := `{"expected_revision":2,"finding":0,"expected_sha":"","fix_sha":"` + strings.Repeat("f", 40) + `","proof_note":"Original focused commit proof"}`
	evidenceHTTPReceipt(t, f.request(http.MethodPost, path+"/fixes", f.writer, "correction-record-fix", fix), evidence.ID, "fix_recorded", 3)
	var runs reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &runs)
	if len(runs.Items) != 1 || runs.Items[0].Source != "native-executor" || runs.Items[0].SourceRunID != evidence.ID {
		t.Fatal("fixture lacks its exact native source run", runs)
	}
	var detail reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+runs.Items[0].ID, &detail)
	state := fixCorrectionHTTPAdjudication(t, f, path)
	if state.Revision != 3 || !state.Finalized || state.FrozenCounts == nil || *state.FrozenCounts != (FindingCounts{Found: 2, Accepted: 1}) || len(state.Findings) != 2 ||
		state.Findings[0].Disposition != "accepted" || state.Findings[0].FixSHA != strings.Repeat("f", 40) || state.Findings[0].ProofNote != "Original focused commit proof" || state.Findings[0].FixedAt == "" ||
		!detail.Run.Finalized || detail.Run.Found != 2 || detail.Run.Accepted != 1 || detail.Run.Routed != 0 || detail.Run.Fixed != 1 || len(detail.Findings) != 2 {
		t.Fatal("fixture is not finalized, accepted and commit-bound", state, detail)
	}
	return f, a, path, detail
}

func fixCorrectionHTTPAdjudication(t *testing.T, f *attemptFixture, path string) ReviewAdjudication {
	t.Helper()
	w := f.request(http.MethodGet, path+"/adjudication", f.reader, "", "")
	evidenceHTTPRequire(t, w, http.StatusOK, "")
	var state ReviewAdjudication
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestFindingFixCorrectionHTTPFinalizedWriterReplayAndFrozenCounts(t *testing.T) {
	f, a, path, original := finalizedFixCorrectionHTTPFixture(t)
	id := original.Run.SourceRunID
	before := fixCorrectionHTTPAdjudication(t, f, path)
	source := f.request(http.MethodGet, path, f.reader, "", "")
	evidenceHTTPRequire(t, source, http.StatusOK, "")
	journal := attemptState(t, f, a)
	historicalDigest := projectionDigest(t, f, original.SnapshotID)
	var historical reviews.RunDetailResponse
	historicalPath := "/runs/" + original.Run.ID + "?snapshot=" + original.SnapshotID
	historicalBody := projectionRead(t, f, historicalPath, &historical)
	var priorAudit string
	if err := f.store.DB().QueryRow(`SELECT group_concat(input_json,'') FROM review_adjudication_changes WHERE evidence_id=? AND revision<=3`, id).Scan(&priorAudit); err != nil {
		t.Fatal(err)
	}
	changes := attemptCount(t, f.store, `SELECT count(*) FROM review_adjudication_changes WHERE evidence_id=?`, id)
	receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	publications := attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications WHERE evidence_id=?`, id)
	if publications != 1 {
		t.Fatal("fixture must have exactly one original finalized checkpoint", publications)
	}

	request := `{"expected_revision":3,"finding":0,"expected_sha":"` + before.Findings[0].FixSHA + `","reason":"Audited correction: source precondition cannot occur"}`
	first := evidenceHTTPReceipt(t, f.request(http.MethodPost, path+"/corrections", f.writer, "audited-correction", request), id, "adjudication_corrected", 4)
	corrected := fixCorrectionHTTPAdjudication(t, f, path)
	if corrected.EvidenceID != id || corrected.Revision != 4 || !corrected.Finalized || !reflect.DeepEqual(corrected.FrozenCounts, before.FrozenCounts) || len(corrected.Findings) != 2 {
		t.Fatal("correction changed finalized source identity or original counts", corrected)
	}
	decision := corrected.Findings[0]
	if decision.Finding != 0 || decision.Disposition != "rejected" || decision.Reason != "Audited correction: source precondition cannot occur" || decision.ActorID != "exec-writer-principal" || decision.DecidedAt == "" ||
		decision.FixSHA != "" || decision.ProofNote != "" || decision.FixedAt != "" || decision.RouteID != "" || decision.OperationalProof != nil || !reflect.DeepEqual(corrected.Findings[1], before.Findings[1]) {
		t.Fatal("correction did not replace only the selected effective decision/proof with authenticated attribution", corrected)
	}
	var detail reviews.RunDetailResponse
	projectionRead(t, f, "/runs/"+original.Run.ID, &detail)
	if detail.Revision != original.Revision+1 || detail.SnapshotID == original.SnapshotID || !detail.Run.Finalized || detail.Run.Found != 2 || detail.Run.Accepted != 1 || detail.Run.Routed != 0 || detail.Run.Fixed != 0 || len(detail.Findings) != 2 {
		t.Fatal("projection did not retain original counts while clearing effective fixed state", detail)
	}
	wantFinding := original.Findings[0]
	wantFinding.Disposition, wantFinding.DecisionReason = decision.Disposition, decision.Reason
	wantFinding.Actor, wantFinding.DecidedAt = decision.ActorID, decision.DecidedAt
	wantFinding.FixSHA, wantFinding.ProofNote, wantFinding.ResolvedAt = "", "", ""
	if !reflect.DeepEqual(detail.Findings[0], wantFinding) || !reflect.DeepEqual(detail.Findings[1], original.Findings[1]) {
		t.Fatal("projection lost captured source text, diverged from the ledger or changed the sibling", detail.Findings)
	}
	var actor, kind, auditBody, at string
	var index int64
	if err := f.store.DB().QueryRow(`SELECT actor_id,kind,finding_index,input_json,created_at FROM review_adjudication_changes WHERE evidence_id=? AND revision=4`, id).Scan(&actor, &kind, &index, &auditBody, &at); err != nil {
		t.Fatal(err)
	}
	var audit struct {
		ExpectedRevision int64           `json:"expected_revision"`
		Finding          int64           `json:"finding"`
		ExpectedSHA      string          `json:"expected_sha"`
		Reason           string          `json:"reason"`
		Previous         FindingDecision `json:"previous"`
	}
	if err := json.Unmarshal([]byte(auditBody), &audit); err != nil {
		t.Fatal(err)
	}
	if actor != decision.ActorID || kind != "adjudication_corrected" || index != 0 || at != decision.DecidedAt || audit.ExpectedRevision != 3 || audit.Finding != 0 || audit.ExpectedSHA != before.Findings[0].FixSHA || audit.Reason != decision.Reason || !reflect.DeepEqual(audit.Previous, before.Findings[0]) {
		t.Fatal("private correction audit lost the original acceptance/commit proof or current actor", audit)
	}
	var retainedAudit string
	if err := f.store.DB().QueryRow(`SELECT group_concat(input_json,'') FROM review_adjudication_changes WHERE evidence_id=? AND revision<=3`, id).Scan(&retainedAudit); err != nil || retainedAudit != priorAudit {
		t.Fatal("correction rewrote earlier audit records", err)
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_adjudication_changes WHERE evidence_id=?`, id) != changes+1 || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != receipts+1 || attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications WHERE evidence_id=?`, id) != publications {
		t.Fatal("correction did not append exactly one audit/receipt or requeued the original checkpoint")
	}
	if attemptState(t, f, a) != journal || projectionDigest(t, f, original.SnapshotID) != historicalDigest || !bytes.Equal(historicalBody, projectionRead(t, f, historicalPath, &historical)) || !bytes.Equal(source.Body.Bytes(), f.request(http.MethodGet, path, f.reader, "", "").Body.Bytes()) {
		t.Fatal("correction changed immutable capture, settled execution or the original proof snapshot")
	}

	assertReplay := func() {
		t.Helper()
		state, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
		replay := f.request(http.MethodPost, path+"/corrections", f.writer, "audited-correction", request)
		evidenceHTTPRequire(t, replay, http.StatusOK, "")
		if !bytes.Equal(first, replay.Body.Bytes()) || adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != projection {
			t.Fatal("exact correction replay changed its receipt or durable effects")
		}
	}
	assertReplay()
	state, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
	changed := strings.Replace(request, "source precondition cannot occur", "different submitted rationale", 1)
	evidenceHTTPRequire(t, f.request(http.MethodPost, path+"/corrections", f.writer, "audited-correction", changed), http.StatusConflict, "idempotency_conflict")
	evidenceHTTPRequire(t, f.request(http.MethodPost, path+"/corrections", f.writer, "stale-correction", request), http.StatusConflict, "review_conflict")
	evidenceHTTPRequire(t, f.request(http.MethodPost, path+"/corrections", f.reader, "audited-correction", request), http.StatusForbidden, "forbidden")
	if adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != projection {
		t.Fatal("changed replay, stale correction or read-only replay changed durable state")
	}
	// Recovery must retain the original receipt even after an unrelated finding
	// advances both ledger and workspace beyond the correction's revision.
	evidenceHTTPReceipt(t, f.request(http.MethodPost, path+"/decisions", f.writer, "later-sibling-decision", `{"expected_revision":4,"finding":1,"disposition":"accepted","reason":"Later independent sibling correction"}`), id, "adjudicated", 5)
	assertReplay()
}

func TestFindingFixCorrectionHTTPGuardRefusalsAreAtomic(t *testing.T) {
	f, a, path, original := finalizedFixCorrectionHTTPFixture(t)
	request := `{"expected_revision":3,"finding":0,"expected_sha":"` + strings.Repeat("f", 40) + `","reason":"Audited correction"}`
	state, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
	for _, tc := range []struct {
		name, token, key, body, code, guard string
		status                              int
	}{
		{"stale-revision", f.writer, "stale", strings.Replace(request, `"expected_revision":3`, `"expected_revision":2`, 1), "review_conflict", "adjudication_revision_conflict", http.StatusConflict},
		{"reviewed-head-is-not-fix-proof", f.writer, "wrong-sha", strings.Replace(request, strings.Repeat("f", 40), strings.Repeat("a", 40), 1), "review_conflict", "fix_sha_conflict", http.StatusConflict},
		{"missing-reason", f.writer, "no-reason", strings.Replace(request, `,"reason":"Audited correction"`, "", 1), "validation_failed", "", http.StatusBadRequest},
		{"blank-reason", f.writer, "blank-reason", strings.Replace(request, "Audited correction", "   ", 1), "validation_failed", "", http.StatusBadRequest},
		{"missing-revision", f.writer, "no-revision", strings.Replace(request, `"expected_revision":3,`, "", 1), "validation_failed", "", http.StatusBadRequest},
		{"null-revision", f.writer, "null-revision", strings.Replace(request, `"expected_revision":3`, `"expected_revision":null`, 1), "validation_failed", "", http.StatusBadRequest},
		{"missing-finding", f.writer, "no-finding", strings.Replace(request, `"finding":0,`, "", 1), "validation_failed", "", http.StatusBadRequest},
		{"missing-sha", f.writer, "no-sha", strings.Replace(request, `"expected_sha":"`+strings.Repeat("f", 40)+`",`, "", 1), "validation_failed", "", http.StatusBadRequest},
		{"missing-key", f.writer, "", request, "idempotency_key_required", "", http.StatusBadRequest},
		{"forged-actor", f.writer, "forged-actor", strings.TrimSuffix(request, "}") + `,"actor_id":"forged-principal"}`, "validation_failed", "", http.StatusBadRequest},
		{"read-only-actor", f.reader, "reader", request, "forbidden", "", http.StatusForbidden},
		{"authorization-before-body", f.reader, "reader-malformed", `{`, "forbidden", "", http.StatusForbidden},
		{"unauthenticated", "", "anonymous", request, "unauthorized", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := evidenceHTTPRequire(t, f.request(http.MethodPost, path+"/corrections", tc.token, tc.key, tc.body), tc.status, tc.code)
			if tc.guard != "" {
				details, ok := value["error"].(map[string]any)["details"].(map[string]any)
				if !ok || details["reason"] != tc.guard || details["current_revision"] != float64(3) {
					t.Fatal("refusal did not identify the current guard", value)
				}
				if tc.guard == "fix_sha_conflict" && (details["field"] != "expected_sha" || details["finding"] != float64(0) || details["current_fix_sha"] != strings.Repeat("f", 40) || details["current_disposition"] != "accepted") {
					t.Fatal("SHA refusal confused reviewed head with current commit proof", details)
				}
			}
			if adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != projection || attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications WHERE evidence_id=?`, original.Run.SourceRunID) != 1 {
				t.Fatal("refused correction changed ledger, audit, events, receipts, checkpoint or projection")
			}
		})
	}
}

func TestFindingFixCorrectionHTTPDivergentProjectedProofRollsBack(t *testing.T) {
	f, a, path, original := finalizedFixCorrectionHTTPFixture(t)
	id := original.Run.SourceRunID
	runID, version := projectionNativeVersion(t, f, original.SnapshotID, id)
	// Simulate a valid-looking projection whose proof no longer matches the
	// authoritative ledger. Failure occurs after the ledger/audit/event writes.
	result, err := f.store.DB().Exec(`UPDATE review_finding_versions SET proof_note='Different retained projection proof' WHERE project_id=? AND run_id=? AND run_version_revision=? AND position=0`, attemptProject, runID, version)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		t.Fatal("did not change exactly the selected synthetic projection proof", n, err)
	}
	state, projection := adjudicationState(t, f, a), projectionDigest(t, f, "")
	request := `{"expected_revision":3,"finding":0,"expected_sha":"` + strings.Repeat("f", 40) + `","reason":"Must not commit against different projected proof"}`
	evidenceHTTPRequire(t, f.request(http.MethodPost, path+"/corrections", f.writer, "divergent-proof", request), http.StatusConflict, "observation_conflict")
	if adjudicationState(t, f, a) != state || projectionDigest(t, f, "") != projection || attemptCount(t, f.store, `SELECT count(*) FROM review_checkpoint_publications WHERE evidence_id=?`, id) != 1 {
		t.Fatal("projection proof mismatch partially committed correction ledger/audit/event/receipt/history")
	}
}
