package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const maxResponseBytes = 4 << 20
const requestTimeout = 5 * time.Second

// SourceRefreshBudget returns the total budget for an already validated source
// wait preference. Argument validation remains with the command or request.
func SourceRefreshBudget(waitSeconds int) time.Duration {
	if waitSeconds > 2 {
		return time.Duration(waitSeconds+5) * time.Second
	}
	return requestTimeout
}

type Client struct {
	identity              profile.Context
	base                  *url.URL
	token, credentialFile string
	http                  *http.Client
	transport             *http.Transport
}

func New(ctx context.Context, binding profile.Binding) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError(err)
	}
	base, err := url.Parse(binding.Context.ServerURL)
	identity := binding.Context
	if err != nil || !safeURL(base, binding.Context.ServerURL) || !cleanText(identity.Profile, 256) || !validID(identity.InstanceID) || !validID(identity.ProjectID) || !validID(identity.WorkerID) || binding.CredentialFile == "" {
		return nil, fail("binding_invalid", "The service profile must contain a safe URL and explicit bounded identities.", "Correct the selected private profile.", 0)
	}
	token, err := readCredential(binding.CredentialFile)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second, ExpectContinueTimeout: time.Second}
	transport.DialContext = (&net.Dialer{Timeout: requestTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = requestTimeout
	transport.TLSHandshakeTimeout = requestTimeout
	transport.MaxConnsPerHost = 4
	transport.MaxIdleConns = 4
	transport.DisableCompression = true
	c := &Client{identity: identity, base: base, token: token, credentialFile: binding.CredentialFile, transport: transport, http: &http.Client{Transport: transport, Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	var instance struct {
		InstanceID string `json:"instance_id"`
	}
	if err = c.get(ctx, []string{"api", "v1", "instance"}, nil, false, &instance); err != nil {
		c.Close()
		return nil, err
	}
	if instance.InstanceID != identity.InstanceID {
		c.Close()
		return nil, fail("instance_mismatch", "The service instance does not match the selected profile.", "Verify the service and explicitly update the private profile if its identity changed.", 409)
	}
	return c, nil
}

func (c *Client) Close()                    { c.transport.CloseIdleConnections() }
func (c *Client) Identity() profile.Context { return c.identity }

func fail(code, message, action string, status int) *Error {
	return &Error{Code: code, Message: message, Action: action, Status: status}
}
func invalidResponse() *Error {
	return fail("invalid_response", "The service returned an invalid or oversized JSON response.", "Check service compatibility and retry the read after correcting the response.", 0)
}
func validation() *Error {
	return fail("validation_failed", "The read requires valid bounded identities, filters and pagination options.", "Use explicit resource IDs and a limit from 1 to 100.", 400)
}
func contextError(err error) *Error {
	if errors.Is(err, context.Canceled) {
		return fail("canceled", "The service read was canceled.", "Retry the read when needed; no mutation was requested.", 0)
	}
	return fail("deadline_exceeded", "The service read deadline expired.", "Check the service connection and retry the read when needed.", 0)
}

func mutationContextError(err error, operation string, unknown bool) *Error {
	code := "deadline_exceeded"
	message := "The " + operation + " request deadline expired."
	if errors.Is(err, context.Canceled) {
		code = "canceled"
		message = "The " + operation + " request was canceled."
	}
	result := fail(code, message, mutationAction(), 0)
	result.OutcomeUnknown = unknown
	return result
}

func mutationAction() string {
	return "Check the service, then retry with the exact same idempotency key, request body and selected binding."
}

func unknownMutation(code, message string, status int) *Error {
	result := fail(code, message, mutationAction(), status)
	result.OutcomeUnknown = true
	return result
}

func unknownRequest(code, operation, reason string, status int) *Error {
	return unknownMutation(code, "The "+operation+" "+reason+"; its outcome is unknown.", status)
}

func requestOperation(method string, segments []string) string {
	if len(segments) < 5 {
		return "service mutation"
	}
	resource := segments[4:]
	if len(resource) == 3 && resource[0] == "jobs" && resource[2] == "checkpoints" && method == http.MethodPost {
		return "checkpoint append"
	}
	if len(resource) == 1 && resource[0] == "jobs" {
		return "job creation"
	}
	if len(resource) == 2 && resource[0] == "jobs" && method == http.MethodPatch {
		return "job edit"
	}
	if len(resource) == 1 && resource[0] == "messages" {
		return "message send"
	}
	if len(resource) == 3 && resource[0] == "messages" {
		if resource[2] == "seen" {
			return "message seen update"
		}
		if resource[2] == "ack" {
			return "message acknowledgment"
		}
	}
	return "service mutation"
}

func cleanText(value string, max int) bool {
	return value != "" && len(value) <= max && value == strings.TrimSpace(value) && utf8.ValidString(value)
}
func validID(value string) bool {
	if !cleanText(value, 128) || value == "." || value == ".." || strings.ContainsAny(value, "/\\%?#") {
		return false
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ValidID admits a bounded resource identity without normalization. Transports
// use the same guard before loading credentials or constructing a client.
func ValidID(value string) bool { return validID(value) }
func safeURL(u *url.URL, raw string) bool {
	if u == nil || !cleanText(raw, 4096) || strings.Contains(raw, "#") || u.Hostname() == "" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	return u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || net.ParseIP(u.Hostname()).IsLoopback())
}

func readCredential(path string) (string, error) {
	invalid := fail("credential_invalid", "The credential must be a private regular file containing one bounded bearer token.", "Use an owner-private credential file referenced by the selected profile.", 0)
	info, err := os.Lstat(path)
	if err != nil {
		return "", fail("credential_unreadable", "The referenced credential file is unavailable.", "Check the private credential reference and its read permissions.", 0)
	}
	if !privateFile(info) {
		return "", invalid
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fail("credential_unreadable", "The referenced credential file could not be opened.", "Check the private credential reference and its read permissions.", 0)
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !privateFile(info) {
		return "", invalid
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return "", invalid
	}
	token := strings.TrimSpace(string(data))
	if token == "" || len(token) > 256 {
		return "", invalid
	}
	for _, r := range token {
		if r < 33 || r > 126 {
			return "", invalid
		}
	}
	return token, nil
}
func privateFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Size() <= 4096 && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
}

func (c *Client) get(ctx context.Context, segments []string, query url.Values, scoped bool, dst any) error {
	return c.request(ctx, http.MethodGet, segments, query, scoped, "", nil, http.StatusOK, false, dst)
}

func (c *Client) request(ctx context.Context, method string, segments []string, query url.Values, scoped bool, key string, body []byte, expectedStatus int, mutation bool, dst any) error {
	return c.requestBounded(ctx, method, segments, query, scoped, key, body, expectedStatus, mutation, dst, maxResponseBytes, decodeResponse)
}

func decodeResponse(data []byte, dst any) error {
	if err := strictjson.Decode(data, dst); err != nil {
		return err
	}
	if !completeShape(data, dst) {
		return strictjson.ErrSyntax
	}
	return nil
}

func (c *Client) requestBounded(ctx context.Context, method string, segments []string, query url.Values, scoped bool, key string, body []byte, expectedStatus int, mutation bool, dst any, maxBytes int, decode func([]byte, any) error) error {
	return c.requestBoundedOptions(ctx, method, segments, query, scoped, key, body, expectedStatus, mutation, dst, maxBytes, decode, nil)
}

// sourceWaitOptions is shared by explicit source-refresh and checkpoint mutations. It
// preserves ordinary request budgets and never mutates shared transport state.
type sourceWaitOptions struct {
	seconds int
	status  *int
}

func (c *Client) requestBoundedOptions(ctx context.Context, method string, segments []string, query url.Values, scoped bool, key string, body []byte, expectedStatus int, mutation bool, dst any, maxBytes int, decode func([]byte, any) error, wait *sourceWaitOptions) error {
	operation := requestOperation(method, segments)
	if err := ctx.Err(); err != nil {
		if mutation {
			return mutationContextError(err, operation, false)
		}
		return contextError(err)
	}
	u := *c.base
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	for _, segment := range segments {
		escaped += "/" + url.PathEscape(segment)
	}
	u.Path, _ = url.PathUnescape(escaped)
	u.RawPath = escaped
	u.RawQuery = query.Encode()
	timeout := requestTimeout
	if wait != nil {
		timeout = SourceRefreshBudget(wait.seconds)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, u.String(), strings.NewReader(string(body)))
	if err != nil {
		return validation()
	}
	if mutation {
		// Go's transport can replay requests carrying Idempotency-Key when
		// GetBody is available. Only the caller may retry after an uncertain
		// outcome, so keep the body single-shot.
		r.GetBody = nil
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("X-FC-Instance", c.identity.InstanceID)
	if wait != nil {
		r.Header.Set("Prefer", "wait="+strconv.Itoa(wait.seconds))
	}
	if mutation {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
	}
	if scoped {
		r.Header.Set("X-FC-Worker", c.identity.WorkerID)
	}
	if err := ctx.Err(); err != nil {
		if mutation {
			return mutationContextError(err, operation, false)
		}
		return contextError(err)
	}
	httpClient := c.http
	if wait != nil {
		ownedClient := *c.http
		ownedClient.Timeout = timeout
		if wait.seconds > 2 {
			ownedTransport := c.transport.Clone()
			ownedTransport.ResponseHeaderTimeout = timeout
			defer ownedTransport.CloseIdleConnections()
			ownedClient.Transport = ownedTransport
		}
		httpClient = &ownedClient
	}
	response, err := httpClient.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			if mutation {
				return mutationContextError(ctx.Err(), operation, true)
			}
			return contextError(ctx.Err())
		}
		if mutation {
			return unknownRequest("connection_failed", operation, "request connection failed", 0)
		}
		return fail("connection_failed", "The service connection failed.", "Check the configured service address and transport.", 0)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		if mutation {
			return unknownRequest("redirect_rejected", operation, "request was redirected", response.StatusCode)
		}
		return fail("redirect_rejected", "The service attempted an unsupported redirect.", "Configure the final service address; credentials are not forwarded through redirects.", response.StatusCode)
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || response.Header.Get("Content-Encoding") != "" {
		if mutation {
			return unknownRequest("invalid_response", operation, "response was invalid", response.StatusCode)
		}
		return invalidResponse()
	}
	responseLimit := maxBytes
	success := response.StatusCode == expectedStatus || wait != nil && response.StatusCode == http.StatusOK
	if !success && responseLimit > 64<<10 {
		responseLimit = 64 << 10
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(responseLimit)+1))
	if err != nil {
		if ctx.Err() != nil {
			if mutation {
				return mutationContextError(ctx.Err(), operation, true)
			}
			return contextError(ctx.Err())
		}
		if mutation {
			return unknownRequest("invalid_response", operation, "response could not be read", response.StatusCode)
		}
		return invalidResponse()
	}
	if len(data) > responseLimit {
		if mutation {
			return unknownRequest("invalid_response", operation, "response was oversized", response.StatusCode)
		}
		return invalidResponse()
	}
	if !success {
		if mutation && response.StatusCode >= 400 && response.StatusCode < 600 {
			if refusal, valid := c.serviceError(data, response.StatusCode); valid {
				if wait != nil && sourceResponseOutcomeUnknown(data) {
					refusal.OutcomeUnknown = true
					refusal.Action = mutationAction()
				}
				if response.StatusCode >= 500 {
					refusal.Message = "The " + operation + " returned a service error; its outcome is unknown."
					refusal.Action = mutationAction()
					refusal.OutcomeUnknown = true
				}
				return refusal
			}
		}
		if mutation {
			return unknownRequest("unexpected_status", operation, "returned an unexpected status", response.StatusCode)
		}
		failure, _ := c.serviceError(data, response.StatusCode)
		return failure
	}
	if wait != nil {
		*wait.status = response.StatusCode
	}
	if decode(data, dst) != nil || c.hasSensitive(dst) {
		if mutation {
			return unknownRequest("invalid_response", operation, "receipt was invalid", response.StatusCode)
		}
		return invalidResponse()
	}
	return nil
}

type serviceFailure struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details any    `json:"details,omitempty"`
	} `json:"error"`
	RequestID string `json:"request_id"`
}

func (c *Client) serviceError(data []byte, status int) (*Error, bool) {
	var value serviceFailure
	if len(data) > 64<<10 || strictjson.Decode(data, &value) != nil || !completeShape(data, &value) || !safeCode(value.Error.Code) || len(value.Error.Message) > 512 || !validID(value.RequestID) {
		return invalidResponse(), false
	}
	details, err := json.Marshal(value.Error.Details)
	if err != nil || len(details) > 4096 || c.hasSensitive(&value) {
		return invalidResponse(), false
	}
	result := fail(value.Error.Code, "The service refused this scoped request.", "Check the selected instance, project, worker and current access.", status)
	result.RequestID = value.RequestID
	result.Details = c.safeDetails(value.Error.Details)
	if result.Code == "instance_mismatch" {
		result.Message = "The service instance does not match the selected profile."
		result.Action = "Verify the service and explicitly update the private profile if its identity changed."
	}
	return result, true
}

func safeCode(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for i, r := range value {
		if r >= 'a' && r <= 'z' || i > 0 && (r >= '0' && r <= '9' || r == '_') {
			continue
		}
		return false
	}
	return true
}
func (c *Client) sensitive(value string) bool {
	return strings.Contains(value, c.token) || strings.Contains(value, c.credentialFile)
}
func (c *Client) safeDetails(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]any)
	for key, item := range object {
		switch key {
		case "current_revision":
			if n, ok := item.(float64); ok && n >= 1 && n == float64(int64(n)) && n < 1<<53 {
				result[key] = int64(n)
			}
		case "project_id":
			if id, ok := item.(string); ok && validID(id) && !c.sensitive(id) {
				result[key] = id
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// Required DTO fields cannot silently become zero values. Optional cursor and
// reply fields carry omitempty tags; all JSON validation stays bounded upstream.
