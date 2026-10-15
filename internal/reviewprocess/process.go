// Package reviewprocess runs the configured agent CLI against a verified,
// immutable workspace. It never admits work or invokes a provider itself.
package reviewprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/reviewworkspace"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const usageTimeout = 15 * time.Second
const reviewTimeout = 20 * time.Minute
const permitLifetime = 60 * time.Second
const maxUsageOutput = 64 << 10
const maxReviewOutput = 16 << 20
const maxErrorOutput = 64 << 10

// Error contains only a stable public classification. It never includes a
// filesystem path, provider output, executable stderr or underlying error.
type Error struct {
	Code           string
	OutcomeUnknown bool
}

func (e *Error) Error() string {
	switch e.Code {
	case "configuration_invalid":
		return "Review process configuration is invalid."
	case "usage_unavailable":
		return "Included review allowance could not be verified."
	case "rate_limited":
		return "No included review allowance is currently available."
	case "source_changed":
		return "The verified review workspace changed during execution."
	case "permit_required":
		return "Verify included allowance for this workspace before reviewing."
	case "process_failed":
		return "The review process could not be completed safely."
	default:
		return "The review process is unavailable."
	}
}

type Usage struct {
	Remaining        int64
	Limit            int64
	FullCapacityInMS int64
	RollingWindowMS  int64
}

type Capture struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Started  bool
}

type Runner struct {
	manager         *reviewworkspace.Manager
	executable      string
	env             []string
	mu              sync.Mutex
	permit          *usagePermit
	usageGeneration uint64
	now             func() time.Time
}

type usagePermit struct {
	workspace workspaceKey
	issued    time.Time
}

type workspaceKey struct {
	directory, comparison, candidate, base, mergeBase, patch, repositoryCanonical string
}

// New resolves the executable once and builds a closed child environment.
// authenticationEnvironment is operator-owned and may contain only private
// home/config directory paths; it is never taken from a worker request.
func New(manager *reviewworkspace.Manager, executable string, authenticationEnvironment map[string]string) (*Runner, error) {
	if !processGroupsSupported || manager == nil || executable == "" || strings.IndexByte(executable, 0) >= 0 {
		return nil, processError("configuration_invalid", false)
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return nil, processError("configuration_invalid", false)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return nil, processError("configuration_invalid", false)
	}
	allowed := map[string]bool{"HOME": true, "USERPROFILE": true, "APPDATA": true, "LOCALAPPDATA": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_CACHE_HOME": true}
	env := make([]string, 0, 12+len(authenticationEnvironment))
	for _, key := range []string{"PATH", "SystemRoot", "TMP", "TEMP", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range authenticationEnvironment {
		if !allowed[key] || value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 || !filepath.IsAbs(value) {
			return nil, processError("configuration_invalid", false)
		}
		env = append(env, key+"="+value)
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "LANG=C.UTF-8", "NO_COLOR=1")
	return &Runner{manager: manager, executable: resolved, env: env, now: time.Now}, nil
}

func processError(code string, unknown bool) error {
	return &Error{Code: code, OutcomeUnknown: unknown}
}

// Usage executes exactly one agent-mode allowance query against the pinned
// workspace and expected account/repository. Every attempt replaces any old
// one-use permit, including failed refreshes.
func (r *Runner) Usage(ctx context.Context, workspace reviewworkspace.Workspace, expectedRepository, expectedOrganization string) (Usage, error) {
	r.mu.Lock()
	r.usageGeneration++
	generation := r.usageGeneration
	r.permit = nil
	r.mu.Unlock()
	if ctx == nil || ctx.Err() != nil || !validRepository(expectedRepository) || !validSelector(expectedOrganization) || workspace.RepositoryCanonical != expectedRepository {
		return Usage{}, processError("usage_unavailable", false)
	}
	if err := r.manager.Verify(ctx, workspace); err != nil {
		if ctx.Err() != nil {
			return Usage{}, processError("usage_unavailable", false)
		}
		return Usage{}, processError("source_changed", false)
	}
	commandCtx, cancel := context.WithTimeout(ctx, usageTimeout)
	defer cancel()
	if commandCtx.Err() != nil {
		return Usage{}, processError("usage_unavailable", false)
	}
	result := r.run(commandCtx, workspace.Directory, []string{"usage", "--agent"}, maxUsageOutput, maxErrorOutput)
	if result.err != nil || result.capture.ExitCode != 0 || !result.capture.Started {
		return Usage{}, processError("usage_unavailable", false)
	}
	usage, err := parseUsage(result.capture.Stdout, expectedRepository, expectedOrganization)
	if err != nil {
		return Usage{}, err
	}
	if usage.Remaining == 0 {
		return Usage{}, processError("rate_limited", false)
	}
	if err := r.manager.Verify(ctx, workspace); err != nil {
		if ctx.Err() != nil {
			return Usage{}, processError("usage_unavailable", false)
		}
		return Usage{}, processError("source_changed", false)
	}
	r.mu.Lock()
	if generation != r.usageGeneration || ctx.Err() != nil {
		r.mu.Unlock()
		return Usage{}, processError("usage_unavailable", false)
	}
	r.permit = &usagePermit{workspace: keyFor(workspace), issued: r.now()}
	r.mu.Unlock()
	return usage, nil
}

// Review consumes the exact fresh usage permit before starting one process.
// A returned capture is raw evidence for a caller-owned private journal; this
// package does not classify success from exit status or inspect provider data.
func (r *Runner) Review(ctx context.Context, workspace reviewworkspace.Workspace) (Capture, error) {
	if ctx == nil || ctx.Err() != nil {
		return Capture{}, processError("process_failed", false)
	}
	if err := r.manager.Verify(ctx, workspace); err != nil {
		if ctx.Err() != nil {
			return Capture{}, processError("process_failed", false)
		}
		return Capture{}, processError("source_changed", false)
	}
	key := keyFor(workspace)
	r.mu.Lock()
	permit := r.permit
	if permit == nil || permit.workspace != key || r.now().Before(permit.issued) || r.now().Sub(permit.issued) > permitLifetime {
		r.permit = nil
		r.mu.Unlock()
		return Capture{}, processError("permit_required", false)
	}
	r.permit = nil
	r.mu.Unlock()
	commandCtx, cancel := context.WithTimeout(ctx, reviewTimeout)
	defer cancel()
	if commandCtx.Err() != nil {
		return Capture{}, processError("process_failed", false)
	}
	result := r.run(commandCtx, workspace.Directory, []string{"review", "--agent", "--committed", "--base", workspace.ComparisonRef}, maxReviewOutput, maxErrorOutput)
	if err := r.manager.Verify(ctx, workspace); err != nil {
		if ctx.Err() != nil {
			return result.capture, processError("process_failed", result.capture.Started)
		}
		return result.capture, processError("source_changed", result.capture.Started)
	}
	if result.err != nil {
		return result.capture, processError("process_failed", result.capture.Started)
	}
	return result.capture, nil
}

type runResult struct {
	capture Capture
	err     error
}

func (r *Runner) run(ctx context.Context, directory string, args []string, stdoutLimit, stderrLimit int) runResult {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(commandCtx, r.executable, args...)
	command.Dir = directory
	command.Env = append([]string(nil), r.env...)
	command.WaitDelay = time.Second
	cleanup, err := configureOwnedProcess(command)
	if err != nil {
		return runResult{capture: Capture{ExitCode: -1}, err: err}
	}
	out := &limitedBuffer{limit: stdoutLimit, cancel: cancel}
	stderr := &limitedBuffer{limit: stderrLimit, cancel: cancel}
	command.Stdout = out
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return runResult{capture: Capture{Stdout: out.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}, err: err}
	}
	started := true
	err = command.Wait()
	cleanupErr := cleanup()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else {
			code = -1
		}
	}
	capture := Capture{Stdout: out.Bytes(), Stderr: stderr.Bytes(), ExitCode: code, Started: started}
	if cleanupErr != nil && !errors.Is(cleanupErr, os.ErrProcessDone) || out.overflow || stderr.overflow || commandCtx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) || err != nil && code < 0 {
		return runResult{capture: capture, err: errOutputOrProcess}
	}
	return runResult{capture: capture}
}

var errOutputOrProcess = errors.New("bounded child process failed")

type limitedBuffer struct {
	data     bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.data.Len()
	if len(p) > remaining {
		b.overflow = true
		b.cancel()
		if remaining > 0 {
			if _, err := b.data.Write(p[:remaining]); err != nil {
				return 0, err
			}
		}
		return remaining, errOutputOrProcess
	}
	return b.data.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.data.Bytes() }

func keyFor(w reviewworkspace.Workspace) workspaceKey {
	return workspaceKey{w.Directory, w.ComparisonRef, w.CandidateSHA, w.BaseSHA, w.MergeBaseSHA, w.PatchSHA256, w.RepositoryCanonical}
}

func validSelector(s string) bool {
	return s != "" && len(s) <= 100 && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\r\n\x00")
}
func validRepository(s string) bool {
	if !validSelector(s) {
		return false
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 100 || part == "." || part == ".." || strings.ContainsAny(part, "\\:%?#") {
			return false
		}
		for _, ch := range part {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
				return false
			}
		}
	}
	return true
}

func parseUsage(data []byte, expectedRepository, expectedOrganization string) (Usage, error) {
	var zero Usage
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || len(trimmed) > maxUsageOutput || strictjson.Validate(trimmed) != nil {
		return zero, processError("usage_unavailable", false)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(trimmed, &root) != nil || root == nil || hasAliases(root, "type", "phase", "status", "organization", "includedReviews", "billingPeriod") {
		return zero, processError("usage_unavailable", false)
	}
	var event struct {
		Type         string `json:"type"`
		Phase        string `json:"phase"`
		Status       string `json:"status"`
		Organization string `json:"organization"`
	}
	if json.Unmarshal(trimmed, &event) != nil || event.Type != "status" || event.Phase != "usage" || event.Status != "complete" || event.Organization != expectedOrganization {
		return zero, processError("usage_unavailable", false)
	}
	var billing map[string]json.RawMessage
	if json.Unmarshal(root["billingPeriod"], &billing) != nil || billing == nil || hasAliases(billing, "state", "usageBillingStatus") {
		return zero, processError("usage_unavailable", false)
	}
	var billingState, billingStatus string
	if decodeExact(billing, "state", &billingState) != nil || decodeExact(billing, "usageBillingStatus", &billingStatus) != nil || billingState != "available" || billingStatus != "inactive" {
		return zero, processError("usage_unavailable", false)
	}
	var raw json.RawMessage
	if raw = root["includedReviews"]; len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return zero, processError("usage_unavailable", false)
	}
	var included map[string]json.RawMessage
	if json.Unmarshal(raw, &included) != nil || included == nil || hasAliases(included, "state", "repository", "remaining", "limit", "fullCapacityInMs", "rollingWindowMs") {
		return zero, processError("usage_unavailable", false)
	}
	var state, repo string
	var u Usage
	if decodeExact(included, "state", &state) != nil || decodeExact(included, "repository", &repo) != nil || decodeExact(included, "remaining", &u.Remaining) != nil || decodeExact(included, "limit", &u.Limit) != nil || decodeExact(included, "fullCapacityInMs", &u.FullCapacityInMS) != nil || decodeExact(included, "rollingWindowMs", &u.RollingWindowMS) != nil || state != "available" || repo != expectedRepository || u.Remaining < 0 || u.Limit < 1 || u.Limit > 10000 || u.Remaining > u.Limit || u.FullCapacityInMS < 0 || u.FullCapacityInMS > 86400000 || u.RollingWindowMS < 1 || u.RollingWindowMS > 86400000 {
		return zero, processError("usage_unavailable", false)
	}
	return u, nil
}

func hasAliases(fields map[string]json.RawMessage, names ...string) bool {
	for key := range fields {
		for _, name := range names {
			if strings.EqualFold(key, name) && key != name {
				return true
			}
		}
	}
	return false
}
func decodeExact(fields map[string]json.RawMessage, key string, dst any) error {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("missing")
	}
	return json.Unmarshal(raw, dst)
}
