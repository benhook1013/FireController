package reviewexec

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestExecutionConfigurationReadStoredFactsWithoutCapabilityOrMutation(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	f.configure(t, attemptProject, "test-capability", 1, true)
	var err error
	// Catalog availability is not part of a stored-configuration read.
	f.e, err = New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	f.handler = mux
	before := policyState(t, f)
	w := f.request("GET", "/api/v1/projects/"+attemptProject+"/reviews/execution/configuration", f.reader, "", "")
	var result ExecutionConfiguration
	var fields map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || json.Unmarshal(w.Body.Bytes(), &fields) != nil || len(fields) != 7 {
		t.Fatal("stored configuration wire shape", w.Code, w.Body.String())
	}
	if result.ProjectID != attemptProject || result.Revision != 2 || result.CapabilityID != "test-capability" || result.Mode != "independent" || !reflect.DeepEqual(result.Queue, []int64{2, 4}) || !reflect.DeepEqual(result.Channels, []string{"cli", "hosted"}) || !result.Paused {
		t.Fatal("stored facts were replaced by runnable/default claims", result)
	}
	for _, forbidden := range []string{"expected_revision", "account", "fingerprint", "executable", "credential", "path"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatal("configuration read leaked stored assertion or private catalog fields")
		}
	}
	if policyState(t, f) != before || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("configuration read mutated domain state")
	}
}

func TestExecutionConfigurationReadScopeQueryAndInstanceFailures(t *testing.T) {
	f := newAttemptFixture(t)
	path := "/api/v1/projects/" + attemptProject + "/reviews/execution/configuration"
	before := policyState(t, f)
	w := f.request("GET", path, f.reader, "", "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"reviews_unconfigured"`) || policyState(t, f) != before {
		t.Fatal("missing configuration silently became an empty passing result", w.Code, w.Body.String())
	}
	f.configure(t, attemptProject, "test-capability", 0, false)
	before = policyState(t, f)
	for _, test := range []struct{ name, path, token, instance string }{
		{"unknown-query", path + "?refresh=true", f.reader, f.store.InstanceID()},
		{"repeated-query", path + "?cursor=x&cursor=y", f.reader, f.store.InstanceID()},
		{"foreign-project", strings.Replace(path, attemptProject, otherAttemptProject, 1), f.reader, f.store.InstanceID()},
		{"foreign-credential", path, f.other, f.store.InstanceID()},
		{"missing-access", path, "", f.store.InstanceID()},
		{"wrong-instance", path, f.reader, "other-instance"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", test.path, nil)
			if test.token != "" {
				r.Header.Set("Authorization", "Bearer "+test.token)
			}
			r.Header.Set("X-FC-Instance", test.instance)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code < 400 || strings.Contains(w.Body.String(), "test-capability") || policyState(t, f) != before {
				t.Fatal("invalid scoped read exposed configuration or mutated state", w.Code, w.Body.String())
			}
		})
	}
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`)
	w = f.request("GET", path, f.reader, "", "")
	if w.Code != 401 {
		t.Fatal("revoked reader retained access", w.Code, w.Body.String())
	}
}

func TestExecutionConfigurationReadAndRequestDistinguishMissingFromOutOfScope(t *testing.T) {
	f := newAttemptFixture(t)
	mux := http.NewServeMux()
	f.e.Register(mux)
	service := httptest.NewServer(mux)
	defer service.Close()
	base := service.URL + "/api/v1/projects/" + attemptProject + "/reviews"
	call := func(method, suffix, body, key string) (int, string) {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), method, base+suffix, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+f.writer)
		r.Header.Set("X-FC-Instance", f.store.InstanceID())
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", key)
		}
		response, err := service.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err = json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, decoded.Error.Code
	}
	request := `{"expected_configuration_revision":1,"pull_number":2,"channel":"cli","head_sha":"` + strings.Repeat("a", 40) + `"}`
	before := policyState(t, f)
	readStatus, readCode := call("GET", "/execution/configuration", "", "")
	requestStatus, requestCode := call("POST", "/requests", request, "missing-config-request")
	if readStatus != http.StatusConflict || requestStatus != readStatus || readCode != "reviews_unconfigured" || requestCode != readCode {
		t.Fatalf("missing configuration contract differs: read=%d/%s request=%d/%s", readStatus, readCode, requestStatus, requestCode)
	}
	if policyState(t, f) != before || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 {
		t.Fatal("missing configuration refusal mutated state")
	}
	zero := int64(0)
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "scoped-config", Configuration{ExpectedRevision: &zero, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli"}}); err != nil {
		t.Fatal(err)
	}
	if status, code := call("GET", "/execution/configuration", "", ""); status != 200 || code != "" {
		t.Fatalf("stored configuration read failed: %d/%s", status, code)
	}
	before = policyState(t, f)
	for _, test := range []struct{ key, body string }{
		{"outside-queue", strings.Replace(request, `"pull_number":2`, `"pull_number":99`, 1)},
		{"outside-channel", strings.Replace(request, `"channel":"cli"`, `"channel":"hosted"`, 1)},
	} {
		status, code := call("POST", "/requests", test.body, test.key)
		if status != http.StatusConflict || code != "review_not_configured" {
			t.Fatalf("%s lost distinct configured-scope refusal: %d/%s", test.key, status, code)
		}
	}
	if policyState(t, f) != before || attemptCount(t, f.store, `SELECT count(*) FROM review_operations`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_attempts`) != 0 || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_reservations`) != 0 {
		t.Fatal("out-of-scope refusal created execution or durable mutation effects")
	}
}
