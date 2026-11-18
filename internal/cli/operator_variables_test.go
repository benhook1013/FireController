package cli_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/cli"
	"github.com/benhook1013/FireController/internal/client"
)

func TestVarsCLISetGetOverrideUnsetHistoryAndDoctor(t *testing.T) {
	f := setupServiceFixture(t)
	p, other := f.project(t, "vars-cli"), f.project(t, "vars-other")
	worker, otherWorker := f.worker(t, p, "Worker"), f.worker(t, other, "Other")
	service := httptest.NewServer(f.handler)
	defer service.Close()
	path := mutationProfiles(t, f, service.URL, map[string][2]string{"chosen": {p, worker}, "other": {other, otherWorker}})
	invoke := func(profile string, args ...string) string {
		t.Helper()
		code, out, stderr := mutationInvoke(t, path, profile, args...)
		if code != cli.ExitSuccess || stderr != "" {
			t.Fatal(args, code, out, stderr)
		}
		return out
	}
	var receipt client.OperatorVariableReceipt
	if json.Unmarshal([]byte(invoke("chosen", "vars", "set", "provider_mode", "codex-low")), &receipt) != nil || receipt.Outcome != "updated" || receipt.Revision != 1 || receipt.Scope != "instance" {
		t.Fatal(receipt)
	}
	if json.Unmarshal([]byte(invoke("chosen", "vars", "set", "provider_mode", "--project", "claude-low", "--expect-revision", "0")), &receipt) != nil || receipt.Scope != "project" || receipt.Revision != 1 {
		t.Fatal(receipt)
	}
	var list client.OperatorVariables
	if json.Unmarshal([]byte(invoke("other", "vars", "list")), &list) != nil || len(list.Variables) != 1 || list.Variables[0].Value != "codex-low" || list.Variables[0].Scope != "instance" {
		t.Fatal("other project", list)
	}
	var detail client.OperatorVariableDetail
	if json.Unmarshal([]byte(invoke("chosen", "vars", "get", "provider_mode")), &detail) != nil || !detail.Set || detail.Value != "claude-low" || detail.Scope != "project" || detail.Instance == nil || detail.Instance.Value != "codex-low" {
		t.Fatal("override", detail)
	}
	var doctor struct {
		OperatorVariables map[string]string `json:"operator_variables"`
	}
	if json.Unmarshal([]byte(invoke("chosen", "doctor")), &doctor) != nil || doctor.OperatorVariables["provider_mode"] != "claude-low" {
		t.Fatal("doctor", doctor)
	}
	code, out, stderr := mutationInvoke(t, path, "chosen", "vars", "set", "provider_mode", "x", "--expect-revision", "0")
	if code != cli.ExitConflict || out != "" || !strings.Contains(stderr, "operator_variable_revision_conflict") {
		t.Fatal("conflict", code, out, stderr)
	}
	invoke("chosen", "vars", "unset", "provider_mode", "--project")
	if json.Unmarshal([]byte(invoke("chosen", "vars", "get", "provider_mode")), &detail) != nil || detail.Value != "codex-low" || detail.Scope != "instance" {
		t.Fatal("fallback", detail)
	}
	var page client.Page[client.OperatorVariableRevision]
	if json.Unmarshal([]byte(invoke("chosen", "vars", "history", "provider_mode", "--project", "--limit", "1")), &page) != nil || len(page.Items) != 1 || page.Items[0].Set || !page.HasMore {
		t.Fatal("history", page)
	}
	for _, args := range [][]string{{"vars", "set", "Bad", "x"}, {"vars", "set", "k"}, {"vars", "get"}, {"vars", "list", "extra"}, {"vars", "set", "k", " padded"}, {"vars", "get", "k", "--project"}, {"vars", "nope", "k"}} {
		if code, _, _ := mutationInvoke(t, path, "chosen", args...); code != cli.ExitUsage {
			t.Fatal("accepted", args, code)
		}
	}
	if code, out, _ := run([]string{"vars", "help"}, nil); code != cli.ExitSuccess || !strings.Contains(out, "provider_mode") {
		t.Fatal("help", code, out)
	}
}
