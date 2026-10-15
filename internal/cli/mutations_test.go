package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
)

func mutationProfiles(t *testing.T, f *serviceFixture, address string, identities map[string][2]string) string {
	t.Helper()
	directory := t.TempDir()
	credential := writeCredential(t, directory, "admin", f.admin)
	entries := make(map[string]any)
	for name, identity := range identities {
		entries[name] = map[string]any{"server_url": address, "instance_id": f.store.InstanceID(), "project_id": identity[0], "worker_id": identity[1], "credential_file": credential}
	}
	path := filepath.Join(directory, "profiles.json")
	if err := os.WriteFile(path, encode(t, map[string]any{"version": 1, "profiles": entries}), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mutationInvoke(t *testing.T, path, profile string, args ...string) (int, string, string) {
	t.Helper()
	return run(append([]string{"--profiles-file", path}, args...), map[string]string{"FC_PROFILE": profile})
}

func mutationSuccess(t *testing.T, path, profile string, args ...string) map[string]any {
	t.Helper()
	code, stdout, stderr := mutationInvoke(t, path, profile, args...)
	if code != cli.ExitSuccess || stderr != "" {
		t.Fatalf("mutation code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	var receipt map[string]any
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil || len(receipt) != 3 {
		t.Fatalf("compact receipt: %v %s", err, stdout)
	}
	return receipt
}

func TestMutationCommandsRecoverReceiptsAndPreserveBoundIdentity(t *testing.T) {
	f := setupServiceFixture(t)
	a, b := f.project(t, "mutation-alpha"), f.project(t, "mutation-beta")
	sender, recipient, other := f.worker(t, a, "Builder"), f.worker(t, a, "Overseer"), f.worker(t, b, "Overseer")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"sender": {a, sender}, "recipient": {a, recipient}, "other": {b, other}})
	providerCalls := f.resolverHit.Load()
	brief := "PRIVATE_MUTATION_BRIEF"
	created := mutationSuccess(t, path, "sender", "jobs", "create", "--key", "create-once", "--title", "  Task  ", "--brief", brief, "--worker", recipient)
	replayed := mutationSuccess(t, path, "sender", "jobs", "create", "--key", "create-once", "--title", "Task", "--brief", brief, "--worker", recipient)
	if created["id"] != replayed["id"] || created["revision"] != float64(1) || created["outcome"] != "created" || strings.Contains(string(mustJSON(t, created)), brief) {
		t.Fatal("create receipt/replay leaked or changed")
	}
	id := created["id"].(string)
	edit := mutationSuccess(t, path, "sender", "jobs", "edit", id, "--key", "edit-once", "--expect-revision", "1", "--brief", "", "--worker", "", "--status", "blocked")
	if edit["revision"] != float64(2) || edit["outcome"] != "updated" {
		t.Fatal("edit did not advance one revision")
	}
	_ = mutationSuccess(t, path, "sender", "jobs", "edit", id, "--key", "edit-once", "--expect-revision", "1", "--brief", "", "--worker", "", "--status", "blocked")
	code, stdout, stderr := mutationInvoke(t, path, "sender", "jobs", "edit", id, "--key", "stale-edit", "--expect-revision", "1", "--status", "active")
	if code != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, `"current_revision":2`) || strings.Contains(stderr, "outcome_unknown") {
		t.Fatalf("stale edit diagnostics: %d %s %s", code, stdout, stderr)
	}
	state := f.call(t, "GET", "/api/v1/projects/"+a+"/jobs/"+id, f.admin, "", nil)
	if state["brief"] != "" || state["worker_id"] != "" || state["status"] != "blocked" {
		t.Fatal("explicit empty edits were lost")
	}
	body := "  PRIVATE_MUTATION_MESSAGE\n"
	sent := mutationSuccess(t, path, "sender", "inbox", "send", "--key", "send-once", "--to", recipient, "--body", body)
	again := mutationSuccess(t, path, "sender", "inbox", "send", "--key", "send-once", "--to", recipient, "--body", body)
	if sent["id"] != again["id"] || sent["outcome"] != "sent" || strings.Contains(string(mustJSON(t, sent)), body) {
		t.Fatal("message retry or compact receipt failed")
	}
	message := sent["id"].(string)
	code, stdout, stderr = mutationInvoke(t, path, "sender", "inbox", "send", "--key", "send-once", "--to", recipient, "--body", "changed")
	if code != cli.ExitConflict || stdout != "" || !strings.Contains(stderr, "idempotency_conflict") || strings.Contains(stderr, "outcome_unknown") {
		t.Fatal("changed request did not conflict safely")
	}
	code, stdout, stderr = mutationInvoke(t, path, "sender", "inbox", "ack", message, "--key", "wrong-recipient")
	if code != cli.ExitService || stdout != "" || !strings.Contains(stderr, "forbidden") {
		t.Fatal("sender could acknowledge recipient state")
	}
	seen := mutationSuccess(t, path, "recipient", "inbox", "seen", message, "--key", "seen-once")
	ack := mutationSuccess(t, path, "recipient", "inbox", "ack", message, "--key", "ack-once")
	unchanged := mutationSuccess(t, path, "recipient", "inbox", "ack", message, "--key", "ack-again")
	if seen["outcome"] != "seen" || ack["outcome"] != "acknowledged" || unchanged["outcome"] != "unchanged" {
		t.Fatal("recipient transitions changed contract")
	}
	reply := mutationSuccess(t, path, "recipient", "inbox", "send", "--key", "reply-once", "--to", sender, "--reply-to", message, "--body", "Reply")
	if reply["conversation_id"] != sent["conversation_id"] {
		t.Fatal("reply split conversation")
	}
	code, stdout, stderr = mutationInvoke(t, path, "other", "jobs", "edit", id, "--key", "foreign-edit", "--expect-revision", "2", "--status", "active")
	if code != cli.ExitService || stdout != "" || !strings.Contains(stderr, "not_found") || strings.Contains(stderr, brief) || strings.Contains(stderr, body) {
		t.Fatal("cross-project edit or privacy failed")
	}
	if f.resolverHit.Load() != providerCalls {
		t.Fatal("ordinary client mutations performed provider work")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMutationArgumentsFailBeforeProfileCredentialsOrNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"jobs", "create", "--title", "Task"},
		{"jobs", "create", "--key", "same", "--key", "other", "--title", "Task"},
		{"jobs", "create", "--key", "bad\nkey", "--title", "Task"},
		{"jobs", "create", "--key", "same", "--title", " "},
		{"jobs", "edit", "job-1", "--key", "same", "--expect-revision", "1"},
		{"jobs", "edit", "job-1", "--key", "same", "--expect-revision", "0", "--brief", ""},
		{"jobs", "edit", "job-1", "--key", "same", "--expect-revision", "9223372036854775807", "--brief", ""},
		{"inbox", "send", "--key", "same", "--to", "worker-1", "--body", " "},
		{"inbox", "send", "--key", "same", "--to", "../worker", "--body", "Hello"},
		{"inbox", "send", "--key", "same", "--to", "worker-1", "--body", string([]byte{0xff})},
		{"inbox", "seen", "msg-1", "--key", "same", "--unread"},
		{"inbox", "ack", "msg-1", "--key", "same", "--body", "irrelevant"},
	} {
		failure(t, append([]string{"--profiles-file", "/missing/private-profile.json"}, args...), nil, cli.ExitUsage, "invalid_arguments")
	}
}

type failedMutationOutput struct{}

func (failedMutationOutput) Write([]byte) (int, error) {
	return 0, errors.New("PRIVATE_OUTPUT_DESTINATION")
}

func TestCommittedMutationWithFailedOutputRemainsRecoverable(t *testing.T) {
	f := setupServiceFixture(t)
	p := f.project(t, "output-failure")
	worker := f.worker(t, p, "Builder")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"worker": {p, worker}})
	args := []string{"jobs", "create", "--key", "recover-output", "--title", "Task"}
	var stderr bytes.Buffer
	code := cli.RunContext(context.Background(), append([]string{"--profiles-file", path, "--profile", "worker"}, args...), nil, failedMutationOutput{}, &stderr)
	if code != cli.ExitConfig || !strings.Contains(stderr.String(), `"outcome_unknown":true`) || strings.Contains(stderr.String(), "PRIVATE_OUTPUT_DESTINATION") {
		t.Fatal("failed receipt output lost recovery meaning or leaked the writer error")
	}
	_ = mutationSuccess(t, path, "worker", args...)
	page := f.call(t, "GET", "/api/v1/projects/"+p+"/jobs", f.admin, "", nil)
	if len(page["items"].([]any)) != 1 {
		t.Fatal("output recovery created a second job")
	}
}

func TestCLICommittedDisconnectRequiresSameKeyRecoveryWithoutRetry(t *testing.T) {
	f := setupServiceFixture(t)
	p := f.project(t, "disconnect")
	worker := f.worker(t, p, "Builder")
	var mutations atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs") && mutations.Add(1) == 1 {
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, r)
			if response.Code != 201 {
				t.Error("fixture mutation did not commit")
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		f.handler.ServeHTTP(w, r)
	}))
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"worker": {p, worker}})
	args := []string{"jobs", "create", "--key", "recover-once", "--title", "Task", "--brief", "PRIVATE_COMMITTED_BRIEF"}
	code, stdout, stderr := mutationInvoke(t, path, "worker", args...)
	if code != cli.ExitService || stdout != "" || !strings.Contains(stderr, `"outcome_unknown":true`) || strings.Contains(stderr, "PRIVATE_COMMITTED_BRIEF") || strings.Contains(stderr, "recover-once") || mutations.Load() != 1 {
		t.Fatalf("lost receipt did not remain safe/unknown/single dispatch: %d %s %s calls=%d", code, stdout, stderr, mutations.Load())
	}
	_ = mutationSuccess(t, path, "worker", args...)
	page := f.call(t, "GET", "/api/v1/projects/"+p+"/jobs", f.admin, "", nil)
	if len(page["items"].([]any)) != 1 || mutations.Load() != 2 {
		t.Fatal("recovery duplicated job or dispatched hidden retries")
	}
	// A caller cancellation before connecting is a known unsent operation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, diagnostic bytes.Buffer
	code = cli.RunContext(ctx, append([]string{"--profiles-file", path, "--profile", "worker"}, args...), nil, &out, &diagnostic)
	if code != cli.ExitCancelled || out.Len() != 0 || strings.Contains(diagnostic.String(), "outcome_unknown") || mutations.Load() != 2 {
		t.Fatal("pre-dispatch cancellation was mislabeled or sent")
	}
}

func TestMutationFreeformValuesPreserveLeadingDashes(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "dash-values")
	sender, recipient := f.worker(t, project, "Builder"), f.worker(t, project, "Reviewer")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"sender": {project, sender}})
	providerCalls := f.resolverHit.Load()
	for _, form := range []string{"separate", "equals"} {
		for _, value := range []struct{ name, title, brief, body string }{
			{"markdown", "--- release title", "---\n# Brief\n--key\n", "---\n## Message\n--status literal\n"},
			{"literal-option", "--key", "--key", "--key"},
		} {
			t.Run(form+"/"+value.name, func(t *testing.T) {
				option := func(flag, text string) []string {
					if form == "equals" {
						return []string{flag + "=" + text}
					}
					return []string{flag, text}
				}
				key := form + "-" + value.name
				args := []string{"jobs", "create", "--key", key + "-create"}
				args = append(args, option("--title", value.title)...)
				args = append(args, option("--brief", value.brief)...)
				created := mutationSuccess(t, path, "sender", args...)
				jobID := created["id"].(string)
				job := f.call(t, "GET", "/api/v1/projects/"+project+"/jobs/"+jobID, f.admin, "", nil)
				if job["title"] != value.title || job["brief"] != value.brief {
					t.Fatal("create changed freeform value bytes")
				}
				title, brief := value.title+" edited", value.brief+"\n--literal-after-edit"
				args = []string{"jobs", "edit", jobID, "--key", key + "-edit", "--expect-revision", "1"}
				args = append(args, option("--title", title)...)
				args = append(args, option("--brief", brief)...)
				mutationSuccess(t, path, "sender", args...)
				job = f.call(t, "GET", "/api/v1/projects/"+project+"/jobs/"+jobID, f.admin, "", nil)
				if job["title"] != title || job["brief"] != brief || job["revision"] != float64(2) {
					t.Fatal("edit changed freeform value bytes or revision")
				}
				args = []string{"inbox", "send", "--key", key + "-send", "--to", recipient}
				args = append(args, option("--body", value.body)...)
				sent := mutationSuccess(t, path, "sender", args...)
				thread := f.call(t, "GET", "/api/v1/projects/"+project+"/conversations/"+sent["conversation_id"].(string), f.admin, "", nil)
				items := thread["items"].([]any)
				if len(items) != 1 || items[0].(map[string]any)["body"] != value.body {
					t.Fatal("send changed freeform message bytes")
				}
			})
		}
	}
	if f.resolverHit.Load() != providerCalls {
		t.Fatal("freeform command workflow contacted a provider")
	}
}

func TestDashLikeMalformedOptionsRejectBeforeCredentialsAndHTTP(t *testing.T) {
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer service.Close()
	config := profiles()
	entry := config["profiles"].(map[string]any)["alpha-overseer"].(map[string]any)
	entry["server_url"] = service.URL
	entry["credential_file"] = filepath.Join(t.TempDir(), "absent-private-credential")
	path := fixture(t, encode(t, config))
	for _, args := range [][]string{
		{"jobs", "create", "--key", "key", "--title"},
		{"jobs", "create", "--key", "key", "--title", "Task", "--brief"},
		{"inbox", "send", "--key", "key", "--to", "worker-one", "--body"},
		{"jobs", "create", "--key", "key", "--title", "--key", "--unexpected"},
		{"inbox", "send", "--key", "key", "--to", "worker-one", "--body", "--key", "--key", "duplicate"},
		{"jobs", "create", "--key", "key", "--title", "Task", "--status", "--key"},
		{"inbox", "send", "--key", "key", "--to", "../worker", "--body", "--key"},
		{"jobs", "list", "--limit", "--key"},
		{"jobs", "list", "--cursor", "--opaque", "--unexpected"},
		{"workers", "list", "--cursor"},
		{"jobs", "list", "--worker", "../worker"},
	} {
		failure(t, append([]string{"--profiles-file", path, "--profile", "alpha-overseer"}, args...), nil, cli.ExitUsage, "invalid_arguments")
	}
	if requests.Load() != 0 {
		t.Fatal("malformed option interpretation contacted HTTP")
	}
}
