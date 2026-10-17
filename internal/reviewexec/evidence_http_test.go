package reviewexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func evidenceHTTPFixture(t *testing.T, findings int) (*attemptFixture, Attempt, string, string) {
	t.Helper()
	f, a, id := adjudicationFixture(t, findings, false)
	// Retained evidence routes must work with no operator capability or driver.
	e, err := New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	e.Register(mux)
	f.handler = mux
	return f, a, id, "/api/v1/projects/" + attemptProject + "/reviews/evidence/" + id
}

func evidenceHTTPRequire(t *testing.T, w *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var value map[string]any
	if json.Unmarshal(w.Body.Bytes(), &value) != nil {
		t.Fatal("response is not JSON")
	}
	if code != "" {
		failure, ok := value["error"].(map[string]any)
		if !ok || failure["code"] != code {
			t.Fatalf("expected code %s: %s", code, w.Body.String())
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("JSON response lacks private response headers")
	}
	return value
}

func evidenceHTTPKeys(t *testing.T, value map[string]any, expected ...string) {
	t.Helper()
	if len(value) != len(expected) {
		t.Fatalf("response field count %d, want %d", len(value), len(expected))
	}
	for _, key := range expected {
		if _, ok := value[key]; !ok {
			t.Fatal("missing response field", key)
		}
	}
}

func evidenceHTTPReceipt(t *testing.T, w *httptest.ResponseRecorder, id, state string, revision int64) []byte {
	t.Helper()
	value := evidenceHTTPRequire(t, w, 200, "")
	evidenceHTTPKeys(t, value, "id", "state", "revision")
	if value["id"] != id || value["state"] != state || value["revision"] != float64(revision) {
		t.Fatal("unexpected compact receipt", value)
	}
	return append([]byte(nil), w.Body.Bytes()...)
}

func TestEvidenceHTTPPublicShapeAndProviderFreeReads(t *testing.T) {
	f, a, id, path := evidenceHTTPFixture(t, 2)
	before := adjudicationState(t, f, a)
	w := f.request("GET", path, f.reader, "", "")
	value := evidenceHTTPRequire(t, w, 200, "")
	evidenceHTTPKeys(t, value, "id", "channel", "project_id", "attempt_id", "operation_id", "capability_fingerprint", "anchor", "capture", "recorded_at")
	if value["channel"] != "cli" || value["id"] != id || value["project_id"] != attemptProject || value["attempt_id"] != a.ID || value["operation_id"] != a.OperationID {
		t.Fatal("evidence identity changed")
	}
	capture, ok := value["capture"].(map[string]any)
	if !ok {
		t.Fatal("capture is not object")
	}
	evidenceHTTPKeys(t, capture, "capture_id", "capture_sha256", "capture_bytes", "started_at", "finished_at", "outcome", "complete", "reported_findings", "reviewed_files", "findings")
	findings, ok := capture["findings"].([]any)
	if !ok || len(findings) != 2 {
		t.Fatal("findings missing")
	}
	for i, item := range findings {
		finding, ok := item.(map[string]any)
		if !ok {
			t.Fatal("finding is not object")
		}
		evidenceHTTPKeys(t, finding, "finding", "title", "detail", "severity", "path", "start_line", "end_line")
		if finding["finding"] != float64(i) || finding["title"] != "Immutable finding "+string(rune('0'+i)) || finding["detail"] != "Literal <script>source text</script>\nwith details" || finding["severity"] != "high" || finding["path"] != "outside/diff.go" || finding["start_line"] != float64(8) || finding["end_line"] != float64(12) {
			t.Fatal("finding lost retained text/index/location", finding)
		}
	}
	var raw map[string]any
	var stored string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(stored), &raw) != nil || !reflect.DeepEqual(value["anchor"], raw["anchor"]) || value["capability_fingerprint"] != raw["capability_fingerprint"] || value["recorded_at"] != raw["recorded_at"] {
		t.Fatal("public DTO did not retain immutable source metadata")
	}
	for _, name := range []string{"capture_id", "capture_sha256", "capture_bytes", "started_at", "finished_at"} {
		if capture[name] != raw["capture"].(map[string]any)[name] {
			t.Fatal("capture metadata changed", name)
		}
	}
	if capture["outcome"] != "completed" || capture["complete"] != true || capture["reported_findings"] != float64(2) || !reflect.DeepEqual(capture["reviewed_files"], []any{"internal/changed.go"}) {
		t.Fatal("capture summary changed")
	}
	if bytes.Contains(w.Body.Bytes(), []byte("expected_anchor")) || bytes.Contains(w.Body.Bytes(), []byte(`"Review"`)) || bytes.Contains(w.Body.Bytes(), []byte("stdout")) || bytes.Contains(w.Body.Bytes(), []byte("stderr")) {
		t.Fatal("public evidence leaked internal parser/raw capture fields")
	}
	state := evidenceHTTPRequire(t, f.request("GET", path+"/adjudication", f.writer, "", ""), 200, "")
	evidenceHTTPKeys(t, state, "evidence_id", "revision", "finalized", "frozen_counts", "findings")
	if state["revision"] != float64(0) || state["frozen_counts"] != nil || state["evidence_id"] != id || strings.Contains(encoded(state), "Immutable finding") {
		t.Fatal("default adjudication leaked source or fabricated state")
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("HTTP reads changed source/journal/projection/receipts/events")
	}
}

func TestEvidenceHTTPDecisionFixFinalizationReplayAndFrozenCounts(t *testing.T) {
	f, a, id, path := evidenceHTTPFixture(t, 2)
	original := f.request("GET", path, f.reader, "", "").Body.String()
	journal := attemptState(t, f, a)
	decision := `{"expected_revision":0,"finding":0,"disposition":"accepted","reason":"Explicit acceptance"}`
	first := evidenceHTTPReceipt(t, f.request("POST", path+"/decisions", f.writer, "accept", decision), id, "adjudicated", 1)
	before := adjudicationState(t, f, a)
	replay := f.request("POST", path+"/decisions", f.writer, "accept", decision)
	if !bytes.Equal(first, replay.Body.Bytes()) || replay.Code != 200 || adjudicationState(t, f, a) != before {
		t.Fatal("decision replay changed state")
	}
	evidenceHTTPRequire(t, f.request("POST", path+"/decisions", f.writer, "accept", strings.Replace(decision, "Explicit acceptance", "Different request", 1)), 409, "idempotency_conflict")
	evidenceHTTPRequire(t, f.request("POST", path+"/decisions", f.writer, "stale-revision", decision), 409, "review_conflict")
	evidenceHTTPReceipt(t, f.request("POST", path+"/decisions", f.writer, "reject", `{"expected_revision":1,"finding":1,"disposition":"rejected","reason":"Explicit rejection"}`), id, "adjudicated", 2)
	fix := `{"expected_revision":2,"finding":0,"expected_sha":"","fix_sha":"` + strings.Repeat("a", 40) + `","proof_note":"Focused proof"}`
	fixReceipt := evidenceHTTPReceipt(t, f.request("POST", path+"/fixes", f.writer, "fix", fix), id, "fix_recorded", 3)
	evidenceHTTPRequire(t, f.request("POST", path+"/finalization", f.writer, "pending-finalization", `{"expected_revision":3}`), 409, "review_conflict")
	if attemptState(t, f, a) != journal {
		t.Fatal("HTTP adjudication settled attempt")
	}
	adjudicationComplete(t, f, a)
	finalRequest := `{"expected_revision":3}`
	finalReceipt := evidenceHTTPReceipt(t, f.request("POST", path+"/finalization", f.writer, "finalize", finalRequest), id, "finalized", 4)
	evidenceHTTPReceipt(t, f.request("POST", path+"/decisions", f.writer, "correction", `{"expected_revision":4,"finding":1,"disposition":"accepted","reason":"Audited later correction"}`), id, "adjudicated", 5)
	state := evidenceHTTPRequire(t, f.request("GET", path+"/adjudication", f.reader, "", ""), 200, "")
	if state["finalized"] != true || state["revision"] != float64(5) || !reflect.DeepEqual(state["frozen_counts"], map[string]any{"found": float64(2), "accepted": float64(1), "routed": float64(0)}) {
		t.Fatal("HTTP correction changed frozen source counts", state)
	}
	before = adjudicationState(t, f, a)
	for _, x := range []struct {
		suffix, key, body string
		receipt           []byte
	}{{"/decisions", "accept", decision, first}, {"/fixes", "fix", fix, fixReceipt}, {"/finalization", "finalize", finalRequest, finalReceipt}} {
		w := f.request("POST", path+x.suffix, f.writer, x.key, x.body)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), x.receipt) {
			t.Fatal("historical HTTP replay lost compact receipt")
		}
	}
	evidenceHTTPRequire(t, f.request("POST", path+"/finalization", f.writer, "already-finalized", `{"expected_revision":5}`), 409, "review_conflict")
	evidenceHTTPRequire(t, f.request("POST", path+"/decisions", f.writer, "withdraw-proof", `{"expected_revision":5,"finding":0,"disposition":"rejected","reason":"Must not erase proof"}`), 409, "review_conflict")
	if adjudicationState(t, f, a) != before || f.request("GET", path, f.reader, "", "").Body.String() != original {
		t.Fatal("replay/refusal changed immutable capture or audit")
	}
}

func TestEvidenceHTTPAuthorizationInstanceAndReadOnlyCookie(t *testing.T) {
	f, a, _, path := evidenceHTTPFixture(t, 1)
	decision := `{"expected_revision":0,"finding":0,"disposition":"accepted","reason":"Explicit reason"}`
	before := adjudicationState(t, f, a)
	for _, suffix := range []string{"", "/adjudication"} {
		evidenceHTTPRequire(t, f.request("GET", path+suffix, "", "", ""), 401, "unauthorized")
		evidenceHTTPRequire(t, f.request("GET", path+suffix, f.other, "", ""), 403, "forbidden")
		otherPath := strings.Replace(path, attemptProject, otherAttemptProject, 1)
		evidenceHTTPRequire(t, f.request("GET", otherPath+suffix, f.other, "", ""), 404, "not_found")
	}
	for _, x := range []struct{ suffix, body string }{{"/decisions", decision}, {"/fixes", `{"expected_revision":0,"finding":0,"expected_sha":"","fix_sha":"` + strings.Repeat("a", 40) + `","proof_note":"Proof"}`}, {"/finalization", `{"expected_revision":0}`}} {
		evidenceHTTPRequire(t, f.request("POST", path+x.suffix, f.reader, "denied", `{`), 403, "forbidden")
		evidenceHTTPRequire(t, f.request("POST", strings.Replace(path, attemptProject, otherAttemptProject, 1)+x.suffix, f.other, "denied", x.body), 404, "not_found")
	}
	for _, method := range []string{"GET", "POST"} {
		suffix, body := "", ""
		if method == "POST" {
			suffix, body = "/decisions", decision
		}
		r := httptest.NewRequest(method, path+suffix, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.writer)
		r.Header.Set("X-FC-Instance", "instance_other")
		r.Header.Set("Idempotency-Key", "denied")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		evidenceHTTPRequire(t, w, 409, "instance_mismatch")
	}
	login := httptest.NewRequest("POST", "http://localhost/api/v1/projects/"+attemptProject+"/browser-session", nil)
	login.SetPathValue("project_id", attemptProject)
	login.Header.Set("Authorization", "Bearer "+f.reader)
	login.Header.Set("X-FC-Instance", f.store.InstanceID())
	id := strings.Repeat("a", 32)
	_, cookie, err := f.store.CreateBrowserSession(login, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		method, suffix, body string
		status               int
	}{{"GET", "", "", 200}, {"GET", "/adjudication", "", 200}, {"POST", "/decisions", decision, 403}, {"POST", "/fixes", `{}`, 403}, {"POST", "/finalization", `{}`, 403}} {
		r := httptest.NewRequest(x.method, "http://localhost"+path+x.suffix, strings.NewReader(x.body))
		r.AddCookie(cookie)
		r.Header.Set("X-FC-Browser-Session", id)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		r.Header.Set("Idempotency-Key", "cookie-denied")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		code := ""
		if x.status != 200 {
			code = "forbidden"
		}
		evidenceHTTPRequire(t, w, x.status, code)
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("denied requests or cookie reads changed state")
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
		t.Fatal(err)
	}
	evidenceHTTPRequire(t, f.request("POST", path+"/decisions", f.writer, "revoked", decision), 401, "unauthorized")
}

func TestEvidenceHTTPStrictBodiesQueriesKeysIDsAndMethods(t *testing.T) {
	f, a, id, path := evidenceHTTPFixture(t, 1)
	before := adjudicationState(t, f, a)
	validDecision := `{"expected_revision":0,"finding":0,"disposition":"accepted","reason":"Reason"}`
	validFix := `{"expected_revision":0,"finding":0,"expected_sha":"","fix_sha":"` + strings.Repeat("a", 40) + `","proof_note":"Proof"}`
	for _, x := range []struct {
		suffix, valid string
		invalid       []string
	}{
		{"/decisions", validDecision, []string{`null`, `{}`, `{"expected_revision":null,"finding":0,"disposition":"accepted","reason":"Reason"}`, `{"expected_revision":0,"finding":null,"disposition":"accepted","reason":"Reason"}`, strings.Replace(validDecision, `"reason":"Reason"`, `"Reason":"Reason"`, 1), strings.Replace(validDecision, `"finding":0`, `"finding":0,"finding":0`, 1), strings.Replace(validDecision, `"finding":0`, `"finding":0,"extra":true`, 1), strings.Replace(validDecision, `"reason":"Reason"`, `"reason":"\ud800"`, 1), strings.Replace(validDecision, `"reason":"Reason"`, `"reason":"`+strings.Repeat("x", 2049)+`"`, 1)}},
		{"/fixes", validFix, []string{`null`, `{}`, strings.Replace(validFix, `"expected_sha":""`, `"expected_sha":null`, 1), strings.Replace(validFix, `"proof_note":"Proof"`, `"proof_note":"Proof","extra":true`, 1), strings.Replace(validFix, `"finding":0`, `"finding":0,"finding":0`, 1)}},
		{"/finalization", `{"expected_revision":0}`, []string{`null`, `{}`, `{"expected_revision":null}`, `{"Expected_revision":0}`, `{"expected_revision":0,"expected_revision":0}`, `{"expected_revision":0,"extra":true}`}},
	} {
		var required map[string]json.RawMessage
		if json.Unmarshal([]byte(x.valid), &required) != nil {
			t.Fatal("invalid strict-body fixture")
		}
		for name := range required {
			for _, omit := range []bool{true, false} {
				var fields map[string]json.RawMessage
				if json.Unmarshal([]byte(x.valid), &fields) != nil {
					t.Fatal("invalid strict-body fixture")
				}
				if omit {
					delete(fields, name)
				} else {
					fields[name] = json.RawMessage("null")
				}
				evidenceHTTPRequire(t, f.request("POST", path+x.suffix, f.writer, "invalid", encoded(fields)), 400, "validation_failed")
			}
		}
		for _, body := range x.invalid {
			evidenceHTTPRequire(t, f.request("POST", path+x.suffix, f.writer, "invalid", body), 400, "validation_failed")
		}
		evidenceHTTPRequire(t, f.request("POST", path+x.suffix, f.writer, "", x.valid), 400, "idempotency_key_required")
		evidenceHTTPRequire(t, f.requestWithKeys("POST", path+x.suffix, f.writer, []string{"one", "two"}, x.valid), 400, "idempotency_key_required")
		for _, query := range []string{"?unexpected=true", "?limit=1", "?x=%zz", "?x=1&x=2"} {
			evidenceHTTPRequire(t, f.request("POST", path+x.suffix+query, f.writer, "invalid", x.valid), 400, "validation_failed")
		}
	}
	for _, suffix := range []string{"", "/adjudication"} {
		for _, query := range []string{"?limit=1", "?cursor=opaque", "?x=%zz"} {
			evidenceHTTPRequire(t, f.request("GET", path+suffix+query, f.reader, "", ""), 400, "validation_failed")
		}
	}
	for _, bad := range []string{"invalid", "review_evidence_" + strings.Repeat("A", 32), "review_evidence_" + strings.Repeat("a", 31), "review_evidence_" + strings.Repeat("a", 33), "review_operation_" + strings.Repeat("a", 32)} {
		badPath := strings.Replace(path, id, bad, 1)
		for _, suffix := range []string{"", "/adjudication"} {
			evidenceHTTPRequire(t, f.request("GET", badPath+suffix, f.reader, "", ""), 400, "validation_failed")
		}
		evidenceHTTPRequire(t, f.request("POST", badPath+"/decisions", f.writer, "invalid", validDecision), 400, "validation_failed")
	}
	evidenceHTTPRequire(t, f.request("GET", strings.Replace(path, id, "review_evidence_"+strings.Repeat("f", 32), 1), f.reader, "", ""), 404, "not_found")
	for _, x := range []struct{ method, suffix string }{{"POST", ""}, {"PATCH", "/adjudication"}, {"GET", "/decisions"}, {"DELETE", "/fixes"}, {"PUT", "/finalization"}} {
		if w := f.request(x.method, path+x.suffix, f.writer, "invalid", `{}`); w.Code != 405 {
			t.Fatalf("unsupported method status %d", w.Code)
		}
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("strict request refusal changed capture/journal/adjudication/events/receipts")
	}
}

func TestEvidenceHTTPEmptyCollectionsAreArrays(t *testing.T) {
	f, _, _, path := evidenceHTTPFixture(t, 0)
	value := evidenceHTTPRequire(t, f.request("GET", path, f.reader, "", ""), 200, "")
	capture := value["capture"].(map[string]any)
	if findings, ok := capture["findings"].([]any); !ok || len(findings) != 0 {
		t.Fatal("empty findings must be []")
	}
	// A partial capture may legitimately retain no reviewed-file list.
	f2, a, in := evidenceFixture(t, "cli", true)
	in.Review.Outcome, in.Review.Complete = "failed", false
	in.Review.ReviewedFiles = nil
	r, err := f2.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	w := f2.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/evidence/"+r.ID, f2.reader, "", "")
	partial := evidenceHTTPRequire(t, w, 200, "")["capture"].(map[string]any)
	if files, ok := partial["reviewed_files"].([]any); !ok || len(files) != 0 {
		t.Fatal("empty reviewed files must be []")
	}
}

func TestEvidenceHTTPFullDetailRetainsLedgerBound(t *testing.T) {
	f, a, in := evidenceFixture(t, "cli", true)
	original := in.Review.Findings[0]
	in.Review.Findings = nil
	for i := 0; i < 100; i++ {
		finding := original
		finding.Detail = strings.Repeat("d", 16<<10)
		in.Review.Findings = append(in.Review.Findings, finding)
	}
	in.Review.ReportedFindings = 100
	in.CaptureBytes = 2 << 20
	r, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	w := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/evidence/"+r.ID, f.reader, "", "")
	capture := evidenceHTTPRequire(t, w, 200, "")["capture"].(map[string]any)
	findings := capture["findings"].([]any)
	if w.Body.Len() <= 1<<20 || len(findings) != 100 || findings[99].(map[string]any)["finding"] != float64(99) || findings[99].(map[string]any)["detail"] != strings.Repeat("d", 16<<10) {
		t.Fatal("full-detail HTTP response truncated valid retained evidence")
	}
}

// A private stored-record failure must not become a panic or a new decision.
func TestEvidenceHTTPRejectsCorruptHostedSourceInventory(t *testing.T) {
	f, a, in := hostedPublicationFixture(t, false)
	receipt, err := f.e.RecordReviewEvidence(t.Context(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_evidence WHERE id=?`, receipt.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var record ReviewEvidence
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		t.Fatal(err)
	}
	record.Capture.Hosted.Sources = nil
	if _, err := f.store.DB().Exec(`UPDATE review_execution_evidence SET body_json=? WHERE id=?`, encoded(record), receipt.ID); err != nil {
		t.Fatal(err)
	}
	before := adjudicationState(t, f, a)
	path := "/api/v1/projects/" + attemptProject + "/reviews/evidence/" + receipt.ID
	evidenceHTTPRequire(t, f.request("GET", path, f.reader, "", ""), 500, "storage_unavailable")
	evidenceHTTPRequire(t, f.request("GET", path+"/adjudication", f.reader, "", ""), 500, "storage_unavailable")
	evidenceHTTPRequire(t, f.request("POST", path+"/decisions", f.writer, "corrupt-source-decision", `{"expected_revision":0,"finding":0,"disposition":"accepted","reason":"Must not commit"}`), 500, "storage_unavailable")
	if adjudicationState(t, f, a) != before {
		t.Fatal("corrupt source read or decision changed durable state")
	}
}

func TestEvidenceHTTPStoredNullFindingsAreArrayWithoutWrites(t *testing.T) {
	f, a, id, path := evidenceHTTPFixture(t, 0)
	adjudicationComplete(t, f, a)
	evidenceHTTPRequire(t, f.request("POST", path+"/finalization", f.writer, "zero-finalize", `{"expected_revision":0}`), 200, "")
	if _, err := f.store.DB().Exec(`UPDATE review_adjudication_state SET body_json=json_set(body_json,'$.findings',NULL) WHERE evidence_id=?`, id); err != nil {
		t.Fatal(err)
	}
	before := adjudicationState(t, f, a)
	value := evidenceHTTPRequire(t, f.request("GET", path+"/adjudication", f.reader, "", ""), 200, "")
	if findings, ok := value["findings"].([]any); !ok || len(findings) != 0 {
		t.Fatal("stored zero findings must be encoded as []")
	}
	state, err := f.e.ReadReviewAdjudication(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), id)
	if err != nil || state.Findings == nil || len(state.Findings) != 0 {
		t.Fatal("direct ledger view retained null collection", err)
	}
	if adjudicationState(t, f, a) != before {
		t.Fatal("collection normalization rewrote retained state")
	}
}
