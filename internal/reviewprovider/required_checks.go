package reviewprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// RequiredCheck preserves exact context spelling. AppID zero represents only a
// provider-explicit/documented unrestricted origin, never missing classic data.
type RequiredCheck struct {
	Context string
	AppID   int64
}

// RequiredCheckAuthority describes required-status-check authority only. Even
// complete empty authority is not a verdict on CI results or merge readiness.
type RequiredCheckAuthority struct {
	State      string
	Reason     string
	Repository string
	Branch     string
	ObservedAt time.Time
	Strict     bool
	Checks     []RequiredCheck
}

type githubClassicProtection struct {
	Required *githubClassicRequired `json:"required_status_checks,required"`
}
type githubClassicRequired struct {
	Strict   bool                 `json:"strict,required"`
	Contexts []string             `json:"contexts"`
	Checks   []githubClassicCheck `json:"checks"`
}
type githubClassicCheck struct {
	Context string `json:"context,required"`
	AppID   *int64 `json:"app_id"`
}
type githubBranchRule struct {
	parametersPresent bool
	Type              string                      `json:"type,required"`
	Parameters        *githubStatusRuleParameters `json:"parameters"`
	RulesetID         *int64                      `json:"ruleset_id"`
	SourceType        *string                     `json:"ruleset_source_type"`
	Source            *string                     `json:"ruleset_source"`
}

// Preserve omission separately from explicit null for optional rule parameters.
func (r *githubBranchRule) UnmarshalJSON(data []byte) error {
	type wire githubBranchRule
	var value wire
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*r = githubBranchRule(value)
	_, r.parametersPresent = fields["parameters"]
	return nil
}

// Copilot review rules request feedback; they do not define required CI gates.
// The published rule schema permits omitted parameters and optional booleans.
// https://docs.github.com/en/rest/repos/rules#get-rules-for-a-branch
func validCopilotReviewRule(r githubBranchRule) bool {
	if !r.parametersPresent {
		return true
	}
	if r.Parameters == nil {
		return false
	}
	var parameters map[string]json.RawMessage
	if strictjson.Decode(r.Parameters.raw, &parameters) != nil || parameters == nil {
		return false
	}
	for name, raw := range parameters {
		if name != "review_draft_pull_requests" && name != "review_on_push" {
			return false
		}
		var flag *bool
		if json.Unmarshal(raw, &flag) != nil || flag == nil {
			return false
		}
	}
	return true
}

type githubStatusRuleParameters struct {
	raw                  []byte
	DoNotEnforceOnCreate *bool             `json:"do_not_enforce_on_create"`
	Checks               []githubRuleCheck `json:"required_status_checks"`
	Strict               *bool             `json:"strict_required_status_checks_policy"`
}
type githubRuleCheck struct {
	Context       string `json:"context,required"`
	IntegrationID *int64 `json:"integration_id"`
}

// Origin-bearing leaves are closed for the pinned provider API version. An
// unfamiliar origin spelling must never turn into documented omission/any.
func (p *githubClassicCheck) UnmarshalJSON(data []byte) error {
	type wire githubClassicCheck
	var value wire
	if err := strictjson.Decode(data, &value); err != nil {
		return err
	}
	*p = githubClassicCheck(value)
	return nil
}
func (p *githubRuleCheck) UnmarshalJSON(data []byte) error {
	type wire githubRuleCheck
	var value wire
	if err := strictjson.Decode(data, &value); err != nil {
		return err
	}
	*p = githubRuleCheck(value)
	return nil
}
func (p *githubStatusRuleParameters) UnmarshalJSON(data []byte) error {
	type wire githubStatusRuleParameters
	var value wire
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*p = githubStatusRuleParameters(value)
	p.raw = append([]byte(nil), data...)
	return nil
}
func statusRuleSemantic(p *githubStatusRuleParameters, checks []RequiredCheck) string {
	if checks != nil {
		values := make(map[RequiredCheck]bool)
		for _, check := range checks {
			values[check] = true
		}
		ordered := make([]RequiredCheck, 0, len(values))
		for check := range values {
			ordered = append(ordered, check)
		}
		sortRequiredChecks(ordered)
		create := false
		if p.DoNotEnforceOnCreate != nil {
			create = *p.DoNotEnforceOnCreate
		}
		body, _ := json.Marshal(struct {
			Strict, DoNotEnforceOnCreate bool
			Checks                       []RequiredCheck
		}{*p.Strict, create, ordered})
		return string(body)
	}
	if p == nil {
		return "null"
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(p.raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return "invalid"
	}
	body, _ := json.Marshal(value)
	return string(body)
}
func sortRequiredChecks(checks []RequiredCheck) {
	slices.SortFunc(checks, func(a, b RequiredCheck) int {
		if order := strings.Compare(a.Context, b.Context); order != 0 {
			return order
		}
		if a.AppID < b.AppID {
			return -1
		}
		if a.AppID > b.AppID {
			return 1
		}
		return 0
	})
}

// RequiredCheckAuthority reads classic protection and every applicable active
// branch rule under one 15-second budget. It never enumerates repositories,
// retries, posts, or evaluates candidate results. The active branch-rules API
// already includes applicable parent rules and excludes disabled/evaluate rules.
// See https://docs.github.com/en/rest/branches/branch-protection and
// https://docs.github.com/en/rest/repos/rules#get-rules-for-a-branch .
func (g *GitHub) RequiredCheckAuthority(ctx context.Context, repository, branch string) (RequiredCheckAuthority, error) {
	result := RequiredCheckAuthority{State: "unproven", Reason: "authority_unavailable"}
	if !validRepositorySelector(repository) || !validRequiredBranch(branch) {
		return result, invalidSelector()
	}
	result.Repository, result.Branch = repository, branch
	if ctx == nil || ctx.Err() != nil {
		return result, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	if g == nil || g.httpClient == nil || !validBaseURL(g.baseURL) {
		return result, providerError("provider_unavailable", "The provider adapter is unavailable.", 0, 0, false)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	path := "/repos/" + repository
	selected := url.PathEscape(branch)
	var classic githubClassicProtection
	if err := g.getJSON(ctx, path+"/branches/"+selected+"/protection", nil, &classic); err != nil {
		var missing *Error
		if !errors.As(err, &missing) || missing.Code != "provider_not_found" || missing.Status != http.StatusNotFound {
			return result, err
		}
		// A protection 404 alone cannot distinguish absence from missing access.
		// The exact existing branch must positively report that it is unprotected.
		// A protected summary can also describe rulesets, so it cannot establish
		// classic absence or replace full classic strictness/app identity.
		var branchState struct {
			Name      string `json:"name,required"`
			Protected *bool  `json:"protected,required"`
		}
		if branchErr := g.getJSON(ctx, path+"/branches/"+selected, nil, &branchState); branchErr != nil {
			return result, branchErr
		}
		if branchState.Name != branch || branchState.Protected == nil {
			return result, err
		}
		if *branchState.Protected {
			result.Reason = "classic_absence_unproven_ruleset_branch"
			result.ObservedAt = time.Now().UTC()
			return result, nil
		}
	}
	var rules []githubBranchRule
	if err := g.getPages(ctx, path+"/rules/branches/"+selected, &rules); err != nil {
		return result, err
	}
	if ctx.Err() != nil {
		return result, providerError("provider_unavailable", "The provider request was canceled.", 0, 0, false)
	}
	result.ObservedAt = time.Now().UTC()
	checks := make(map[RequiredCheck]struct{})
	positiveApps := make(map[string]int64)
	add := func(context string, app int64) string {
		if !validRequiredContext(context) {
			return "required_checks_malformed"
		}
		if app > 0 {
			if known, ok := positiveApps[context]; ok && known != app {
				return "conflicting_app_requirements"
			}
			positiveApps[context] = app
		}
		checks[RequiredCheck{Context: context, AppID: app}] = struct{}{}
		if len(checks) > githubMaxItems {
			return "required_checks_capacity"
		}
		return ""
	}
	unproven := func(reason string) (RequiredCheckAuthority, error) { result.Reason = reason; return result, nil }
	if c := classic.Required; c != nil {
		if c.Contexts == nil && c.Checks == nil || len(c.Contexts) > githubMaxItems || len(c.Checks) > githubMaxItems {
			return unproven("required_checks_malformed")
		}
		result.Strict = c.Strict
		bound := make(map[string]bool)
		for _, check := range c.Checks {
			if !validRequiredContext(check.Context) {
				return unproven("required_checks_malformed")
			}
			if check.AppID == nil || *check.AppID == 0 {
				return unproven("check_app_identity_unproven")
			}
			app := *check.AppID
			if app == -1 {
				app = 0
			} else if app < 1 {
				return unproven("check_app_identity_unproven")
			}
			if reason := add(check.Context, app); reason != "" {
				return unproven(reason)
			}
			bound[check.Context] = true
		}
		for _, context := range c.Contexts {
			if !validRequiredContext(context) {
				return unproven("required_checks_malformed")
			}
			if !bound[context] {
				return unproven("check_app_identity_unproven")
			}
		}
	}
	// Parent provenance is optional. When provided, conflicting representations
	// of that same rule stay unproven; identical semantic duplicates reconcile.
	type ruleIdentity struct {
		id   int64
		kind string
	}
	seenRules := make(map[ruleIdentity]string)
	for _, rule := range rules {
		if rule.Type == "" || len(rule.Type) > 128 || !utf8.ValidString(rule.Type) || strings.IndexFunc(rule.Type, unicode.IsControl) >= 0 {
			return unproven("required_checks_malformed")
		}
		if rule.RulesetID != nil {
			if *rule.RulesetID <= 0 {
				return unproven("required_checks_malformed")
			}
		}
		if rule.SourceType != nil && (*rule.SourceType != "Repository" && *rule.SourceType != "Organization" && *rule.SourceType != "Enterprise") {
			return unproven("required_checks_malformed")
		}
		if rule.Source != nil && (!validPlainText(*rule.Source, 300) || strings.TrimSpace(*rule.Source) == "") {
			return unproven("required_checks_malformed")
		}
		var normalized []RequiredCheck
		switch rule.Type {
		case "required_status_checks":
			p := rule.Parameters
			if p == nil || p.Strict == nil || p.Checks == nil || len(p.Checks) > githubMaxItems {
				return unproven("required_checks_malformed")
			}
			normalized = make([]RequiredCheck, 0, len(p.Checks))
			for _, check := range p.Checks {
				app := int64(0)
				if check.IntegrationID != nil {
					if *check.IntegrationID <= 0 {
						return unproven("check_app_identity_unproven")
					}
					app = *check.IntegrationID
				}
				if !validRequiredContext(check.Context) {
					return unproven("required_checks_malformed")
				}
				normalized = append(normalized, RequiredCheck{Context: check.Context, AppID: app})
			}
		// These documented rules constrain ref updates, commit/path metadata or
		// human review, not required check origins/results. Ignoring them asserts
		// nothing about their satisfaction or full merge gates. Integration gates
		// (workflows, scanning, deployments, merge queue, etc.) stay unsupported.
		case "copilot_code_review":
			if !validCopilotReviewRule(rule) {
				return unproven("required_checks_malformed")
			}
		case "creation", "update", "deletion", "required_linear_history", "required_signatures", "pull_request", "non_fast_forward", "commit_message_pattern", "commit_author_email_pattern", "committer_email_pattern", "branch_name_pattern", "tag_name_pattern", "file_path_restriction", "max_file_path_length", "file_extension_restriction", "max_file_size":
		default:
			return unproven("unsupported_rule")
		}
		if rule.RulesetID != nil {
			identity := ruleIdentity{*rule.RulesetID, rule.Type}
			semantic := statusRuleSemantic(rule.Parameters, normalized)
			if prior, ok := seenRules[identity]; ok && prior != semantic {
				return unproven("contradictory_rule_identity")
			}
			seenRules[identity] = semantic
		}
		if normalized != nil {
			result.Strict = result.Strict || *rule.Parameters.Strict
			for _, check := range normalized {
				if reason := add(check.Context, check.AppID); reason != "" {
					return unproven(reason)
				}
			}
		}
	}
	result.Checks = make([]RequiredCheck, 0, len(checks))
	for check := range checks {
		result.Checks = append(result.Checks, check)
	}
	sortRequiredChecks(result.Checks)
	result.State, result.Reason = "complete", ""
	return result, nil
}
func validRequiredContext(value string) bool {
	return len(value) <= 255 && utf8.ValidString(value) && strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) < 0
}
func validRequiredBranch(value string) bool {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || value == "@" || strings.HasPrefix(value, "-") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == ' ' || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return true
}
