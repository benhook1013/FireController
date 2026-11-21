package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

const multiProjectConnectorPath = "/api/v1/operator/connector-credentials"

type testConnectorGrant struct {
	ProjectID   string   `json:"project_id"`
	WorkerID    string   `json:"worker_id"`
	Permissions []string `json:"permissions"`
}

func connectorGrantBody(t *testing.T, token string, projects ...testConnectorGrant) string {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	body, err := json.Marshal(map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "projects": projects})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestProvisionMultiProjectConnectorAtomicAndScoped(t *testing.T) {
	f := setup(t)
	one, two := f.project(t, "owner/one"), f.project(t, "owner/two")
	worker1, worker2 := f.worker(t, one, "First"), f.worker(t, two, "Second")
	project1 := testConnectorGrant{one, worker1, []string{"write", "read"}}
	project2 := testConnectorGrant{two, worker2, []string{"read"}}
	const token, manager = "synthetic-cross-project-connector", "synthetic-local-manager"
	projectManagerGrant(t, f, one, manager)
	body := connectorGrantBody(t, token, project1, project2)

	req := func(method, path, auth, key, worker, input string, status int) map[string]any {
		t.Helper()
		return require(t, request(f.handler, method, path, auth, key, worker, input), status)
	}
	beforeCreds, beforeEvents := count(t, f.store, "credentials"), count(t, f.store, "events")
	req("POST", multiProjectConnectorPath, manager, "deny-manager", "", body, http.StatusForbidden)
	req("POST", multiProjectConnectorPath, f.admin, "reject-duplicate-project", "", connectorGrantBody(t, token, project1, project1), http.StatusBadRequest)
	invalid := project2
	invalid.WorkerID = worker1
	req("POST", multiProjectConnectorPath, f.admin, "wrong-project-worker-rollback", "", connectorGrantBody(t, token, project1, invalid), http.StatusNotFound)
	if count(t, f.store, "credentials") != beforeCreds || count(t, f.store, "events") != beforeEvents {
		t.Fatal("rejected request committed a partial credential or audit event")
	}

	receipt := req("POST", multiProjectConnectorPath, f.admin, "create-connector", "", body, http.StatusOK)
	credential := receipt["id"].(string)
	replayed := req("POST", multiProjectConnectorPath, f.admin, "create-connector", "", connectorGrantBody(t, token, project2, project1), http.StatusOK)
	if !reflect.DeepEqual(receipt, replayed) {
		t.Fatal("reordered exact grant did not replay")
	}
	if count(t, f.store, "credentials") != beforeCreds+1 || count(t, f.store, "events") != beforeEvents+2 {
		t.Fatal("issuance did not create exactly one credential and one audit event per project")
	}
	for _, project := range []testConnectorGrant{project1, project2} {
		var boundWorker string
		var instanceAdmin, projectManager int
		const query = `SELECT b.worker_id,c.is_admin,g.can_manage FROM connector_credentials b
JOIN credentials c ON c.id=b.credential_id JOIN grants g USING(credential_id,project_id)
WHERE b.credential_id=? AND b.project_id=?`
		if err := f.store.DB().QueryRow(query, credential, project.ProjectID).Scan(&boundWorker, &instanceAdmin, &projectManager); err != nil || boundWorker != project.WorkerID || instanceAdmin != 0 || projectManager != 0 {
			t.Fatal("wrong retained worker or unintended manager privileges", err)
		}
	}

	req("GET", "/api/v1/projects/"+one, token, "", worker1, "", http.StatusOK)
	req("GET", "/api/v1/projects/"+two, token, "", worker2, "", http.StatusOK)
	req("GET", "/api/v1/projects/"+two, token, "", worker1, "", http.StatusForbidden)
	req("POST", "/api/v1/projects/"+one+"/workers", token, "write-one", worker1, `{"name":"first project"}`, http.StatusOK)
	req("POST", "/api/v1/projects/"+two+"/workers", token, "deny-read-only", worker2, `{"name":"second project"}`, http.StatusForbidden)
	req("POST", multiProjectConnectorPath, token, "deny-self-escalation", "", body, http.StatusForbidden)

	req("DELETE", "/api/v1/projects/"+one+"/credentials/"+credential, manager, "revoke-project-one", "", "", http.StatusOK)
	req("GET", "/api/v1/projects/"+one, token, "", worker1, "", http.StatusUnauthorized)
	req("GET", "/api/v1/projects/"+two, token, "", worker2, "", http.StatusOK)
}
