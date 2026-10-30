package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestProjectOwnerCLIExistingOperationsAndPrivateCredentialHash(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t, true)
	const rawToken = "synthetic-private-delegated-token"
	var leaked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(rawToken)) {
			leaked = true
		}
		if strings.Contains(r.URL.RawQuery, rawToken) {
			leaked = true
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"owner": {project, worker}})
	invoke := func(args ...string) string {
		t.Helper()
		code, out, stderr := mutationInvoke(t, profiles, "owner", args...)
		if code != 0 || stderr != "" || strings.Contains(out, rawToken) || strings.Contains(stderr, rawToken) {
			t.Fatal(code, out, stderr)
		}
		return out
	}
	file := func(name string, value any) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		b, _ := json.Marshal(value)
		if os.WriteFile(p, b, 0600) != nil {
			t.Fatal("private request fixture")
		}
		return p
	}
	policyFile := file("policy.json", client.ConfigureReviewPolicyRequest{ExpectedRevision: 1, RequiredChannels: []string{"cli", "hosted"}, MaxReviewFiles: 88})
	var policy client.ReviewRequestReceipt
	if json.Unmarshal([]byte(invoke("reviews", "policy-configure", "--file", policyFile, "--key", "policy-change")), &policy) != nil || policy.Revision != 2 {
		t.Fatal(policy)
	}
	var current client.ReviewPolicyRecord
	if json.Unmarshal([]byte(invoke("reviews", "policy")), &current) != nil || current.Policy.MaxReviewFiles != 88 {
		t.Fatal(current)
	}
	executionFile := file("execution.json", client.ConfigureReviewExecutionRequest{ExpectedRevision: 1, CapabilityID: "fixture", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli", "hosted"}, Paused: true})
	invoke("reviews", "execution-configure", "--file", executionFile, "--key", "execution-change")
	var created client.WorkerCreateReceipt
	if json.Unmarshal([]byte(invoke("workers", "create", "--name", "CLI Inspector", "--key", "create-worker")), &created) != nil || created.ID == "" {
		t.Fatal(created)
	}
	credentialFile := filepath.Join(t.TempDir(), "new.token")
	if os.WriteFile(credentialFile, []byte(rawToken+"\n"), 0600) != nil {
		t.Fatal("private token fixture")
	}
	var grant client.ProjectCredentialReceipt
	if json.Unmarshal([]byte(invoke("credentials", "grant", "--credential-file", credentialFile, "--permission", "read", "--key", "grant-reader")), &grant) != nil || grant.ID == "" {
		t.Fatal(grant)
	}
	updateFile := file("permissions.json", client.UpdateProjectCredentialRequest{ExpectedPermissions: []string{"read"}, Permissions: []string{"read", "write"}})
	invoke("credentials", "update", grant.ID, "--file", updateFile, "--key", "update-reader")
	staleCode, staleOut, staleErr := mutationInvoke(t, profiles, "owner", "credentials", "update", grant.ID, "--file", updateFile, "--key", "stale-update")
	if staleCode != cli.ExitConflict || staleOut != "" || !strings.Contains(staleErr, "returned current_permissions") {
		t.Fatal(staleCode, staleOut, staleErr)
	}
	invoke("credentials", "revoke", grant.ID, "--key", "revoke-reader")
	invoke("workers", "pause", "--worker", created.ID, "--reason", "Human pause", "--expect-revision", "0", "--key", "pause-inspector")
	var selected client.WorkerPause
	if json.Unmarshal([]byte(invoke("workers", "pause-state", "--worker", created.ID)), &selected) != nil || !selected.Paused || selected.WorkerID != created.ID {
		t.Fatal(selected)
	}
	var own client.WorkerPause
	if json.Unmarshal([]byte(invoke("workers", "pause-state")), &own) != nil || own.Paused || own.WorkerID != worker {
		t.Fatal(own)
	}
	var history client.Page[client.WorkerPauseHistoryEntry]
	if json.Unmarshal([]byte(invoke("workers", "pause-history", "--worker", created.ID)), &history) != nil || len(history.Items) != 1 || history.Items[0].ActorWorkerID != worker {
		t.Fatal(history)
	}
	job := f.call(t, "POST", "/api/v1/projects/"+project+"/jobs", f.admin, worker, map[string]any{"title": "Inspector work", "brief": "Bound target", "worker_id": created.ID})["id"].(string)
	invoke("jobs", "primary-set", job, "--worker", created.ID, "--expect-revision", "0", "--key", "focus-inspector")
	var focus client.PrimaryJob
	if json.Unmarshal([]byte(invoke("jobs", "primary", "--worker", created.ID)), &focus) != nil || focus.JobID != job {
		t.Fatal(focus)
	}
	message := f.call(t, "POST", "/api/v1/projects/"+project+"/messages", f.admin, worker, map[string]any{"recipient_worker_id": created.ID, "body": "Selected inbox"})["id"].(string)
	f.call(t, "POST", "/api/v1/projects/"+project+"/messages", f.admin, created.ID, map[string]any{"recipient_worker_id": worker, "body": "Selected outbox"})
	var inbox client.Page[client.InboxItem]
	if json.Unmarshal([]byte(invoke("inbox", "list", "--worker", created.ID)), &inbox) != nil || len(inbox.Items) != 1 || inbox.Items[0].RecipientWorkerID != created.ID {
		t.Fatal(inbox)
	}
	var outbox client.Page[client.InboxItem]
	if json.Unmarshal([]byte(invoke("inbox", "sent", "--worker", created.ID)), &outbox) != nil || len(outbox.Items) != 1 || outbox.Items[0].SenderWorkerID != created.ID {
		t.Fatal(outbox)
	}
	denied, _, deniedErr := mutationInvoke(t, profiles, "owner", "inbox", "ack", message, "--key", "no-impersonation")
	if denied == 0 || !strings.Contains(deniedErr, "forbidden") {
		t.Fatal(denied, deniedErr)
	}
	if leaked {
		t.Fatal("credential reached query output")
	}
}
func TestProjectOwnerCLIHelpAndMalformedBeforeProfiles(t *testing.T) {
	for _, op := range [][]string{{"reviews", "policy"}, {"reviews", "policy-configure"}, {"reviews", "execution-configure"}, {"reviews", "attempt-settle"}, {"workers", "create"}, {"credentials", "grant"}, {"credentials", "revoke"}, {"credentials", "update"}} {
		code, out, stderr := run(append(op, "--help"), nil)
		if code != 0 || stderr != "" || !strings.Contains(out, "documentation") {
			t.Fatal(op, code, out, stderr)
		}
	}
	p := filepath.Join(t.TempDir(), "incomplete.json")
	if os.WriteFile(p, []byte(`{"expected_revision":1,"capability_id":"fixture","mode":"independent","queue":[],"channels":["cli"]}`), 0600) != nil {
		t.Fatal("private fixture")
	}
	code, out, stderr := run([]string{"--profiles-file", "/missing/private-profiles", "reviews", "execution-configure", "--file", p, "--key", "bad"}, nil)
	if code == cli.ExitSuccess || out != "" || !strings.Contains(stderr, "invalid_management_input") {
		t.Fatal(code, out, stderr)
	}
	for _, args := range [][]string{{"workers", "create", "--name", " padded ", "--key", "x"}, {"credentials", "grant", "--token-sha256", "raw secret", "--permission", "read", "--key", "x"}, {"credentials", "update", "bad/id", "--file", p, "--key", "x"}} {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(args, code, out, stderr)
		}
	}
}
