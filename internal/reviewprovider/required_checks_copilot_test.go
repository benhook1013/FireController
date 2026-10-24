package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

func TestRequiredCheckAuthorityCopilotRulePreservesRequiredCI(t *testing.T) {
	for name, parameters := range map[string]string{
		"observed options":            `,"parameters":{"review_draft_pull_requests":false,"review_on_push":true}`,
		"opposite options":            `,"parameters":{"review_draft_pull_requests":true,"review_on_push":false}`,
		"single optional flag":        `,"parameters":{"review_on_push":false}`,
		"empty optional parameters":   `,"parameters":{}`,
		"omitted optional parameters": ``,
	} {
		t.Run(name, func(t *testing.T) {
			rule := `{"type":"copilot_code_review","ruleset_id":123,"ruleset_source_type":"Repository","ruleset_source":"owner/repo"` + parameters + `}`
			g, calls := authorityServer(t, authorityClassic, `[`+authorityRule+`,`+rule+`]`)
			got, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			want := []RequiredCheck{{Context: "build", AppID: 17}, {Context: "lint"}}
			if err != nil || got.State != "complete" || got.Reason != "" || !got.Strict || !reflect.DeepEqual(got.Checks, want) || calls.Load() != 2 {
				t.Fatal("known non-CI review rule hid required checks", got, err, calls.Load())
			}
		})
	}
}

func TestRequiredCheckAuthorityMalformedCopilotRuleRemainsUnproven(t *testing.T) {
	for name, parameters := range map[string]string{
		"null parameters":    `null`,
		"array parameters":   `[]`,
		"null draft flag":    `{"review_draft_pull_requests":null}`,
		"null push flag":     `{"review_on_push":null}`,
		"string draft flag":  `{"review_draft_pull_requests":"false"}`,
		"numeric push flag":  `{"review_on_push":1}`,
		"unknown gate field": `{"required_status_checks":[]}`,
		"unknown option":     `{"future_merge_gate":true}`,
		"case alias":         `{"Review_on_push":false}`,
		"duplicate flag":     `{"review_on_push":false,"review_on_push":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			g, _ := authorityServer(t, authorityClassic, `[{"type":"copilot_code_review","parameters":`+parameters+`}]`)
			got, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
			if got.State != "unproven" || got.Checks != nil || err == nil && got.Reason != "required_checks_malformed" {
				t.Fatal("malformed review rule supplied CI authority", got, err)
			}
		})
	}
	// Recognizing this documented rule never turns arbitrary future rules into
	// ignorable non-CI metadata.
	g, _ := authorityServer(t, authorityClassic, `[{"type":"copilot_code_review","parameters":{"review_on_push":true}},{"type":"future_check_gate","parameters":{}}]`)
	got, err := g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
	if err != nil || got.State != "unproven" || got.Reason != "unsupported_rule" || got.Checks != nil {
		t.Fatal("unknown rule stopped failing closed", got, err)
	}
	// Contradictory instances of one retained rule keep their existing refusal.
	rule := `{"type":"copilot_code_review","ruleset_id":123,"parameters":{"review_on_push":true}}`
	g, _ = authorityServer(t, authorityClassic, `[`+rule+`,`+strings.Replace(rule, `true`, `false`, 1)+`]`)
	got, err = g.RequiredCheckAuthority(t.Context(), "owner/repo", "main")
	if err != nil || got.State != "unproven" || got.Reason != "contradictory_rule_identity" || got.Checks != nil {
		t.Fatal("contradictory review rule supplied CI authority", got, err)
	}
}
