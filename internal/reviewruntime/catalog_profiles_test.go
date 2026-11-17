package reviewruntime

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func profileCatalogFixture(t *testing.T) map[string]any {
	t.Helper()
	root := t.TempDir()
	return map[string]any{
		"version": 1,
		"cli_authentication_profiles": []any{
			map[string]any{"id": "developer-a", "account": "account-a", "organization": "team", "authentication_environment": map[string]any{"HOME": filepath.Join(root, "account-a")}},
			map[string]any{"id": "developer-b", "account": "account-b", "organization": "team", "authentication_environment": map[string]any{"HOME": filepath.Join(root, "account-b")}},
		},
		"capabilities": []any{
			map[string]any{"id": "project-one", "account": "account-a", "organization": "team", "channels": []string{"cli"}, "git_executable": "git", "cli_executable": "coderabbit", "cli_authentication_profile": "developer-a"},
			map[string]any{"id": "project-two", "account": "account-a", "organization": "team", "channels": []string{"cli"}, "git_executable": "git", "cli_executable": "coderabbit", "cli_authentication_profile": "developer-a"},
			map[string]any{"id": "other-developer", "account": "account-b", "organization": "team", "channels": []string{"cli"}, "git_executable": "git", "cli_executable": "coderabbit", "cli_authentication_profile": "developer-b"},
		},
	}
}

func loadProfileCatalog(t *testing.T, input map[string]any) (Catalog, error) {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return LoadCatalog(writeCatalog(t, t.TempDir(), string(data)))
}

func TestNamedCLIProfilesResolveWithoutCredentialsAndKeepIndependentCopies(t *testing.T) {
	input := profileCatalogFixture(t)
	loaded, err := loadProfileCatalog(t, input)
	if err != nil {
		t.Fatal(err)
	}
	first, second, third := loaded.Capabilities[0], loaded.Capabilities[1], loaded.Capabilities[2]
	if first.Account != second.Account || first.Account == third.Account || !reflect.DeepEqual(first.AuthenticationEnvironment, second.AuthenticationEnvironment) || reflect.DeepEqual(first.AuthenticationEnvironment, third.AuthenticationEnvironment) {
		t.Fatal("named profile account or environment binding changed")
	}
	// No authentication directories exist. Loading resolves configuration only;
	// it never copies an existing login or attempts provider authentication.
	first.AuthenticationEnvironment["HOME"] = filepath.Join(t.TempDir(), "modified")
	if first.AuthenticationEnvironment["HOME"] == second.AuthenticationEnvironment["HOME"] {
		t.Fatal("capabilities share mutable authentication maps")
	}
	serialized, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(serialized), fmt.Sprintf("%#v %v", loaded, loaded.Capabilities)} {
		for _, hidden := range []string{"developer-a", "developer-b", "authentication_environment", second.AuthenticationEnvironment["HOME"], third.AuthenticationEnvironment["HOME"]} {
			if strings.Contains(output, hidden) {
				t.Fatal("loaded catalog exposed private profile data")
			}
		}
	}
}

func TestNamedCLIProfileUsesEffectiveBindingFingerprint(t *testing.T) {
	input := profileCatalogFixture(t)
	named, err := loadProfileCatalog(t, input)
	if err != nil {
		t.Fatal(err)
	}
	profiles := input["cli_authentication_profiles"].([]any)
	capabilities := input["capabilities"].([]any)
	first := capabilities[0].(map[string]any)
	delete(first, "cli_authentication_profile")
	first["authentication_environment"] = profiles[0].(map[string]any)["authentication_environment"]
	inline, err := loadProfileCatalog(t, input)
	if err != nil || inline.Capabilities[0].Fingerprint != named.Capabilities[0].Fingerprint {
		t.Fatal("moving unchanged authentication into a named profile changed effective identity", err)
	}
	delete(first, "authentication_environment")
	first["cli_authentication_profile"] = "renamed-profile"
	capabilities[1].(map[string]any)["cli_authentication_profile"] = "renamed-profile"
	profiles[0].(map[string]any)["id"] = "renamed-profile"
	rename, err := loadProfileCatalog(t, input)
	if err != nil || rename.Capabilities[0].Fingerprint != named.Capabilities[0].Fingerprint {
		t.Fatal("a profile label changed effective identity", err)
	}
	profiles[0].(map[string]any)["authentication_environment"].(map[string]any)["HOME"] = filepath.Join(t.TempDir(), "new-login-context")
	changed, err := loadProfileCatalog(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Capabilities[0].Fingerprint == named.Capabilities[0].Fingerprint || changed.Capabilities[1].Fingerprint == named.Capabilities[1].Fingerprint || changed.Capabilities[2].Fingerprint != named.Capabilities[2].Fingerprint {
		t.Fatal("profile change did not affect exactly its referencing capabilities")
	}
}

func TestNamedCLIProfileRejectsAmbiguityAndUnsafeBindings(t *testing.T) {
	tests := map[string]func(map[string]any, map[string]any, map[string]any){
		"null-profiles":             func(root, profile, cap map[string]any) { root["cli_authentication_profiles"] = nil },
		"object-profiles":           func(root, profile, cap map[string]any) { root["cli_authentication_profiles"] = map[string]any{} },
		"missing-profile":           func(root, profile, cap map[string]any) { cap["cli_authentication_profile"] = "missing" },
		"empty-reference":           func(root, profile, cap map[string]any) { cap["cli_authentication_profile"] = "" },
		"null-reference":            func(root, profile, cap map[string]any) { cap["cli_authentication_profile"] = nil },
		"invalid-reference":         func(root, profile, cap map[string]any) { cap["cli_authentication_profile"] = "../outside" },
		"inline-and-reference":      func(root, profile, cap map[string]any) { cap["authentication_environment"] = map[string]any{} },
		"null-inline-and-reference": func(root, profile, cap map[string]any) { cap["authentication_environment"] = nil },
		"account-mismatch":          func(root, profile, cap map[string]any) { cap["account"] = "other-account" },
		"organization-mismatch":     func(root, profile, cap map[string]any) { cap["organization"] = "other-org" },
		"hosted-only":               func(root, profile, cap map[string]any) { cap["channels"] = []string{"hosted"} },
		"missing-id":                func(root, profile, cap map[string]any) { delete(profile, "id") },
		"null-id":                   func(root, profile, cap map[string]any) { profile["id"] = nil },
		"unknown-field":             func(root, profile, cap map[string]any) { profile["secret_token"] = "synthetic-not-a-secret" },
		"missing-account":           func(root, profile, cap map[string]any) { delete(profile, "account") },
		"missing-org":               func(root, profile, cap map[string]any) { delete(profile, "organization") },
		"missing-environment":       func(root, profile, cap map[string]any) { delete(profile, "authentication_environment") },
		"empty-environment":         func(root, profile, cap map[string]any) { profile["authentication_environment"] = map[string]any{} },
		"null-environment":          func(root, profile, cap map[string]any) { profile["authentication_environment"] = nil },
		"null-home": func(root, profile, cap map[string]any) {
			profile["authentication_environment"] = map[string]any{"HOME": nil}
		},
		"relative-home": func(root, profile, cap map[string]any) {
			profile["authentication_environment"] = map[string]any{"HOME": "relative"}
		},
		"secret-environment": func(root, profile, cap map[string]any) {
			profile["authentication_environment"] = map[string]any{"CODERABBIT_API_KEY": "synthetic-not-a-secret"}
		},
		"duplicate-id":      func(root, profile, cap map[string]any) { root["cli_authentication_profiles"] = []any{profile, profile} },
		"too-many-profiles": func(root, profile, cap map[string]any) { root["cli_authentication_profiles"] = make([]any, 33) },
		"missing-home": func(root, profile, cap map[string]any) {
			profile["authentication_environment"] = map[string]any{"XDG_CACHE_HOME": filepath.Join(t.TempDir(), "cache")}
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			input := profileCatalogFixture(t)
			profile := input["cli_authentication_profiles"].([]any)[0].(map[string]any)
			capability := input["capabilities"].([]any)[0].(map[string]any)
			change(input, profile, capability)
			_, err := loadProfileCatalog(t, input)
			if err == nil || err.Error() != configurationError().Error() {
				t.Fatal("invalid profile was accepted or exposed private details", err)
			}
		})
	}
}

func TestNamedCLIProfilesRejectOverlappingDifferentAccountHomes(t *testing.T) {
	for _, relation := range []string{"same", "nested", "parent", "cleaned"} {
		t.Run(relation, func(t *testing.T) {
			input := profileCatalogFixture(t)
			profiles := input["cli_authentication_profiles"].([]any)
			first := profiles[0].(map[string]any)["authentication_environment"].(map[string]any)
			second := profiles[1].(map[string]any)["authentication_environment"].(map[string]any)
			home := first["HOME"].(string)
			switch relation {
			case "same":
				second["HOME"] = home
			case "nested":
				second["HOME"] = filepath.Join(home, "nested")
			case "parent":
				second["HOME"] = filepath.Dir(home)
			case "cleaned":
				second["HOME"] = home + string(filepath.Separator) + "child" + string(filepath.Separator) + ".."
			}
			if _, err := loadProfileCatalog(t, input); err == nil {
				t.Fatal("overlapping directories gave different account labels the same login context")
			}
		})
	}
}

func TestNamedCLIProfileCannotAliasInlineAccountContext(t *testing.T) {
	for _, referenced := range []bool{true, false} {
		t.Run(fmt.Sprintf("referenced=%t", referenced), func(t *testing.T) {
			input := profileCatalogFixture(t)
			profiles := input["cli_authentication_profiles"].([]any)
			third := input["capabilities"].([]any)[2].(map[string]any)
			delete(third, "cli_authentication_profile")
			third["authentication_environment"] = profiles[0].(map[string]any)["authentication_environment"]
			if !referenced {
				input["capabilities"] = []any{third}
			}
			if _, err := loadProfileCatalog(t, input); err == nil {
				t.Fatal("inline capability relabeled a named profile's home as another account")
			}
		})
	}
}
