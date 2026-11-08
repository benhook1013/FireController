package reviewexec

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCLIWaitEstimateProjectionPreservesBaselineAndVersionContract(t *testing.T) {
	f, a, in, _ := cliWaitFixture(t)
	if _, err := f.e.ScheduleCLICooldown(t.Context(), attemptProject, a.ID, a.Revision); err != nil {
		t.Fatal(err)
	}
	original := cliWaitRead(t, f, a.OperationID)
	if original.Wait.CLIAllowance == nil || original.Wait.CLIAllowance.ObservedAt != in.ObservedAt || *original.Wait.CLIAllowance.FullCapacityInMS != in.FullCapacityInMS {
		t.Fatal("lost retained provider estimate", original.Wait)
	}
	base := "/api/v1/projects/" + attemptProject + "/reviews/operations/" + a.OperationID
	for _, path := range []string{base, base + "/status", "/api/v1/projects/" + attemptProject + "/reviews/rounds/progress?pull_numbers=2&view=stages"} {
		for _, version := range []string{"1", "2", "3"} {
			response := versionWaitRead(t, f, path, version)
			contains := strings.Contains(response.Body.String(), `"cli_allowance"`)
			if contains != (version == "3") {
				t.Fatal("incorrect negotiated estimate projection", version, response.Body.String())
			}
		}
	}
	// Projection must not mutate shared provenance, comparison or retry time.
	old := operationWaitForResponse(original.Wait, "cli", "2")
	if old.CLIAllowance != nil || original.Wait.CLIAllowance == nil || old.RetryNotBefore != original.Wait.RetryNotBefore {
		t.Fatal("version projection changed wait", old, original.Wait)
	}
	active := original
	active.State = "preparing"
	active.AttemptID = a.ID
	if currentOperationWait(active) != nil {
		t.Fatal("historical baseline leaked as current wait")
	}
	// Zero is a valid retained provider estimate, not missing data.
	zero := int64(0)
	copy := *original.Wait.CLIAllowance
	copy.FullCapacityInMS = &zero
	wait := *original.Wait
	wait.CLIAllowance = &copy
	raw, err := json.Marshal(operationWaitForResponse(&wait, "cli", "3"))
	if err != nil || !strings.Contains(string(raw), `"full_capacity_in_ms":0`) {
		t.Fatal("lost zero estimate", string(raw), err)
	}
	// The original attempt comparison is still required for its estimate.
	admissionSQL(t, f, `UPDATE review_operations SET head_sha=? WHERE id=?`, strings.Repeat("c", 40), a.OperationID)
	if _, err := readOperation(t.Context(), f.store.DB(), attemptProject, a.OperationID); err == nil {
		t.Fatal("mismatched baseline comparison supplied an estimate")
	}
}
