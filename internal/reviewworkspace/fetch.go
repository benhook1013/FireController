package reviewworkspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const fetchBudget = 60 * time.Second
const fetchOutputLimit = 64 << 10

// Fetch provisions or refreshes this manager's private bare cache from one
// fixed GitHub repository URL. It fetches only the two caller-pinned commits;
// no remote refs, credentials, or provider metadata are retained.
func (m *Manager) Fetch(ctx context.Context, repositoryID int64, canonicalRepository, candidateSHA, baseSHA, token string) (result error) {
	if m == nil || ctx == nil || repositoryID <= 0 || !validCanonicalRepository(canonicalRepository) || !validSHA(candidateSHA) || !validSHA(baseSHA) || !validGitHubToken(token) {
		return errInput
	}
	cache, err := m.CachePath(repositoryID)
	if err != nil {
		return errInput
	}
	if m.fetchGate == nil {
		return errConfiguration
	}
	operationCtx, cancel := context.WithTimeout(ctx, fetchBudget)
	defer cancel()
	defer func() {
		if result != nil && operationCtx.Err() != nil {
			result = errors.Join(result, operationCtx.Err())
		}
	}()
	select {
	case m.fetchGate <- struct{}{}:
		defer func() { <-m.fetchGate }()
	case <-operationCtx.Done():
		return errMaterialize
	}
	if operationCtx.Err() != nil || m.layout() != nil {
		return errMaterialize
	}

	_, statErr := os.Lstat(cache)
	if errors.Is(statErr, os.ErrNotExist) {
		if err = os.Mkdir(cache, 0o700); err != nil {
			return errMaterialize
		}
	} else if statErr != nil {
		return errConfiguration
	}
	if privateDirectory(cache, false) != nil {
		return errConfiguration
	}
	directory, err := os.Open(cache)
	if err != nil {
		return errMaterialize
	}
	entries, readErr := directory.ReadDir(1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil {
		return errMaterialize
	}
	// A failed initialization may leave only our empty private directory. Retry
	// that case without deleting anything or reinitializing nonempty state.
	if len(entries) == 0 {
		if _, err = m.fetchOutput(operationCtx, m.root, token, "init", "--bare", "--template=", "--", cache); err != nil {
			return errMaterialize
		}
		if privateDirectory(cache, false) != nil {
			return errConfiguration
		}
	}
	gitDir := "--git-dir=" + cache
	bare, verifyErr := m.fetchOutput(operationCtx, cache, token, gitDir, "rev-parse", "--is-bare-repository")
	if verifyErr != nil || bare != "true" {
		return errMaterialize
	}

	if !m.completeFetchObjects(operationCtx, cache, gitDir, candidateSHA, baseSHA) {
		remote := "https://github.com/" + canonicalRepository + ".git"
		if _, err = m.fetchOutput(operationCtx, cache, token, gitDir, "fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", remote, candidateSHA, baseSHA); err != nil {
			return errMaterialize
		}
		if !m.completeFetchObjects(operationCtx, cache, gitDir, candidateSHA, baseSHA) {
			return errMaterialize
		}
	}
	// Fixed private refs let a genuinely new pinned fetch negotiate retained
	// ancestry. They never select source, track a remote branch or store a token.
	for _, pin := range []struct{ ref, sha string }{{"refs/firecontroller/cache/candidate", candidateSHA}, {"refs/firecontroller/cache/base", baseSHA}} {
		if _, err := m.fetchOutput(operationCtx, cache, "", gitDir, "update-ref", pin.ref, pin.sha); err != nil {
			return errMaterialize
		}
	}
	return nil
}

func (m *Manager) completeFetchObjects(ctx context.Context, cache, gitDir, candidateSHA, baseSHA string) bool {
	shallow, err := m.fetchOutput(ctx, cache, "", gitDir, "rev-parse", "--is-shallow-repository")
	if err != nil || shallow != "false" {
		return false
	}
	for _, sha := range []string{candidateSHA, baseSHA} {
		commit, err := m.fetchOutput(ctx, cache, "", gitDir, "rev-parse", "--verify", sha+"^{commit}")
		if err != nil || commit != sha {
			return false
		}
	}
	// Quiet suppresses object/path output, but --missing=error still verifies
	// reachable blobs as well as commits/trees. No implicit partial-clone fetch.
	_, err = m.fetchOutput(ctx, cache, "", gitDir, "rev-list", "--objects", "--missing=error", "--quiet", candidateSHA, baseSHA)
	return err == nil
}

func validCanonicalRepository(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if len(part) < 1 || len(part) > 100 || strings.Trim(part, ".") == "" {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
				return false
			}
		}
	}
	return true
}

func validGitHubToken(token string) bool {
	if len(token) > 256 || !utf8.ValidString(token) {
		return false
	}
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (m *Manager) fetchOutput(ctx context.Context, directory, token string, args ...string) (string, error) {
	commandCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	fixed := []string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "core.autocrlf=false", "-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "credential.helper=", "-c", "http.followRedirects=false"}
	command := exec.CommandContext(commandCtx, m.git, append(fixed, args...)...)
	command.Dir = directory
	command.Env = append(append([]string(nil), m.env...), "GIT_NO_LAZY_FETCH=1")
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		command.Env = append(command.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+basic)
	}
	command.WaitDelay = time.Second
	cleanup := configureFetchProcess(command)
	defer cleanup()
	var stdout bytes.Buffer
	command.Stdout = &boundedWriter{writer: &stdout, remaining: fetchOutputLimit, cancel: cancel}
	command.Stderr = &boundedWriter{writer: ioDiscard{}, remaining: fetchOutputLimit, cancel: cancel}
	if err := command.Run(); err != nil || commandCtx.Err() != nil {
		return "", errMaterialize
	}
	if !utf8.Valid(stdout.Bytes()) {
		return "", errMaterialize
	}
	return strings.TrimSuffix(stdout.String(), "\n"), nil
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
