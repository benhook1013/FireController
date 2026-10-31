package githubhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

var badSelectors = errors.New("invalid GitHub hint selectors")
var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

const maxPulls = 100

func knownEvent(event string) bool {
	switch event {
	case "ping", "pull_request", "check_run", "check_suite", "workflow_run", "status", "push":
		return true
	}
	return false
}
func normalize(event string, body []byte) (Hint, bool, error) {
	var hint Hint
	top, err := object(body)
	if err != nil {
		return hint, false, err
	}
	if event == "ping" {
		return hint, true, nil
	}
	if event == "pull_request" || event == "check_run" || event == "check_suite" || event == "workflow_run" {
		action, err := text(top, "action")
		if err != nil || action == "" || len(action) > 64 {
			return hint, false, badSelectors
		}
		if !knownAction(event, action) {
			return hint, true, nil
		}
	}
	repoRaw, err := field(top, "repository")
	if err != nil {
		return hint, false, err
	}
	repo, err := object(repoRaw)
	if err != nil {
		return hint, false, err
	}
	hint.RepositoryID, err = positive(repo, "id", 1<<63-1)
	if err != nil {
		return hint, false, err
	}
	hint.CI = true
	hint.Metadata = event == "pull_request" || event == "push"
	switch event {
	case "pull_request":
		raw, err := field(top, "pull_request")
		if err != nil {
			return hint, false, err
		}
		pull, err := object(raw)
		if err != nil {
			return hint, false, err
		}
		number, err := positive(top, "number", 1<<31-1)
		if err != nil {
			return hint, false, err
		}
		inner, err := positive(pull, "number", 1<<31-1)
		if err != nil || inner != number {
			return hint, false, badSelectors
		}
		raw, err = field(pull, "head")
		if err != nil {
			return hint, false, err
		}
		head, err := object(raw)
		if err != nil {
			return hint, false, err
		}
		hint.HeadSHA, err = sha(head, "sha")
		if err != nil {
			return hint, false, err
		}
		raw, err = field(pull, "base")
		if err != nil {
			return hint, false, err
		}
		base, err := object(raw)
		if err != nil {
			return hint, false, err
		}
		hint.BaseRef, err = text(base, "ref")
		if err != nil || !branch(hint.BaseRef) {
			return hint, false, badSelectors
		}
		hint.PullNumbers = []int64{number}
	case "check_run", "check_suite", "workflow_run":
		raw, err := field(top, event)
		if err != nil {
			return hint, false, err
		}
		check, err := object(raw)
		if err != nil {
			return hint, false, err
		}
		hint.HeadSHA, err = sha(check, "head_sha")
		if err != nil {
			return hint, false, err
		}
		raw, err = field(check, "pull_requests")
		if err != nil {
			return hint, false, err
		}
		if raw != nil {
			hint.PullNumbers, err = pulls(raw)
			if err != nil {
				return hint, false, err
			}
		}
	case "status":
		hint.HeadSHA, err = sha(top, "sha")
		if err != nil {
			return hint, false, err
		}
	case "push":
		ref, err := text(top, "ref")
		if err != nil || ref == "" {
			return hint, false, badSelectors
		}
		if !strings.HasPrefix(ref, "refs/heads/") {
			return hint, true, nil
		}
		hint.BranchRef = strings.TrimPrefix(ref, "refs/heads/")
		if !branch(hint.BranchRef) {
			return hint, false, badSelectors
		}
		raw, err := field(top, "deleted")
		if err != nil {
			return hint, false, err
		}
		deleted := false
		if raw != nil && (bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &deleted) != nil) {
			return hint, false, badSelectors
		}
		hint.HeadSHA, err = text(top, "after")
		if err != nil || !commitSHA.MatchString(hint.HeadSHA) {
			return hint, false, badSelectors
		}
		if deleted {
			if hint.HeadSHA != strings.Repeat("0", 40) {
				return hint, false, badSelectors
			}
			hint.HeadSHA = ""
		} else if hint.HeadSHA == strings.Repeat("0", 40) {
			return hint, false, badSelectors
		}
	}
	return hint, false, nil
}
func knownAction(event, action string) bool {
	switch event {
	case "pull_request":
		switch action {
		case "opened", "reopened", "closed", "synchronize", "edited", "ready_for_review", "converted_to_draft", "auto_merge_enabled", "auto_merge_disabled", "enqueued", "dequeued":
			return true
		}
	case "check_run":
		switch action {
		case "created", "completed", "rerequested", "requested_action":
			return true
		}
	case "check_suite":
		switch action {
		case "requested", "completed", "rerequested":
			return true
		}
	case "workflow_run":
		switch action {
		case "requested", "in_progress", "completed":
			return true
		}
	}
	return false
}
func object(raw []byte) (map[string]json.RawMessage, error) {
	var v map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v == nil {
		return nil, badSelectors
	}
	return v, nil
}
func field(v map[string]json.RawMessage, key string) (json.RawMessage, error) {
	for supplied := range v {
		if supplied != key && strings.EqualFold(supplied, key) {
			return nil, badSelectors
		}
	}
	return v[key], nil
}
func text(v map[string]json.RawMessage, key string) (string, error) {
	raw, err := field(v, key)
	if err != nil || raw == nil || bytes.Equal(raw, []byte("null")) {
		return "", badSelectors
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", badSelectors
	}
	return s, nil
}
func positive(v map[string]json.RawMessage, key string, max int64) (int64, error) {
	raw, err := field(v, key)
	var n int64
	if err != nil || raw == nil || json.Unmarshal(raw, &n) != nil || n < 1 || n > max {
		return 0, badSelectors
	}
	return n, nil
}
func sha(v map[string]json.RawMessage, key string) (string, error) {
	s, err := text(v, key)
	if err != nil || !commitSHA.MatchString(s) || s == strings.Repeat("0", 40) {
		return "", badSelectors
	}
	return s, nil
}
func branch(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && !strings.HasPrefix(s, "/") && !strings.HasSuffix(s, "/") && !strings.Contains(s, "//") && !strings.Contains(s, "..") && !strings.Contains(s, "@{") && strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("~^:?*[\\", r)
	}) < 0
}
func pulls(raw []byte) ([]int64, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('[') {
		return nil, badSelectors
	}
	out := []int64{}
	seen := map[int64]bool{}
	for count := 0; d.More(); count++ {
		if count >= maxPulls {
			return nil, badSelectors
		}
		var item map[string]json.RawMessage
		if d.Decode(&item) != nil || item == nil {
			return nil, badSelectors
		}
		n, err := positive(item, "number", 1<<31-1)
		if err != nil {
			return nil, err
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if _, err = d.Token(); err != nil {
		return nil, badSelectors
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
