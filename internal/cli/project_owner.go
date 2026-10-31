package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/strictjson"
)

type ownerManagementInvocation struct {
	operation, key, id, file, credentialFile string
	policy                                   client.ConfigureReviewPolicyRequest
	execution                                client.ConfigureReviewExecutionRequest
	settlement                               client.SettleReviewAttemptRequest
	worker                                   client.CreateWorkerRequest
	grant                                    client.GrantProjectCredentialRequest
	update                                   client.UpdateProjectCredentialRequest
	control                                  client.ReviewOperationControl
}

func ownerManagementOperation(operation string) bool {
	switch operation {
	case "reviews operation-control", "reviews policy", "reviews policy-configure", "reviews execution-configure", "reviews attempt-settle", "workers create", "credentials grant", "credentials revoke", "credentials update":
		return true
	}
	return false
}
func parseOwnerManagement(operation string, args []string) (*ownerManagementInvocation, bool) {
	in := &ownerManagementInvocation{operation: operation}
	if operation == "reviews operation-control" {
		return parseOperationControl(in, args)
	}
	if operation == "reviews policy" {
		return in, len(args) == 0
	}
	positions := []string{}
	values := map[string]string{}
	var permissions []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positions = append(positions, args[i])
			continue
		}
		flag, value, assigned := strings.Cut(args[i], "=")
		switch flag {
		case "--file", "--key", "--name", "--token-sha256", "--credential-file", "--permission":
		default:
			return nil, false
		}
		if _, ok := values[flag]; ok && flag != "--permission" {
			return nil, false
		}
		if !assigned {
			i++
			if i >= len(args) {
				return nil, false
			}
			value = args[i]
		}
		if value == "" {
			return nil, false
		}
		values[flag] = value
		if flag == "--permission" {
			permissions = append(permissions, value)
		}
	}
	in.key, in.file, in.credentialFile = values["--key"], values["--file"], values["--credential-file"]
	if !validMutationKey(in.key) {
		return nil, false
	}
	allowed := map[string]bool{"--key": true}
	idRequired := operation == "reviews attempt-settle" || operation == "credentials revoke" || operation == "credentials update"
	if idRequired {
		if len(positions) != 1 || !validResourceID(positions[0]) {
			return nil, false
		}
		in.id = positions[0]
	} else if len(positions) != 0 {
		return nil, false
	}
	switch operation {
	case "reviews policy-configure", "reviews execution-configure", "reviews attempt-settle", "credentials update":
		allowed["--file"] = true
		if !filepath.IsAbs(in.file) {
			return nil, false
		}
	case "workers create":
		allowed["--name"] = true
		in.worker.Name = values["--name"]
		if client.ValidateCreateWorker(in.worker) != nil {
			return nil, false
		}
	case "credentials grant":
		allowed["--token-sha256"], allowed["--credential-file"], allowed["--permission"] = true, true, true
		in.grant = client.GrantProjectCredentialRequest{TokenSHA256: values["--token-sha256"], Permissions: permissions}
		if (in.credentialFile == "") == (in.grant.TokenSHA256 == "") {
			return nil, false
		}
		if in.credentialFile != "" {
			if !filepath.IsAbs(in.credentialFile) {
				return nil, false
			}
			in.grant.TokenSHA256 = strings.Repeat("a", 64)
		}
		if client.ValidateGrantProjectCredential(in.grant) != nil {
			return nil, false
		}
	case "credentials revoke":
	default:
		return nil, false
	}
	for flag := range values {
		if !allowed[flag] {
			return nil, false
		}
	}
	return in, true
}
func decodeOwnerJSON(body []byte, out any, required ...string) bool {
	if strictjson.Decode(body, out) != nil {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return false
	}
	for _, key := range required {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			return false
		}
	}
	return true
}
func prepareOwnerManagement(ctx context.Context, in *ownerManagementInvocation) error {
	bad := func() error {
		return &client.Error{Code: "invalid_management_input", Message: "Use the declared complete management fields in a bounded owner-private input file.", Action: "Check local help and correct the selected file; no service request was sent.", Status: 400}
	}
	if in.credentialFile != "" {
		token, ok := readPrivateTextFile(in.credentialFile, 4096)
		if !ok {
			return bad()
		}
		token = strings.TrimSpace(token)
		if len(token) < 1 {
			return bad()
		}
		for _, r := range token {
			if r < 33 || r > 126 {
				return bad()
			}
		}
		sum := sha256.Sum256([]byte(token))
		in.grant.TokenSHA256 = hex.EncodeToString(sum[:])
	}
	if in.file == "" {
		return nil
	}
	body, err := readOwnerPrivateJSON(ctx, in.file)
	if err != nil {
		return err
	}
	switch in.operation {
	case "reviews policy-configure":
		if !decodeOwnerJSON(body, &in.policy, "expected_revision", "required_channels", "max_review_files") || client.ValidateConfigureReviewPolicy(in.policy) != nil {
			return bad()
		}
	case "reviews execution-configure":
		if !decodeOwnerJSON(body, &in.execution, "expected_revision", "capability_id", "mode", "queue", "channels", "paused") || client.ValidateConfigureReviewExecution(in.execution) != nil {
			return bad()
		}
	case "reviews attempt-settle":
		if !decodeOwnerJSON(body, &in.settlement, "expected_revision", "expected_repository_id", "expected_head_sha", "expected_external_id", "external_action_stopped", "reason", "evidence_reference") || client.ValidateSettleReviewAttempt(in.id, in.settlement) != nil {
			return bad()
		}
	case "credentials update":
		if !decodeOwnerJSON(body, &in.update, "expected_permissions", "permissions") || client.ValidateUpdateProjectCredential(in.id, in.update) != nil {
			return bad()
		}
	}
	return nil
}
func executeOwnerManagement(ctx, parent context.Context, c *client.Client, in *ownerManagementInvocation, stdout, stderr io.Writer) int {
	var v any
	var err error
	switch in.operation {
	case "reviews operation-control":
		v, err = c.ControlReviewOperation(ctx, in.key, in.id, in.control)
	case "reviews policy":
		v, err = c.ReviewPolicy(ctx)
	case "reviews policy-configure":
		v, err = c.ConfigureReviewPolicy(ctx, in.key, in.policy)
	case "reviews execution-configure":
		v, err = c.ConfigureReviewExecution(ctx, in.key, in.execution)
	case "reviews attempt-settle":
		v, err = c.SettleReviewAttempt(ctx, in.key, in.id, in.settlement)
	case "workers create":
		v, err = c.CreateWorker(ctx, in.key, in.worker)
	case "credentials grant":
		v, err = c.GrantProjectCredential(ctx, in.key, in.grant)
	case "credentials revoke":
		v, err = c.RevokeProjectCredential(ctx, in.key, in.id)
	case "credentials update":
		v, err = c.UpdateProjectCredential(ctx, in.key, in.id, in.update)
	}
	if err != nil {
		return reportClientError(stderr, err, parent)
	}
	if in.operation == "reviews policy" {
		return emit(stdout, stderr, v)
	}
	b, err := json.Marshal(v)
	if err == nil {
		b = append(b, '\n')
		var n int
		n, err = stdout.Write(b)
		if n != len(b) && err == nil {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return reportDetails(stderr, ExitConfig, diagnostic{Code: "output_failed", Message: "The management receipt could not be written.", Action: "Retain the original exact arguments, selected project and idempotency key for recovery.", OutcomeUnknown: true})
	}
	return ExitSuccess
}

func parseOperationControl(in *ownerManagementInvocation, args []string) (*ownerManagementInvocation, bool) {
	if len(args) < 1 {
		return nil, false
	}
	in.id = args[0]
	values, ok := reviewSourceFlags(args[1:], "--action", "--expect-revision", "--key")
	if !ok {
		return nil, false
	}
	in.key = values["--key"]
	in.control.Action = values["--action"]
	n, err := strconv.ParseInt(values["--expect-revision"], 10, 64)
	in.control.ExpectedRevision = n
	return in, err == nil && strconv.FormatInt(n, 10) == values["--expect-revision"] && validMutationKey(in.key) && client.ValidateReviewOperationControl(in.id, in.control) == nil
}
