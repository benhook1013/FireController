package reviewprovider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const authorityClassic = `{"required_status_checks":{"strict":false,"contexts":["build","build"],"checks":[{"context":"build","app_id":17}]}}`
const authorityRule = `{"type":"required_status_checks","ruleset_id":42,"ruleset_source_type":"Organization","ruleset_source":"example-org","parameters":{"required_status_checks":[{"context":"build","integration_id":17},{"context":"lint"}],"strict_required_status_checks_policy":true}}`

func authorityServer(t *testing.T, classic, rules string) (*GitHub, *atomic.Int64) {
	t.Helper()
	calls := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer synthetic-authority-token" {
			t.Error("authority read changed method/credential")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo/branches/main/protection":
			_, _ = ioWriteString(w, classic)
		case "/repos/owner/repo/rules/branches/main":
			_, _ = ioWriteString(w, rules)
		default:
			t.Error("authority read unrelated scope", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return testGitHub(server, "synthetic-authority-token"), calls
}
func TestRequiredCheckAuthorityClassicAndActiveParentRuleUnion(t *testing.T) {
	g, calls := authorityServer(t, authorityClassic, "["+authorityRule+`,{"type":"required_status_checks","ruleset_id":43,"ruleset_source_type":"Repository","ruleset_source":"owner/repo","parameters":{"required_status_checks":[{"context":"lint","integration_id":null},{"context":"unit","integration_id":23},{"context":"any","integration_id":null}],"strict_required_status_checks_policy":false}},{"type":"pull_request","ruleset_id":43,"parameters":{"required_approving_review_count":2}},{"type":"commit_message_pattern","parameters":{"operator":"starts_with","pattern":"TASK"}}]`)
	started := time.Now()
	result, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
	want := []RequiredCheck{{Context: "any"}, {Context: "build", AppID: 17}, {Context: "lint"}, {Context: "unit", AppID: 23}}
	if err != nil || result.State != "complete" || result.Reason != "" || result.Repository != "owner/repo" || result.Branch != "main" || !result.Strict || !reflect.DeepEqual(result.Checks, want) || result.ObservedAt.Before(started) || result.ObservedAt.After(time.Now()) || result.ObservedAt.Location() != time.UTC || calls.Load() != 2 {
		t.Fatal("authority union/identity/origin/time lost", result, err, calls.Load())
	}
}
func TestRequiredCheckAuthorityExplicitAbsenceAndAnyOrigin(t *testing.T) {
	for _, tc := range []struct {
		classic, rules string
		want           []RequiredCheck
	}{
		{`{"required_status_checks":null}`, `[]`, []RequiredCheck{}},
		{`{"required_status_checks":{"strict":false,"contexts":[],"checks":[]}}`, `[]`, []RequiredCheck{}},
		{`{"required_status_checks":{"strict":true,"contexts":["build"],"checks":[{"context":"build","app_id":-1},{"context":"build","app_id":-1}]}}`, `[]`, []RequiredCheck{{Context: "build"}}},
		{`{"required_status_checks":null}`, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":" exact spelling "}],"strict_required_status_checks_policy":false}}]`, []RequiredCheck{{Context: " exact spelling "}}},
	} {
		t.Run(fmt.Sprintf("%d-%d", len(tc.classic), len(tc.rules)), func(t *testing.T) {
			g, _ := authorityServer(t, tc.classic, tc.rules)
			result, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if err != nil || result.State != "complete" || result.Checks == nil || !reflect.DeepEqual(result.Checks, tc.want) {
				t.Fatal("proven absence/origin/spelling lost", result, err)
			}
		})
	}
}
func TestRequiredCheckAuthorityMissingOriginsConflictsAndUnsupportedRulesHold(t *testing.T) {
	cases := []struct{ name, classic, rules, reason string }{
		{"legacy context", `{"required_status_checks":{"strict":false,"contexts":["build"]}}`, `[]`, "check_app_identity_unproven"},
		{"legacy unmatched context", strings.Replace(authorityClassic, `"build","build"`, `"build","legacy"`, 1), `[]`, "check_app_identity_unproven"},
		{"classic missing app", strings.Replace(authorityClassic, `,"app_id":17`, "", 1), `[]`, "check_app_identity_unproven"},
		{"classic null app", strings.Replace(authorityClassic, `"app_id":17`, `"app_id":null`, 1), `[]`, "check_app_identity_unproven"},
		{"classic zero app", strings.Replace(authorityClassic, `"app_id":17`, `"app_id":0`, 1), `[]`, "check_app_identity_unproven"},
		{"classic negative app", strings.Replace(authorityClassic, `"app_id":17`, `"app_id":-2`, 1), `[]`, "check_app_identity_unproven"},
		{"conflicting positive apps", authorityClassic, `[` + strings.Replace(authorityRule, `"integration_id":17`, `"integration_id":18`, 1) + `]`, "conflicting_app_requirements"},
		{"rule zero origin", authorityClassic, `[` + strings.Replace(authorityRule, `"integration_id":17`, `"integration_id":0`, 1) + `]`, "check_app_identity_unproven"},
		{"rule negative origin", authorityClassic, `[` + strings.Replace(authorityRule, `"integration_id":17`, `"integration_id":-1`, 1) + `]`, "check_app_identity_unproven"},
		{"null parameters", authorityClassic, `[{"type":"required_status_checks","parameters":null}]`, "required_checks_malformed"},
		{"missing parameters", authorityClassic, `[{"type":"required_status_checks"}]`, "required_checks_malformed"},
		{"missing strict", authorityClassic, `[{"type":"required_status_checks","parameters":{"required_status_checks":[]}}]`, "required_checks_malformed"},
		{"null strict", authorityClassic, `[{"type":"required_status_checks","parameters":{"required_status_checks":[],"strict_required_status_checks_policy":null}}]`, "required_checks_malformed"},
		{"missing checks", authorityClassic, `[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":false}}]`, "required_checks_malformed"},
		{"contradictory rule identity", authorityClassic, `[` + authorityRule + `,` + strings.Replace(authorityRule, `"strict_required_status_checks_policy":true`, `"strict_required_status_checks_policy":false`, 1) + `]`, "contradictory_rule_identity"},
	}
	for _, kind := range []string{"workflows", "code_scanning", "merge_queue", "required_deployments", "code_quality", "code_coverage", "license_compliance_scanning", "future_check_gate"} {
		cases = append(cases, struct{ name, classic, rules, reason string }{kind, authorityClassic, `[{"type":"` + kind + `","parameters":{}}]`, "unsupported_rule"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, calls := authorityServer(t, tc.classic, tc.rules)
			result, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if err != nil || result.State != "unproven" || result.Reason != tc.reason || result.Checks != nil || calls.Load() != 2 {
				t.Fatal("ambiguous authority acquired passing requirements", result, err, calls.Load())
			}
		})
	}
}
func TestRequiredCheckAuthorityIdenticalRuleReconciliationAndContradictions(t *testing.T) {
	equivalent := `{"ruleset_id":42,"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true,"required_status_checks":[{"context":"lint","integration_id":null},{"context":"build","integration_id":17},{"context":"build","integration_id":17}],"do_not_enforce_on_create":false}}`
	g, _ := authorityServer(t, authorityClassic, "["+authorityRule+","+equivalent+"]")
	result, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
	if err != nil || result.State != "complete" || !reflect.DeepEqual(result.Checks, []RequiredCheck{{Context: "build", AppID: 17}, {Context: "lint"}}) {
		t.Fatal("identical semantic rule duplicate changed authority", result, err)
	}
	for _, changed := range []string{
		strings.Replace(equivalent, `"integration_id":17`, `"integration_id":18`, 2),
		strings.Replace(equivalent, `"do_not_enforce_on_create":false`, `"do_not_enforce_on_create":true`, 1),
	} {
		g, _ := authorityServer(t, authorityClassic, "["+authorityRule+","+changed+"]")
		value, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
		if err != nil || value.State != "unproven" || value.Reason != "contradictory_rule_identity" || value.Checks != nil {
			t.Fatal("contradictory source identity reconciled", value, err)
		}
	}
	// Optional metadata does not invent a shared identity: separate requirement
	// rows with no parent ID still union and deduplicate only actual requirements.
	noMetadata := strings.ReplaceAll(authorityRule, `"ruleset_id":42,`, "")
	g, _ = authorityServer(t, authorityClassic, "["+noMetadata+","+noMetadata+"]")
	result, err = g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
	if err != nil || result.State != "complete" || len(result.Checks) != 2 {
		t.Fatal("missing metadata invented unique identity/requirements", result, err)
	}
}

func TestRequiredCheckAuthorityMalformedWireFailsClosed(t *testing.T) {
	cases := []struct{ name, classic, rules string }{
		{"classic missing outer", `{}`, `[]`},
		{"classic missing strict", `{"required_status_checks":{"contexts":[],"checks":[]}}`, `[]`},
		{"classic null strict", `{"required_status_checks":{"strict":null,"checks":[]}}`, `[]`},
		{"classic decimal app", strings.Replace(authorityClassic, `"app_id":17`, `"app_id":17.0`, 1), `[]`},
		{"classic bool app", strings.Replace(authorityClassic, `"app_id":17`, `"app_id":true`, 1), `[]`},
		{"classic duplicate key", strings.Replace(authorityClassic, `"app_id":17`, `"app_id":17,"app_id":17`, 1), `[]`},
		{"classic alias", strings.Replace(authorityClassic, `"app_id":17`, `"App_id":17`, 1), `[]`},
		{"null contexts", strings.Replace(authorityClassic, `["build","build"]`, `null`, 1), `[]`},
		{"null rules", authorityClassic, `null`},
		{"missing rule type", authorityClassic, `[{}]`},
		{"null rule", authorityClassic, `[null]`},
		{"null required array", authorityClassic, `[{"type":"required_status_checks","parameters":{"required_status_checks":null,"strict_required_status_checks_policy":true}}]`},
		{"rule decimal app", authorityClassic, `[` + strings.Replace(authorityRule, `"integration_id":17`, `"integration_id":1.7`, 1) + `]`},
		{"rule string app", authorityClassic, `[` + strings.Replace(authorityRule, `"integration_id":17`, `"integration_id":"17"`, 1) + `]`},
		{"duplicate raw rule key", authorityClassic, `[` + strings.Replace(authorityRule, `"type":"required_status_checks"`, `"type":"required_status_checks","type":"required_status_checks"`, 1) + `]`},
		{"unpaired context", strings.Replace(authorityClassic, `"context":"build"`, `"context":"\ud800"`, 1), `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := authorityServer(t, tc.classic, tc.rules)
			result, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if err == nil || result.State != "unproven" || result.Checks != nil {
				t.Fatal("malformed authority accepted", result, err)
			}
			if strings.Contains(err.Error(), "synthetic-authority-token") {
				t.Fatal("diagnostic exposed credential")
			}
		})
	}
	for _, context := range []string{"", " ", "build\n", "build\t", "build\u0085", strings.Repeat("x", 256)} {
		g, _ := authorityServer(t, strings.ReplaceAll(authorityClassic, "build", strings.Trim(fmt.Sprintf("%q", context), `"`)), `[]`)
		result, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
		if err == nil && result.State != "unproven" {
			t.Fatal("invalid required context accepted", len(context), result)
		}
	}
}
func TestRequiredCheckAuthoritySelectorsAndSelectedBranchPagination(t *testing.T) {
	var calls atomic.Int64
	branch := "feature/encoded%branch"
	escaped := url.PathEscape(branch)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" {
			t.Error("authority performed provider mutation")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/repos/owner/repo/branches/" + escaped + "/protection":
			_, _ = ioWriteString(w, `{"required_status_checks":null}`)
		case "/repos/owner/repo/rules/branches/" + escaped:
			if r.URL.Query().Get("per_page") != "100" {
				t.Error("paging lost bound")
			}
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", "<"+serverURL(r)+r.URL.EscapedPath()+"?per_page=100&page=2>; rel=next")
				_, _ = ioWriteString(w, `[{"type":"required_status_checks","ruleset_id":1,"parameters":{"required_status_checks":[{"context":"ci","integration_id":11}],"strict_required_status_checks_policy":false}}]`)
			} else {
				_, _ = ioWriteString(w, `[{"type":"required_status_checks","ruleset_id":2,"parameters":{"required_status_checks":[{"context":"lint","integration_id":12}],"strict_required_status_checks_policy":true}}]`)
			}
		default:
			t.Error("scope changed", r.URL.EscapedPath())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	g := testGitHub(server, "")
	result, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", branch)
	if err != nil || result.State != "complete" || len(result.Checks) != 2 || calls.Load() != 3 {
		t.Fatal("safe encoded selected branch pagination failed", result, err, calls.Load())
	}
	before := calls.Load()
	for _, bad := range []string{"", "../main", "refs//main", "main.lock", ".hidden", "main/", "/main", "-main", "@", "a@{b", "main?x=1", "main\n", "main\\x", "main*", string([]byte{0xff})} {
		_, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", bad)
		if !hasCode(err, "provider_invalid_response") {
			t.Fatal("invalid branch reached HTTP", bad, err)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid selector performed HTTP")
	}
	for _, repo := range []string{"owner/repo/extra", "../repo", "owner/repo?x=1"} {
		_, err := g.RequiredCheckAuthority(t.Context(), repo, "main")
		if err == nil {
			t.Fatal("invalid repository accepted")
		}
	}
}
func TestRequiredCheckAuthorityPermissionsPartialPagesAndNoRedirect(t *testing.T) {
	for _, status := range []int{403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = ioWriteString(w, `PRIVATE TOKEN ERROR`)
			}))
			defer server.Close()
			result, err := testGitHub(server, "").RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if err == nil || result.State != "unproven" || result.Checks != nil || calls.Load() != 1 || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("missing/forbidden authority became empty proof", result, err)
			}
		})
	}
	for _, kind := range []string{"page failure", "loop", "scope change", "malformed later", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/protection") {
					_, _ = ioWriteString(w, authorityClassic)
					return
				}
				if r.URL.Query().Get("page") == "2" {
					if kind == "malformed later" {
						_, _ = ioWriteString(w, `[{`)
					} else {
						w.WriteHeader(403)
					}
					return
				}
				if kind == "redirect" {
					w.Header().Set("Location", "https://unrelated.invalid/private")
					w.WriteHeader(302)
					return
				}
				target := serverURL(r) + r.URL.Path + "?per_page=100&page=2"
				if kind == "loop" {
					target = serverURL(r) + r.URL.Path + "?per_page=100&page=1"
				}
				if kind == "scope change" {
					target = serverURL(r) + "/repos/owner/repo/rules/branches/other?per_page=100&page=2"
				}
				w.Header().Set("Link", "<"+target+">; rel=next")
				_, _ = ioWriteString(w, "["+authorityRule+"]")
			}))
			defer server.Close()
			result, err := testGitHub(server, "synthetic-private").RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if err == nil || result.State != "unproven" || result.Checks != nil || calls.Load() > 3 || strings.Contains(err.Error(), "unrelated.invalid") {
				t.Fatal("partial/untrusted collection became proof", result, err, calls.Load())
			}
		})
	}
}

type authorityTransport func(*http.Request) (*http.Response, error)

func (f authorityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRequiredCheckAuthorityOneAggregateBudgetAndCancellation(t *testing.T) {
	g, _ := authorityServer(t, authorityClassic, "["+authorityRule+"]")
	original := g.httpClient.Transport
	var deadlines []time.Time
	// Disable only the fixture's client timeout to inspect the outer operation
	// deadline directly; production NewGitHub retains its five-second limit.
	g.httpClient.Timeout = 0
	g.httpClient.Transport = authorityTransport(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Error("authority request lacks aggregate deadline")
		}
		deadlines = append(deadlines, deadline)
		return original.RoundTrip(r)
	})
	started := time.Now()
	result, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
	if err != nil || result.State != "complete" || len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) || deadlines[0].Before(started.Add(14*time.Second)) || deadlines[0].After(time.Now().Add(15*time.Second)) {
		t.Fatal("collections restarted/lost fifteen-second operation budget", deadlines, result, err)
	}
	entered, closed := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/protection") {
			_, _ = ioWriteString(w, authorityClassic)
			return
		}
		close(entered)
		<-r.Context().Done()
		close(closed)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		value, err := testGitHub(server, "").RequiredCheckAuthority(ctx, "owner/repo", "main")
		if value.State != "unproven" || value.Checks != nil {
			t.Error("cancelled authority acquired partial proof")
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("rules read not entered")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancellation hidden")
		}
	case <-time.After(time.Second):
		t.Fatal("authority cancellation not bounded")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled HTTP left open")
	}
	if calls.Load() != 2 {
		t.Fatal("authority cancellation retried")
	}
}

func TestRequiredCheckAuthorityAllDocumentedIgnoredRulesAreNotMergeCredit(t *testing.T) {
	types := []string{"creation", "update", "deletion", "required_linear_history", "required_signatures", "pull_request", "non_fast_forward", "commit_message_pattern", "commit_author_email_pattern", "committer_email_pattern", "branch_name_pattern", "tag_name_pattern", "file_path_restriction", "max_file_path_length", "file_extension_restriction", "max_file_size"}
	rows := make([]string, 0, len(types))
	for _, kind := range types {
		rows = append(rows, `{"type":"`+kind+`"}`)
	}
	g, _ := authorityServer(t, `{"required_status_checks":null}`, "["+strings.Join(rows, ",")+"]")
	value, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
	if err != nil || value.State != "complete" || value.Checks == nil || len(value.Checks) != 0 {
		t.Fatal("non-CI rule required invented status checks", value, err)
	}
}
func TestRequiredCheckAuthoritySpecialGitRefCharactersStayInSelectedPath(t *testing.T) {
	for _, branch := range []string{"topic#literal", "topic%2Fname", "révision/界", "refs/heads/topic"} {
		t.Run(branch, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				expected := "/repos/owner/repo/"
				if strings.HasSuffix(r.URL.EscapedPath(), "/protection") {
					expected += "branches/" + url.PathEscape(branch) + "/protection"
					_, _ = ioWriteString(w, `{"required_status_checks":null}`)
				} else {
					expected += "rules/branches/" + url.PathEscape(branch)
					_, _ = ioWriteString(w, `[]`)
				}
				if r.URL.EscapedPath() != expected || r.URL.Fragment != "" {
					t.Error("branch escaped selected path", r.URL)
				}
			}))
			defer server.Close()
			value, err := testGitHub(server, "").RequiredCheckAuthority(t.Context(), "owner/repo", branch)
			if err != nil || value.State != "complete" || value.Branch != branch || calls.Load() != 2 {
				t.Fatal("valid selected Git ref changed", value, err)
			}
		})
	}
}

func TestRequiredCheckAuthorityUnknownOriginLeavesCannotBecomeAny(t *testing.T) {
	for _, key := range []string{"app_id", "integrationId", "Integration_id", "origin", "app", "unknown"} {
		t.Run("rule/"+key, func(t *testing.T) {
			rules := `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"ci","` + key + `":17}],"strict_required_status_checks_policy":false}}]`
			g, _ := authorityServer(t, `{"required_status_checks":null}`, rules)
			value, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if err == nil || value.State != "unproven" || value.Checks != nil {
				t.Fatal("unsupported rule origin weakened to unrestricted check", value, err)
			}
		})
	}
	for _, key := range []string{"integration_id", "appId", "App_id", "origin", "app", "unknown"} {
		t.Run("classic/"+key, func(t *testing.T) {
			classic := strings.Replace(authorityClassic, `"app_id":17`, `"app_id":17,"`+key+`":17`, 1)
			g, _ := authorityServer(t, classic, `[]`)
			value, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if err == nil || value.State != "unproven" || value.Checks != nil {
				t.Fatal("unknown classic origin accepted despite valid known field", value, err)
			}
		})
	}
	g, _ := authorityServer(t, strings.Replace(authorityClassic, "build", string([]byte{0xff}), 1), `[]`)
	value, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
	if err == nil || value.State != "unproven" || value.Checks != nil {
		t.Fatal("invalid raw UTF8 accepted", value, err)
	}
}
