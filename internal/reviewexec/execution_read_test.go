package reviewexec

import (
	"encoding/json"
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
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"code":"review_not_configured"`) || policyState(t, f) != before {
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
