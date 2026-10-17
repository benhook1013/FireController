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

func policyHTTPFixture(t *testing.T) (*attemptFixture, string) {
	t.Helper()
	f := policyFixture(t)
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	return f, "/api/v1/projects/" + attemptProject + "/reviews/policy"
}

const policyHTTPInitial = `{"expected_revision":0,"required_channels":["hosted","cli"],"paused_channels":[],"max_review_files":0}`

func TestPolicyHTTPConfigurationReadAndHistoricalReceipt(t *testing.T) {
	f, path := policyHTTPFixture(t)
	evidenceHTTPRequire(t, f.request("GET", path, f.reader, "", ""), 409, "reviews_policy_unconfigured")
	if policyState(t, f) != "//" {
		t.Fatal("missing policy read allocated defaults")
	}
	first := evidenceHTTPReceipt(t, f.request("POST", path, f.admin, "first-policy", policyHTTPInitial), attemptProject, "policy_configured", 1)
	before := policyState(t, f)
	value := evidenceHTTPRequire(t, f.request("GET", path, f.reader, "", ""), 200, "")
	evidenceHTTPKeys(t, value, "project_id", "revision", "actor_id", "recorded_at", "policy")
	policy, ok := value["policy"].(map[string]any)
	if !ok {
		t.Fatal("policy is not an object")
	}
	evidenceHTTPKeys(t, policy, "required_channels", "paused_channels", "max_review_files")
	if value["project_id"] != attemptProject || value["revision"] != float64(1) || value["actor_id"] == "" || value["recorded_at"] == "" || policy["max_review_files"] != float64(0) || !reflect.DeepEqual(policy["required_channels"], []any{"hosted", "cli"}) || !reflect.DeepEqual(policy["paused_channels"], []any{}) {
		t.Fatal("policy record changed explicit order/zero/empty array", value)
	}
	if strings.Contains(encoded(value), "expected_revision") || strings.Contains(encoded(value), f.admin) || strings.Contains(encoded(value), "bootstrap-admin") {
		t.Fatal("current record leaked request-only fields or private data")
	}
	if policyState(t, f) != before {
		t.Fatal("GET policy changed stored state")
	}
	second := `{"expected_revision":1,"required_channels":["cli"],"paused_channels":["hosted"],"max_review_files":100000}`
	evidenceHTTPReceipt(t, f.request("POST", path, f.admin, "second-policy", second), attemptProject, "policy_configured", 2)
	before = policyState(t, f)
	replay := f.request("POST", path, f.admin, "first-policy", policyHTTPInitial)
	if replay.Code != 200 || !bytes.Equal(first, replay.Body.Bytes()) {
		t.Fatal("HTTP replay lost its original compact policy receipt")
	}
	evidenceHTTPRequire(t, f.request("POST", path, f.admin, "stale", policyHTTPInitial), 409, "review_conflict")
	evidenceHTTPRequire(t, f.request("POST", path, f.admin, "first-policy", strings.Replace(policyHTTPInitial, `"max_review_files":0`, `"max_review_files":1`, 1)), 409, "idempotency_conflict")
	current := evidenceHTTPRequire(t, f.request("GET", path, f.writer, "", ""), 200, "")
	if current["revision"] != float64(2) || current["policy"].(map[string]any)["max_review_files"] != float64(100000) || policyState(t, f) != before {
		t.Fatal("historical retry changed current policy")
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_policy_config`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM events`) != 2 || attemptCount(t, f.store, `SELECT count(*) FROM receipts`) != 2 {
		t.Fatal("HTTP policy write duplicated or omitted effects")
	}
	// No capability/driver is configured; these are only local preference writes.
	if len(f.e.capabilities) != 0 {
		t.Fatal("policy fixture unexpectedly enabled a provider")
	}
	for _, table := range []string{"review_execution_config", "review_operations", "review_execution_attempts", "review_execution_reservations", "review_execution_transitions"} {
		if attemptCount(t, f.store, "SELECT count(*) FROM "+table) != 0 {
			t.Fatal("policy route changed execution", table)
		}
	}
}

func TestPolicyHTTPAuthorizationCookieInstanceAndRevocation(t *testing.T) {
	f, path := policyHTTPFixture(t)
	before := policyState(t, f)
	evidenceHTTPRequire(t, f.request("GET", path, "", "", ""), 401, "unauthorized")
	evidenceHTTPRequire(t, f.request("GET", path, f.other, "", ""), 403, "forbidden")
	for _, token := range []string{f.reader, f.writer, f.other} {
		evidenceHTTPRequire(t, f.request("POST", path, token, "denied", `{`), 403, "forbidden")
	}
	evidenceHTTPRequire(t, f.request("POST", path, "", "denied", `{`), 401, "unauthorized")
	otherPath := strings.Replace(path, attemptProject, otherAttemptProject, 1)
	evidenceHTTPRequire(t, f.request("GET", otherPath, f.other, "", ""), 409, "reviews_policy_unconfigured")
	for _, method := range []string{"GET", "POST"} {
		r := httptest.NewRequest(method, path, strings.NewReader(policyHTTPInitial))
		r.Header.Set("Authorization", "Bearer "+f.admin)
		r.Header.Set("X-FC-Instance", "instance_other")
		r.Header.Set("Idempotency-Key", "denied")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		evidenceHTTPRequire(t, w, 409, "instance_mismatch")
	}
	if policyState(t, f) != before {
		t.Fatal("unauthorized or wrong-instance requests changed policy")
	}
	first := evidenceHTTPReceipt(t, f.request("POST", path, f.admin, "configured", policyHTTPInitial), attemptProject, "policy_configured", 1)
	evidenceHTTPRequire(t, f.request("GET", otherPath, f.other, "", ""), 409, "reviews_policy_unconfigured")
	other := `{"expected_revision":0,"required_channels":["hosted"],"paused_channels":["cli"],"max_review_files":1}`
	evidenceHTTPReceipt(t, f.request("POST", otherPath, f.admin, "other-project", other), otherAttemptProject, "policy_configured", 1)
	if got := evidenceHTTPRequire(t, f.request("GET", otherPath, f.other, "", ""), 200, ""); got["project_id"] != otherAttemptProject || got["policy"].(map[string]any)["max_review_files"] != float64(1) {
		t.Fatal("policy crossed projects")
	}
	// Even an administrator's derived browser session remains read-only.
	login := httptest.NewRequest("POST", "http://localhost/api/v1/projects/"+attemptProject+"/browser-session", nil)
	login.SetPathValue("project_id", attemptProject)
	login.Header.Set("Authorization", "Bearer "+f.admin)
	login.Header.Set("X-FC-Instance", f.store.InstanceID())
	id := strings.Repeat("a", 32)
	_, cookie, err := f.store.CreateBrowserSession(login, id, false)
	if err != nil {
		t.Fatal(err)
	}
	before = policyState(t, f)
	for _, x := range []struct {
		method, path, body string
		status             int
		code               string
	}{{"GET", path, "", 200, ""}, {"GET", otherPath, "", 403, "forbidden"}, {"POST", path, policyHTTPInitial, 403, "forbidden"}} {
		r := httptest.NewRequest(x.method, "http://localhost"+x.path, strings.NewReader(x.body))
		r.Header.Set("X-FC-Browser-Session", id)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		r.Header.Set("Idempotency-Key", "cookie-denied")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		evidenceHTTPRequire(t, w, x.status, x.code)
	}
	if policyState(t, f) != before {
		t.Fatal("cookie reads/denied write changed policy")
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE is_admin=1`); err != nil {
		t.Fatal(err)
	}
	evidenceHTTPRequire(t, f.request("POST", path, f.admin, "configured", policyHTTPInitial), 401, "unauthorized")
	if len(first) == 0 || policyState(t, f) != before {
		t.Fatal("revoked exact replay changed policy")
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`); err != nil {
		t.Fatal(err)
	}
	evidenceHTTPRequire(t, f.request("GET", path, f.reader, "", ""), 401, "unauthorized")
}

func TestPolicyHTTPStrictRequiredFieldsAndNoMutationOnFailure(t *testing.T) {
	f, path := policyHTTPFixture(t)
	before := policyState(t, f)
	var valid map[string]json.RawMessage
	if json.Unmarshal([]byte(policyHTTPInitial), &valid) != nil {
		t.Fatal("invalid policy fixture")
	}
	for field := range valid {
		for _, omit := range []bool{true, false} {
			var body map[string]json.RawMessage
			_ = json.Unmarshal([]byte(policyHTTPInitial), &body)
			if omit {
				delete(body, field)
			} else {
				body[field] = json.RawMessage("null")
			}
			evidenceHTTPRequire(t, f.request("POST", path, f.admin, "invalid", encoded(body)), 400, "validation_failed")
		}
	}
	for _, body := range []string{
		`null`, `{}`, `[]`, policyHTTPInitial + `{}`,
		strings.Replace(policyHTTPInitial, `"expected_revision":0`, `"expected_revision":0,"expected_revision":0`, 1),
		strings.Replace(policyHTTPInitial, `"expected_revision":0`, `"Expected_revision":0`, 1),
		strings.Replace(policyHTTPInitial, `"expected_revision":0`, `"expected_revision":-1`, 1),
		strings.Replace(policyHTTPInitial, `"expected_revision":0`, `"expected_revision":9223372036854775807`, 1),
		strings.Replace(policyHTTPInitial, `"expected_revision":0`, `"expected_revision":"0"`, 1),
		strings.Replace(policyHTTPInitial, `"max_review_files":0`, `"max_review_files":0,"extra":true`, 1),
		strings.Replace(policyHTTPInitial, `"max_review_files":0`, `"max_review_files":0.5`, 1),
		strings.Replace(policyHTTPInitial, `"max_review_files":0`, `"max_review_files":-1`, 1),
		strings.Replace(policyHTTPInitial, `"max_review_files":0`, `"max_review_files":100001`, 1),
		strings.Replace(policyHTTPInitial, `["hosted","cli"]`, `[]`, 1),
		strings.Replace(policyHTTPInitial, `["hosted","cli"]`, `["cli","cli"]`, 1),
		strings.Replace(policyHTTPInitial, `["hosted","cli"]`, `["manual"]`, 1),
		strings.Replace(policyHTTPInitial, `["hosted","cli"]`, `["CLI"]`, 1),
		strings.Replace(policyHTTPInitial, `["hosted","cli"]`, `["cli\ud800"]`, 1),
		strings.Replace(policyHTTPInitial, `["hosted","cli"]`, `[null]`, 1),
		strings.Replace(policyHTTPInitial, `"paused_channels":[]`, `"paused_channels":["hosted","hosted"]`, 1),
		strings.Replace(policyHTTPInitial, `"paused_channels":[]`, `"paused_channels":[null]`, 1),
		strings.Replace(policyHTTPInitial, `"paused_channels":[]`, `"paused_channels":{}`, 1),
		strings.Replace(policyHTTPInitial, `["hosted","cli"]`, `["`+strings.Repeat("a", 256<<10)+`"]`, 1),
	} {
		evidenceHTTPRequire(t, f.request("POST", path, f.admin, "invalid", body), 400, "validation_failed")
	}
	for _, key := range []string{"", " padded "} {
		evidenceHTTPRequire(t, f.request("POST", path, f.admin, key, policyHTTPInitial), 400, "idempotency_key_required")
	}
	evidenceHTTPRequire(t, f.requestWithKeys("POST", path, f.admin, []string{"one", "two"}, policyHTTPInitial), 400, "idempotency_key_required")
	for _, query := range []string{"?limit=1", "?unexpected=1", "?x=1&x=2", "?x=%zz"} {
		evidenceHTTPRequire(t, f.request("GET", path+query, f.reader, "", ""), 400, "validation_failed")
		evidenceHTTPRequire(t, f.request("POST", path+query, f.admin, "invalid", policyHTTPInitial), 400, "validation_failed")
	}
	for _, method := range []string{"PUT", "PATCH", "DELETE"} {
		if w := f.request(method, path, f.admin, "invalid", policyHTTPInitial); w.Code != 405 {
			t.Fatal("unsupported policy method accepted")
		}
	}
	if policyState(t, f) != before {
		t.Fatal("strict request refusal created policy/event/receipt")
	}
	// Genuine zero and empty values are valid only when supplied explicitly.
	evidenceHTTPReceipt(t, f.request("POST", path, f.admin, "valid-explicit-zero", policyHTTPInitial), attemptProject, "policy_configured", 1)
}
