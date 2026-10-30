package client_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestProjectOwnerClientActualScopedManagement(t *testing.T) {
	f := newMutationFixture(t)
	if err := reviewexec.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	engine, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "owner-capability", Provider: "synthetic", Account: "fixture", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", f.handler)
	engine.Register(mux)
	f.handler = mux
	service := httptest.NewServer(f.handler)
	defer service.Close()
	grant := func(token string, p []string) string {
		sum := sha256.Sum256([]byte(token))
		return f.call(t, "POST", "/api/v1/projects/"+f.project+"/credentials", f.admin, f.sender, map[string]any{"token_sha256": hex.EncodeToString(sum[:]), "permissions": p})["id"].(string)
	}
	const ownerToken = "synthetic-private-manager-token"
	grant(ownerToken, []string{"manage"})
	grant("synthetic-writer-token", []string{"read", "write"})
	grant("synthetic-reader-token", []string{"read"})
	owner := f.newClient(t, ownerToken, f.sender, service.URL)
	writer := f.newClient(t, "synthetic-writer-token", f.sender, service.URL)
	reader := f.newClient(t, "synthetic-reader-token", f.sender, service.URL)
	policy := client.ConfigureReviewPolicyRequest{RequiredChannels: []string{"cli", "hosted"}, MaxReviewFiles: 99}
	_, err = writer.ConfigureReviewPolicy(t.Context(), "writer-policy", policy)
	checkError(t, err, "forbidden")
	first, err := owner.ConfigureReviewPolicy(t.Context(), "owner-policy", policy)
	if err != nil || first.Revision != 1 {
		t.Fatal(first, err)
	}
	replay, err := owner.ConfigureReviewPolicy(t.Context(), "owner-policy", policy)
	if err != nil || !reflect.DeepEqual(first, replay) {
		t.Fatal(replay, err)
	}
	record, err := reader.ReviewPolicy(t.Context())
	if err != nil || record.Policy.MaxReviewFiles != 99 || record.ProjectID != f.project {
		t.Fatal(record, err)
	}
	_, err = owner.ConfigureReviewPolicy(t.Context(), "stale-policy", policy)
	checkError(t, err, "review_conflict")
	initial := client.ConfigureReviewExecutionRequest{CapabilityID: "owner-capability", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli", "hosted"}}
	f.call(t, "POST", "/api/v1/projects/"+f.project+"/reviews/execution/configuration", f.admin, f.sender, initial)
	config := client.ConfigureReviewExecutionRequest{ExpectedRevision: 1, CapabilityID: "owner-capability", Mode: "independent", Queue: []int64{}, Channels: []string{"cli", "hosted"}}
	_, err = writer.ConfigureReviewExecution(t.Context(), "writer-execution", config)
	checkError(t, err, "forbidden")
	configured, err := owner.ConfigureReviewExecution(t.Context(), "owner-execution", config)
	if err != nil || configured.Revision != 2 {
		t.Fatalf("configuration: %+v error: %#v", configured, err)
	}
	current, err := reader.ReviewExecution(t.Context())
	if err != nil || current.Queue == nil || len(current.Queue) != 0 {
		t.Fatal(current, err)
	}
	worker, err := writer.CreateWorker(t.Context(), "create-inspector", client.CreateWorkerRequest{Name: "Inspector"})
	if err != nil || worker.Outcome != "created" {
		t.Fatal(worker, err)
	}
	_, err = reader.CreateWorker(t.Context(), "read-create", client.CreateWorkerRequest{Name: "Denied"})
	checkError(t, err, "forbidden")
	digest := sha256.Sum256([]byte("synthetic-new-nonadmin-token"))
	newGrant, err := owner.GrantProjectCredential(t.Context(), "create-grant", client.GrantProjectCredentialRequest{TokenSHA256: hex.EncodeToString(digest[:]), Permissions: []string{"read"}})
	if err != nil || newGrant.PrincipalID == "" {
		t.Fatal(newGrant, err)
	}
	update := client.UpdateProjectCredentialRequest{ExpectedPermissions: []string{"read"}, Permissions: []string{"manage"}}
	changed, err := owner.UpdateProjectCredential(t.Context(), "update-grant", newGrant.ID, update)
	if err != nil || !reflect.DeepEqual(changed.Permissions, []string{"read", "write", "manage"}) {
		t.Fatal(changed, err)
	}
	_, err = owner.UpdateProjectCredential(t.Context(), "stale-grant", newGrant.ID, update)
	checkError(t, err, "credential_permissions_changed")
	var mismatch *client.Error
	if !errors.As(err, &mismatch) || mismatch.Details == nil {
		t.Fatal("permission guard lost current set", err)
	}
	revoked, err := owner.RevokeProjectCredential(t.Context(), "revoke-grant", newGrant.ID)
	if err != nil || revoked.Outcome != "revoked" {
		t.Fatal(revoked, err)
	}
	other := f.call(t, "POST", "/api/v1/projects", f.admin, "", map[string]any{"name": "other", "repository": "owner/other"})["id"].(string)
	otherDigest := sha256.Sum256([]byte("other-project-credential"))
	foreign := f.call(t, "POST", "/api/v1/projects/"+other+"/credentials", f.admin, "", map[string]any{"token_sha256": hex.EncodeToString(otherDigest[:]), "permissions": []string{"read"}})["id"].(string)
	_, err = owner.RevokeProjectCredential(t.Context(), "foreign-revoke", foreign)
	var unavailable *client.Error
	if !errors.As(err, &unavailable) || unavailable.Status != 404 {
		t.Fatal(err)
	}
}

func TestSelectedWorkerClientKeepsActingIdentity(t *testing.T) {
	f := newMutationFixture(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	c := f.newClient(t, f.admin, f.sender, service.URL)
	job, err := c.CreateJob(t.Context(), "other-worker-job", client.CreateJobRequest{Title: "Selected worker work", Brief: "One assignment", WorkerID: f.recipient})
	if err != nil {
		t.Fatal(err)
	}
	pause, err := c.ReplaceWorkerPause(t.Context(), "pause-selected", client.ReplaceWorkerPauseRequest{Paused: true, Reason: "Human work pause"}, f.recipient)
	if err != nil || pause.ID != f.recipient {
		t.Fatal(pause, err)
	}
	own, err := c.WorkerPause(t.Context())
	if err != nil || own.WorkerID != f.sender || own.Paused {
		t.Fatal(own, err)
	}
	selected, err := c.WorkerPause(t.Context(), f.recipient)
	if err != nil || !selected.Paused {
		t.Fatal(selected, err)
	}
	history, err := c.WorkerPauseHistory(t.Context(), client.PageOptions{}, f.recipient)
	if err != nil || len(history.Items) != 1 || history.Items[0].ActorWorkerID != f.sender || history.Items[0].WorkerID != f.recipient {
		t.Fatal(history, err)
	}
	primary, err := c.ReplacePrimaryJob(t.Context(), "selected-focus", client.ReplacePrimaryJobRequest{JobID: job.ID}, f.recipient)
	if err != nil || primary.ID != f.recipient {
		t.Fatal(primary, err)
	}
	focus, err := c.PrimaryJob(t.Context(), f.recipient)
	if err != nil || focus.JobID != job.ID {
		t.Fatal(focus, err)
	}
	ownFocus, err := c.PrimaryJob(t.Context())
	if err != nil || ownFocus.JobID != "" {
		t.Fatal(ownFocus, err)
	}
	focusHistory, err := c.PrimaryJobHistory(t.Context(), client.PageOptions{}, f.recipient)
	if err != nil || len(focusHistory.Items) != 1 || focusHistory.Items[0].ActorWorkerID != f.sender {
		t.Fatal(focusHistory, err)
	}
	_, err = c.PrimaryJob(t.Context(), "other-project-worker")
	checkError(t, err, "worker_not_found")
}

func TestOwnerClientLocalValidation(t *testing.T) {
	c := &client.Client{}
	for _, invoke := range []func() error{
		func() error {
			_, e := c.ConfigureReviewPolicy(t.Context(), "k", client.ConfigureReviewPolicyRequest{})
			return e
		},
		func() error {
			_, e := c.ConfigureReviewExecution(t.Context(), "k", client.ConfigureReviewExecutionRequest{CapabilityID: "x", Mode: "independent", Channels: []string{"cli"}})
			return e
		},
		func() error {
			_, e := c.SettleReviewAttempt(t.Context(), "k", "review_attempt_"+strings.Repeat("a", 32), client.SettleReviewAttemptRequest{ExpectedRevision: 1, ExpectedRepositoryID: 1, ExternalActionStopped: true, Reason: "Stopped", EvidenceReference: "proof"})
			return e
		},
		func() error {
			_, e := c.GrantProjectCredential(t.Context(), "k", client.GrantProjectCredentialRequest{TokenSHA256: "raw credential", Permissions: []string{"manage"}})
			return e
		},
		func() error {
			_, e := c.CreateWorker(t.Context(), "k", client.CreateWorkerRequest{Name: " padded "})
			return e
		},
	} {
		if err := invoke(); err == nil {
			t.Fatal("invalid management input reached service")
		}
	}
}

func TestOwnerSettlementClientPreservesExactOriginalAssertions(t *testing.T) {
	attempt := "review_attempt_" + strings.Repeat("a", 32)
	input := client.SettleReviewAttemptRequest{ExpectedRevision: 7, ExpectedRepositoryID: 42, ExpectedHeadSHA: strings.Repeat("b", 40), ExpectedExternalID: "retained-original", ExternalActionStopped: true, Reason: "External action independently stopped.", EvidenceReference: "private-proof-handle"}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		calls++
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/attempts/"+attempt+"/settlement") || r.Header.Get("Idempotency-Key") != "settle-original" {
			t.Error("unexpected settlement transport")
		}
		var got client.SettleReviewAttemptRequest
		if json.NewDecoder(r.Body).Decode(&got) != nil || got != input {
			t.Error("original guard changed", got)
		}
		write(w, 200, client.ReviewAttemptSettlementReceipt{AttemptID: attempt, OperationID: executionOperationID, Revision: 8, State: "failed", Outcome: "settled"})
	}))
	defer server.Close()
	c := newClient(t, server)
	receipt, err := c.SettleReviewAttempt(t.Context(), "settle-original", attempt, input)
	if err != nil || receipt.Revision != 8 || calls != 1 {
		t.Fatal(receipt, err, calls)
	}
}

func TestOwnerCredentialConflictFiltersPrivateDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/instance" {
			handshake(w)
			return
		}
		write(w, 409, map[string]any{"request_id": "request-one", "error": map[string]any{"code": "credential_permissions_changed", "message": "PRIVATE narrative", "details": map[string]any{"credential_id": "credential-one", "current_permissions": []string{"read", "write"}, "private_evidence": "PRIVATE evidence"}}})
	}))
	defer server.Close()
	c := newClient(t, server)
	_, err := c.UpdateProjectCredential(t.Context(), "guarded-access", "credential-one", client.UpdateProjectCredentialRequest{ExpectedPermissions: []string{"read"}, Permissions: []string{"manage"}})
	var e *client.Error
	if !errors.As(err, &e) || e.Code != "credential_permissions_changed" || !strings.Contains(e.Action, "returned current_permissions") {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(e.Details)
	if strings.Contains(string(raw), "PRIVATE") || !strings.Contains(string(raw), "current_permissions") {
		t.Fatal(string(raw))
	}
}

func TestSelectedWorkerInboxOutboxReadKeepsReceiptAuthority(t *testing.T) {
	f := newMutationFixture(t)
	server := httptest.NewServer(f.handler)
	defer server.Close()
	c := f.newClient(t, f.admin, f.sender, server.URL)
	first, err := c.SendMessage(t.Context(), "first-selected-message", client.SendMessageRequest{RecipientWorkerID: f.recipient, Body: "First selected recipient message"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SendMessage(t.Context(), "second-selected-message", client.SendMessageRequest{RecipientWorkerID: f.recipient, Body: "Second selected recipient message"}); err != nil {
		t.Fatal(err)
	}
	page, err := c.Inbox(t.Context(), client.PageOptions{Limit: 1}, client.InboxFilter{}, f.recipient)
	if err != nil || len(page.Items) != 1 || page.Items[0].RecipientWorkerID != f.recipient || !page.HasMore || page.Cursor == "" {
		t.Fatal(page, err)
	}
	_, err = c.Inbox(t.Context(), client.PageOptions{Limit: 1, Cursor: page.Cursor}, client.InboxFilter{}, f.sender)
	var invalid *client.Error
	if !errors.As(err, &invalid) || invalid.Status != 400 {
		t.Fatal("target cursor was reusable", err)
	}
	f.call(t, "POST", "/api/v1/projects/"+f.project+"/messages", f.admin, f.recipient, map[string]any{"recipient_worker_id": f.sender, "body": "Selected worker outgoing message"})
	out, err := c.Outbox(t.Context(), client.PageOptions{}, client.InboxFilter{}, f.recipient)
	if err != nil || len(out.Items) != 1 || out.Items[0].SenderWorkerID != f.recipient {
		t.Fatal(out, err)
	}
	_, err = c.Acknowledge(t.Context(), "cannot-ack-selected-read", first.ID)
	checkError(t, err, "forbidden")
	delivery, err := c.MessageDelivery(t.Context(), first.ID)
	if err != nil || delivery.Seen || delivery.Acknowledged {
		t.Fatal("read impersonated recipient", delivery, err)
	}
	other := f.call(t, "POST", "/api/v1/projects", f.admin, "", map[string]any{"name": "foreign", "repository": "owner/foreign"})["id"].(string)
	foreign := f.call(t, "POST", "/api/v1/projects/"+other+"/workers", f.admin, "", map[string]any{"name": "Foreign"})["id"].(string)
	_, err = c.Inbox(t.Context(), client.PageOptions{}, client.InboxFilter{}, foreign)
	checkError(t, err, "not_found")
}
