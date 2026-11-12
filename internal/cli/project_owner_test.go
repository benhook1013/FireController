package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestProjectOwnerCLIExistingOperationsAndPrivateCredentialHash(t *testing.T) {
	f, project, worker := reviewExecutionFixture(t)
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
	policyFile := file("policy.json", client.ConfigureReviewPolicyRequest{PausedChannels: []string{}, ExpectedRevision: 1, RequiredChannels: []string{"cli", "hosted"}, MaxReviewFiles: 88, RoundDefaults: &client.ReviewRoundDefaults{Hosted: client.ReviewRoundBounds{Minimum: 2, Maximum: 5}, CLI: client.ReviewRoundBounds{Minimum: 0, Maximum: 8}}})
	var policy client.ReviewRequestReceipt
	if json.Unmarshal([]byte(invoke("reviews", "policy-configure", "--file", policyFile, "--key", "policy-change")), &policy) != nil || policy.Revision != 2 {
		t.Fatal(policy)
	}
	var current client.ReviewPolicyRecord
	if json.Unmarshal([]byte(invoke("reviews", "policy")), &current) != nil || current.Policy.MaxReviewFiles != 88 || current.Policy.RoundDefaults == nil || current.Policy.RoundDefaults.Hosted.Maximum != 5 || current.Policy.RoundDefaults.CLI.Minimum != 0 {
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
	message := f.call(t, "POST", "/api/v1/projects/"+project+"/messages", f.admin, worker, map[string]any{"recipient_worker_id": created.ID, "body": "Selected inbox"})["id"].(string)
	f.call(t, "POST", "/api/v1/projects/"+project+"/messages", f.admin, created.ID, map[string]any{"recipient_worker_id": worker, "body": "Selected outbox"})
	var inbox client.Page[client.InboxItem]
	if json.Unmarshal([]byte(invoke("inbox", "list", "--worker", created.ID)), &inbox) != nil || len(inbox.Items) != 1 || inbox.Items[0].RecipientWorkerID != created.ID {
		t.Fatal(inbox)
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
	for _, op := range [][]string{{"reviews", "policy"}, {"reviews", "policy-configure"}, {"reviews", "execution-configure"}, {"workers", "create"}, {"credentials", "grant"}, {"credentials", "revoke"}, {"credentials", "update"}} {
		code, out, stderr := run(append(op, "--help"), nil)
		if code != 0 || stderr != "" || !strings.Contains(out, "documentation") {
			t.Fatal(op, code, out, stderr)
		}
		if op[0] == "reviews" && op[1] == "policy-configure" {
			var help struct {
				FileExample struct {
					ExpectedRevision *int64 `json:"expected_revision"`
				} `json:"file_example"`
			}
			if err := json.Unmarshal([]byte(out), &help); err != nil || help.FileExample.ExpectedRevision == nil || *help.FileExample.ExpectedRevision != 0 || !strings.Contains(out, "reviews_policy_unconfigured") || !strings.Contains(out, "For replacement") {
				t.Fatal("initial policy guidance missing", out, err)
			}
		}
		if op[0] == "credentials" && op[1] == "grant" && (!strings.Contains(out, "32-256 printable nonspace ASCII bytes") || !strings.Contains(out, "surrounding whitespace is trimmed")) {
			t.Fatal("credential-file bounds missing from local help", out)
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
	for _, defaults := range []string{`null`, `{"hosted":{"minimum":0,"maximum":1}}`, `{"hosted":{"minimum":0,"maximum":1},"cli":{"minimum":2,"maximum":1}}`} {
		body := `{"expected_revision":0,"paused_channels":[],"required_channels":["cli"],"max_review_files":0,"round_defaults":` + defaults + `}`
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		code, out, stderr := run([]string{"--profiles-file", "/missing/private-profiles", "reviews", "policy-configure", "--file", p, "--key", "bad"}, nil)
		if code == cli.ExitSuccess || out != "" || !strings.Contains(stderr, "invalid_management_input") {
			t.Fatal("invalid policy defaults reached profile loading", code, out, stderr)
		}
	}
	for _, args := range [][]string{{"workers", "create", "--name", " padded ", "--key", "x"}, {"credentials", "grant", "--token-sha256", "raw secret", "--permission", "read", "--key", "x"}, {"credentials", "update", "bad/id", "--file", p, "--key", "x"}} {
		code, out, stderr := run(append([]string{"--profiles-file", "/missing"}, args...), nil)
		if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_arguments") {
			t.Fatal(args, code, out, stderr)
		}
	}
}

func TestProjectOwnerCLICredentialFileBoundsPreserveHashAndReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, token, padding string
	}{
		{"minimum", strings.Repeat("!", 32), " \t\r\n"},
		{"maximum", strings.Repeat("~", 256), "\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupServiceFixture(t)
			project := f.project(t, "credential-boundary")
			worker := f.worker(t, project, "Owner")
			var leaked atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				if bytes.Contains(body, []byte(tc.token)) || strings.Contains(r.URL.RawQuery, tc.token) {
					leaked.Store(true)
				}
				f.handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"owner": {project, worker}})
			credentialFile := filepath.Join(t.TempDir(), "boundary.token")
			if err := os.WriteFile(credentialFile, []byte(tc.padding+tc.token+tc.padding), 0600); err != nil {
				t.Fatal(err)
			}
			code, out, stderr := mutationInvoke(t, profiles, "owner", "credentials", "grant", "--credential-file", credentialFile, "--permission", "read", "--key", "boundary-grant")
			if code != cli.ExitSuccess || stderr != "" || strings.Contains(out, tc.token) || strings.Contains(out, credentialFile) {
				t.Fatal(code, out, stderr)
			}
			var grant client.ProjectCredentialReceipt
			if json.Unmarshal([]byte(out), &grant) != nil || grant.ID == "" || grant.PrincipalID == "" || grant.ProjectID != project || grant.Outcome != "granted" {
				t.Fatal("invalid boundary grant receipt", out)
			}
			// Digest input must preserve the exact receipt and principal from the trimmed file input.
			sum := sha256.Sum256([]byte(tc.token))
			code, replay, stderr := mutationInvoke(t, profiles, "owner", "credentials", "grant", "--token-sha256", hex.EncodeToString(sum[:]), "--permission", "read", "--key", "boundary-grant")
			if code != cli.ExitSuccess || stderr != "" || replay != out || leaked.Load() {
				t.Fatal("credential digest or replay changed", code, replay, stderr)
			}
			if got := f.call(t, "GET", "/api/v1/projects/"+project, tc.token, worker, nil); got["id"] != project {
				t.Fatal("granted boundary token cannot authenticate", got)
			}
		})
	}
}

func TestProjectOwnerCLICredentialFileRejectsInvalidTokensBeforeHTTP(t *testing.T) {
	f := setupServiceFixture(t)
	project := f.project(t, "invalid-credentials")
	worker := f.worker(t, project, "Owner")
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		f.handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	profiles := mutationProfiles(t, f, server.URL, map[string][2]string{"owner": {project, worker}})
	for _, tc := range []struct {
		name, token string
	}{
		{"empty", ""},
		{"one-byte", "!"},
		{"short", strings.Repeat("!", 31)},
		{"short-padded", " \t" + strings.Repeat("!", 31) + "\r\n"},
		{"long", strings.Repeat("!", 257)},
		{"long-padded", "\t" + strings.Repeat("!", 257) + "\n"},
		{"raw-limit", strings.Repeat("!", 4096)},
		{"past-raw-limit", strings.Repeat("!", 4097)},
		{"excessive-padding", strings.Repeat(" ", 4096) + strings.Repeat("!", 32)},
		{"whitespace-only", " \t\r\n"},
		{"interior-space", strings.Repeat("!", 16) + " " + strings.Repeat("!", 16)},
		{"interior-newline", strings.Repeat("!", 16) + "\n" + strings.Repeat("!", 16)},
		{"control", strings.Repeat("!", 32) + "\x7f"},
		{"non-ascii", strings.Repeat("!", 32) + "é"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentialFile := filepath.Join(t.TempDir(), "rejected.token")
			if err := os.WriteFile(credentialFile, []byte(tc.token), 0600); err != nil {
				t.Fatal(err)
			}
			for _, profileFile := range []string{profiles, filepath.Join(t.TempDir(), "missing-profiles.json")} {
				code, out, stderr := mutationInvoke(t, profileFile, "owner", "credentials", "grant", "--credential-file", credentialFile, "--permission", "read", "--key", "invalid-grant")
				if code != cli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid_management_input") || !strings.Contains(stderr, "no service request was sent") || requests.Load() != 0 {
					t.Fatal("invalid token reached profiles or HTTP", code, out, stderr, requests.Load())
				}
				if strings.Contains(stderr, credentialFile) || tc.token != "" && strings.TrimSpace(tc.token) != "" && strings.Contains(stderr, tc.token) {
					t.Fatal("private credential input reached diagnostics")
				}
			}
		})
	}
}

func TestManagementGroupHelpIsStructuredAndLocal(t *testing.T) {
	for _, tc := range []struct {
		group, documentation string
		operations           []string
	}{
		{"credentials", "docs/local-api.md", []string{"credentials grant --help", "credentials update --help", "credentials revoke --help"}},
		{"workers", "docs/worker-retirement.md", []string{"workers create --help", "workers retirement --help", "workers retire --help", "workers list [--include-retired]"}},
		{"reviews", "docs/review-execution.md", []string{"reviews policy --help", "reviews policy-configure --help", "reviews execution-configure --help", "reviews start --help", "reviews run-read --help"}},
	} {
		for _, argument := range []string{"help", "--help"} {
			t.Run(tc.group+"/"+argument, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				var out, errout bytes.Buffer
				code := cli.RunContext(ctx, []string{"--profiles-file", "/missing/private-profile.json", tc.group, argument}, nil, &out, &errout)
				if code != cli.ExitSuccess || errout.Len() != 0 || out.Len() > 4096 || strings.Contains(out.String(), "private-profile") {
					t.Fatalf("local group help: code=%d stdout=%s stderr=%s", code, &out, &errout)
				}
				var help struct {
					Usage         string   `json:"usage"`
					Required      []string `json:"required"`
					Optional      []string `json:"optional"`
					Semantics     []string `json:"semantics"`
					Documentation string   `json:"documentation"`
				}
				if err := json.Unmarshal(out.Bytes(), &help); err != nil {
					t.Fatalf("help must be a structured object: %v; stdout=%s", err, &out)
				}
				if help.Usage != "<bound launcher> "+tc.group+" help" || help.Required == nil || len(help.Required) != 0 || !slices.Equal(help.Optional, tc.operations) || len(help.Semantics) == 0 || help.Documentation != tc.documentation {
					t.Fatalf("group help lost its command contract: %+v", help)
				}
			})
		}
	}
}
