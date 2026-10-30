package client

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

type ReviewPolicy struct {
	RequiredChannels []string `json:"required_channels"`
	MaxReviewFiles   int64    `json:"max_review_files"`
}
type ReviewPolicyRecord struct {
	ProjectID  string       `json:"project_id"`
	Revision   int64        `json:"revision"`
	ActorID    string       `json:"actor_id"`
	RecordedAt string       `json:"recorded_at"`
	Policy     ReviewPolicy `json:"policy"`
}
type ConfigureReviewPolicyRequest struct {
	ExpectedRevision int64    `json:"expected_revision"`
	RequiredChannels []string `json:"required_channels"`
	MaxReviewFiles   int64    `json:"max_review_files"`
}
type ConfigureReviewExecutionRequest struct {
	ExpectedRevision int64    `json:"expected_revision"`
	CapabilityID     string   `json:"capability_id"`
	Mode             string   `json:"mode"`
	Queue            []int64  `json:"queue"`
	Channels         []string `json:"channels"`
	Paused           bool     `json:"paused"`
}
type SettleReviewAttemptRequest struct {
	ExpectedRevision      int64  `json:"expected_revision"`
	ExpectedRepositoryID  int64  `json:"expected_repository_id"`
	ExpectedHeadSHA       string `json:"expected_head_sha"`
	ExpectedExternalID    string `json:"expected_external_id"`
	ExternalActionStopped bool   `json:"external_action_stopped"`
	Reason                string `json:"reason"`
	EvidenceReference     string `json:"evidence_reference"`
}
type ReviewAttemptSettlementReceipt struct {
	AttemptID   string `json:"attempt_id"`
	OperationID string `json:"operation_id"`
	Revision    int64  `json:"revision"`
	State       string `json:"state"`
	Outcome     string `json:"outcome"`
}
type CreateWorkerRequest struct {
	Name string `json:"name"`
}
type WorkerCreateReceipt struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Outcome  string `json:"outcome"`
}
type GrantProjectCredentialRequest struct {
	TokenSHA256 string   `json:"token_sha256"`
	Permissions []string `json:"permissions"`
}
type ProjectCredentialReceipt struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id,omitempty"`
	ProjectID   string `json:"project_id"`
	Outcome     string `json:"outcome"`
}

func ownerChannels(values []string) bool {
	if values == nil || len(values) < 1 || len(values) > 2 {
		return false
	}
	seen := map[string]bool{}
	for _, v := range values {
		if !oneReviewOf(v, "cli", "hosted") || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
func ownerText(s string, max int, multiline bool) bool {
	if !utf8.ValidString(s) || len(s) < 1 || len(s) > max || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			return false
		}
	}
	return true
}
func ValidateConfigureReviewPolicy(r ConfigureReviewPolicyRequest) error {
	if r.ExpectedRevision < 0 || r.ExpectedRevision == math.MaxInt64 || !ownerChannels(r.RequiredChannels) || r.MaxReviewFiles < 0 || r.MaxReviewFiles > 100000 {
		return mutationValidation()
	}
	return nil
}
func ValidateConfigureReviewExecution(r ConfigureReviewExecutionRequest) error {
	if r.ExpectedRevision < 0 || r.ExpectedRevision == math.MaxInt64 || !validExecutionIdentifier(r.CapabilityID) || !oneReviewOf(r.Mode, "independent", "stacked") || r.Queue == nil || len(r.Queue) > 100 || !ownerChannels(r.Channels) {
		return mutationValidation()
	}
	seen := map[int64]bool{}
	for _, n := range r.Queue {
		if !validPullNumber(n) || seen[n] {
			return mutationValidation()
		}
		seen[n] = true
	}
	return nil
}
func settlementText(s string, multiline bool) bool {
	if !utf8.ValidString(s) || len(s) > 1024 || strings.TrimSpace(s) == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t')) {
			return false
		}
	}
	return true
}
func ValidateSettleReviewAttempt(id string, r SettleReviewAttemptRequest) error {
	if !validReviewOpaqueID(id, "review_attempt_") || r.ExpectedRevision < 1 || r.ExpectedRevision == math.MaxInt64 || r.ExpectedRepositoryID < 1 || len(r.ExpectedHeadSHA) != 40 || !validReviewSHA(r.ExpectedHeadSHA) || r.ExpectedExternalID != "" && !validExecutionIdentifier(r.ExpectedExternalID) || !r.ExternalActionStopped || !settlementText(r.Reason, true) || !settlementText(r.EvidenceReference, false) {
		return mutationValidation()
	}
	return nil
}
func ValidateCreateWorker(r CreateWorkerRequest) error {
	if !ownerText(r.Name, 200, false) {
		return mutationValidation()
	}
	return nil
}
func ValidateGrantProjectCredential(r GrantProjectCredentialRequest) error {
	if len(r.TokenSHA256) != 64 || !lowerHex(r.TokenSHA256) || r.Permissions == nil || len(r.Permissions) < 1 || len(r.Permissions) > 3 {
		return mutationValidation()
	}
	seen := map[string]bool{}
	for _, p := range r.Permissions {
		if !oneReviewOf(p, "read", "write", "manage") || seen[p] {
			return mutationValidation()
		}
		seen[p] = true
	}
	if !seen["read"] && !seen["manage"] {
		return mutationValidation()
	}
	return nil
}
func lowerHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
func (c *Client) ReviewPolicy(ctx context.Context) (ReviewPolicyRecord, error) {
	var v ReviewPolicyRecord
	err := c.requestBounded(ctx, http.MethodGet, c.projectPath("reviews", "policy"), nil, true, "", nil, http.StatusOK, false, &v, 8<<10, decodeResponse)
	if err != nil {
		return v, err
	}
	if v.ProjectID != c.identity.ProjectID || v.Revision < 1 || !validID(v.ActorID) || !validJobProgressTime(v.RecordedAt) || !ownerChannels(v.Policy.RequiredChannels) || v.Policy.MaxReviewFiles < 0 || v.Policy.MaxReviewFiles > 100000 {
		return ReviewPolicyRecord{}, invalidResponse()
	}
	return v, nil
}
func (c *Client) ConfigureReviewPolicy(ctx context.Context, key string, r ConfigureReviewPolicyRequest) (ReviewRequestReceipt, error) {
	var v ReviewRequestReceipt
	if !validMutationKey(key) || ValidateConfigureReviewPolicy(r) != nil {
		return v, mutationValidation()
	}
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("reviews", "policy"), key, r, http.StatusOK, &v); err != nil {
		return v, err
	}
	if v.ID != c.identity.ProjectID || v.State != "policy_configured" || v.Revision != r.ExpectedRevision+1 {
		return ReviewRequestReceipt{}, unknownMutation("invalid_response", "The policy receipt was invalid; retain the original key and request.", 200)
	}
	return v, nil
}
func (c *Client) ConfigureReviewExecution(ctx context.Context, key string, r ConfigureReviewExecutionRequest) (ReviewRequestReceipt, error) {
	var v ReviewRequestReceipt
	if !validMutationKey(key) || ValidateConfigureReviewExecution(r) != nil {
		return v, mutationValidation()
	}
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("reviews", "execution", "configuration"), key, r, http.StatusCreated, &v); err != nil {
		return v, err
	}
	if v.ID != c.identity.ProjectID || v.State != "configured" || v.Revision != r.ExpectedRevision+1 {
		return ReviewRequestReceipt{}, unknownMutation("invalid_response", "The execution receipt was invalid; retain the original key and request.", 201)
	}
	return v, nil
}
func (c *Client) SettleReviewAttempt(ctx context.Context, key, id string, r SettleReviewAttemptRequest) (ReviewAttemptSettlementReceipt, error) {
	var v ReviewAttemptSettlementReceipt
	if !validMutationKey(key) || ValidateSettleReviewAttempt(id, r) != nil {
		return v, mutationValidation()
	}
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("reviews", "attempts", id, "settlement"), key, r, http.StatusOK, &v); err != nil {
		return v, err
	}
	if v.AttemptID != id || ValidateReviewOperationID(v.OperationID) != nil || v.Revision != r.ExpectedRevision+1 || v.State != "failed" || v.Outcome != "settled" {
		return ReviewAttemptSettlementReceipt{}, unknownMutation("invalid_response", "The settlement receipt was invalid; retain original attempt and request.", 200)
	}
	return v, nil
}
func (c *Client) CreateWorker(ctx context.Context, key string, r CreateWorkerRequest) (WorkerCreateReceipt, error) {
	var v WorkerCreateReceipt
	if !validMutationKey(key) || ValidateCreateWorker(r) != nil {
		return v, mutationValidation()
	}
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("workers"), key, r, http.StatusOK, &v); err != nil {
		return v, err
	}
	if !validID(v.ID) || v.Revision != 1 || v.Outcome != "created" {
		return WorkerCreateReceipt{}, unknownMutation("invalid_response", "The worker receipt was invalid; retain original name and key.", 200)
	}
	return v, nil
}
func (c *Client) GrantProjectCredential(ctx context.Context, key string, r GrantProjectCredentialRequest) (ProjectCredentialReceipt, error) {
	var v ProjectCredentialReceipt
	if !validMutationKey(key) || ValidateGrantProjectCredential(r) != nil {
		return v, mutationValidation()
	}
	if err := c.mutate(ctx, http.MethodPost, c.projectPath("credentials"), key, r, http.StatusOK, &v); err != nil {
		return v, err
	}
	if !validID(v.ID) || !validID(v.PrincipalID) || v.ProjectID != c.identity.ProjectID || v.Outcome != "granted" {
		return ProjectCredentialReceipt{}, unknownMutation("invalid_response", "The credential receipt was invalid; retain the digest, permissions and key.", 200)
	}
	return v, nil
}
func (c *Client) RevokeProjectCredential(ctx context.Context, key, id string) (ProjectCredentialReceipt, error) {
	var v ProjectCredentialReceipt
	if !validMutationKey(key) || !validID(id) {
		return v, mutationValidation()
	}
	if err := c.mutate(ctx, http.MethodDelete, c.projectPath("credentials", id), key, struct{}{}, http.StatusOK, &v); err != nil {
		return v, err
	}
	if v.ID != id || v.ProjectID != c.identity.ProjectID || v.PrincipalID != "" || v.Outcome != "revoked" {
		return ProjectCredentialReceipt{}, unknownMutation("invalid_response", "The revoke receipt was invalid; retain the credential ID and key.", 200)
	}
	return v, nil
}

type UpdateProjectCredentialRequest struct {
	ExpectedPermissions []string `json:"expected_permissions"`
	Permissions         []string `json:"permissions"`
}
type ProjectCredentialUpdateReceipt struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	Outcome     string   `json:"outcome"`
	Permissions []string `json:"permissions"`
}

func ValidateUpdateProjectCredential(id string, r UpdateProjectCredentialRequest) error {
	if !validID(id) || ValidateGrantProjectCredential(GrantProjectCredentialRequest{TokenSHA256: strings.Repeat("a", 64), Permissions: r.ExpectedPermissions}) != nil || ValidateGrantProjectCredential(GrantProjectCredentialRequest{TokenSHA256: strings.Repeat("a", 64), Permissions: r.Permissions}) != nil {
		return mutationValidation()
	}
	return nil
}
func normalizedPermissions(p []string) []string {
	read, write, manage := false, false, false
	for _, v := range p {
		switch v {
		case "read":
			read = true
		case "write":
			write = true
		case "manage":
			read, write, manage = true, true, true
		}
	}
	r := []string{}
	if read {
		r = append(r, "read")
	}
	if write {
		r = append(r, "write")
	}
	if manage {
		r = append(r, "manage")
	}
	return r
}
func (c *Client) UpdateProjectCredential(ctx context.Context, key, id string, r UpdateProjectCredentialRequest) (ProjectCredentialUpdateReceipt, error) {
	var v ProjectCredentialUpdateReceipt
	if !validMutationKey(key) || ValidateUpdateProjectCredential(id, r) != nil {
		return v, mutationValidation()
	}
	if err := c.mutate(ctx, http.MethodPut, c.projectPath("credentials", id), key, r, http.StatusOK, &v); err != nil {
		return v, projectCredentialError(err, id)
	}
	want := strings.Join(normalizedPermissions(r.Permissions), ",")
	if v.ID != id || v.ProjectID != c.identity.ProjectID || !oneReviewOf(v.Outcome, "updated", "unchanged") || strings.Join(v.Permissions, ",") != want {
		return ProjectCredentialUpdateReceipt{}, unknownMutation("invalid_response", "The access update receipt was invalid; preserve original credential ID, permissions and key.", 200)
	}
	return v, nil
}

// selectedWorker changes the resource selector, never the authenticated acting identity.
func (c *Client) selectedWorker(selected []string) (string, error) {
	if len(selected) > 1 {
		return "", validation()
	}
	worker := c.identity.WorkerID
	if len(selected) == 1 && selected[0] != "" {
		worker = selected[0]
	}
	if !validID(worker) {
		return "", validation()
	}
	return worker, nil
}

func safeCredentialPermissionDetails(object map[string]any) map[string]any {
	id, ok := object["credential_id"].(string)
	if !ok || !validID(id) {
		return nil
	}
	raw, ok := object["current_permissions"].([]any)
	if !ok {
		return nil
	}
	var permissions []string
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		permissions = append(permissions, s)
	}
	if ValidateGrantProjectCredential(GrantProjectCredentialRequest{TokenSHA256: strings.Repeat("a", 64), Permissions: permissions}) != nil || strings.Join(permissions, ",") != strings.Join(normalizedPermissions(permissions), ",") {
		return nil
	}
	return map[string]any{"credential_id": id, "current_permissions": permissions}
}

func projectCredentialError(err error, id string) error {
	var e *Error
	if !errors.As(err, &e) || e.Code != "credential_permissions_changed" || e.OutcomeUnknown {
		return err
	}
	details, ok := e.Details.(map[string]any)
	if !ok || details["credential_id"] != id {
		return fail("invalid_response", "The service returned an unverifiable permission conflict.", "Preserve the intended credential change and verify the selected service; no new key was generated.", 409)
	}
	e.Message = "The selected credential's project permissions changed."
	e.Action = "Compare returned current_permissions with the intended change. If still appropriate, use that set as expected_permissions with a new explicit key. This refused request changed no permissions."
	return e
}
