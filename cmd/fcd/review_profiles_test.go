package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewruntime"
)

func TestNamedCLIAuthenticationProfilesComposeWithoutLogin(t *testing.T) {
	path, _ := privateTestCatalog(t, []string{"cli"})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var input map[string]any
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	capability := input["capabilities"].([]any)[0].(map[string]any)
	environment := capability["authentication_environment"].(map[string]any)
	home := environment["HOME"].(string)
	input["cli_authentication_profiles"] = []any{map[string]any{
		"id": "shared-developer", "account": capability["account"], "organization": capability["organization"], "authentication_environment": environment,
	}}
	delete(capability, "authentication_environment")
	capability["cli_authentication_profile"] = "shared-developer"
	second := make(map[string]any, len(capability))
	for k, v := range capability {
		second[k] = v
	}
	second["id"] = "second-project-capability"
	input["capabilities"] = []any{capability, second}
	data, err = json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := reviewruntime.LoadCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "private-service")
	store, err := core.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := reviewexec.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	engine, drivers, err := buildReviewExecution(store, dir, catalog)
	if err != nil || engine == nil || len(drivers) != 2 {
		t.Fatal("named login profile did not compose independently referenced capabilities", err)
	}
	for _, id := range []string{"local-cli", "second-project-capability"} {
		if _, ok := drivers[id].(*reviewruntime.CLIDriver); !ok {
			t.Fatal("expected CLI driver missing")
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("runtime composition created an authentication home", err)
	}
}
