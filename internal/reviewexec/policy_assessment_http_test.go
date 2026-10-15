package reviewexec

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPolicyAssessmentHTTPScopedCompactRead(t *testing.T) {
	f, a, id, base := evidenceHTTPFixture(t, 1)
	path := base + "/policy"
	before := adjudicationState(t, f, a)
	for _, token := range []string{f.reader, f.writer, f.admin} {
		w := f.request("GET", path, token, "", "")
		value := evidenceHTTPRequire(t, w, 200, "")
		evidenceHTTPKeys(t, value, "project_id", "evidence_id", "anchor", "source", "policy_revision", "evaluated_at", "state", "channels")
		if value["project_id"] != attemptProject || value["evidence_id"] != id || value["source"] != "captured_comparison" || value["state"] == "sufficient" || w.Body.Len() > 16<<10 {
			t.Fatal("assessment lost its explicit captured comparison or compact boundary", value)
		}
		for _, secret := range []string{f.admin, f.reader, "Immutable finding", "capture_sha256", "frozen_counts", "merge_ready"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("compact assessment exposed unrelated evidence or authority", secret)
			}
		}
	}
	evidenceHTTPRequire(t, f.request("GET", path, "", "", ""), 401, "unauthorized")
	evidenceHTTPRequire(t, f.request("GET", path, f.other, "", ""), 403, "forbidden")
	evidenceHTTPRequire(t, f.request("GET", strings.Replace(path, attemptProject, otherAttemptProject, 1), f.other, "", ""), 404, "not_found")
	for _, query := range []string{"?head_sha=" + strings.Repeat("a", 40), "?refresh=true", "?snapshot=other", "?unknown=1"} {
		evidenceHTTPRequire(t, f.request("GET", path+query, f.reader, "", ""), 400, "validation_failed")
	}
	if w := f.request("POST", path, f.admin, "no-policy-assessment-write", "{}"); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("assessment route accepted a mutation", w.Code)
	}
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Authorization", "Bearer "+f.reader)
	r.Header.Set("X-FC-Instance", "instance_other")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	evidenceHTTPRequire(t, w, 409, "instance_mismatch")
	if adjudicationState(t, f, a) != before {
		t.Fatal("assessment reads changed execution, evidence, decisions, receipts or events")
	}
}
