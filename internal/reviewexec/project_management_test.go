package reviewexec

import "testing"

func TestProjectWriterConfiguresPolicyAndExecutionWithReplayReauthorization(t *testing.T) {
	f := newAttemptFixtureWithPolicy(t, false)
	if err := EnsurePolicySchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	scope := f.scope(t, attemptProject, f.writer, false)
	revision := int64(0)
	initial := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2}, Channels: []string{"cli"}}
	_, err := f.e.Configure(t.Context(), scope, "writer-cannot-bind", initial)
	attemptCode(t, err, "operator_capability_binding_required")
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "operator-initial-binding", initial); err != nil {
		t.Fatal(err)
	}
	revision = 1
	config := Configuration{ExpectedRevision: &revision, CapabilityID: "test-capability", Mode: "stacked", Queue: []int64{2, 4}, Channels: []string{"cli"}, Paused: true}
	switched := config
	switched.CapabilityID = "other-account"
	_, err = f.e.Configure(t.Context(), scope, "writer-cannot-switch-account", switched)
	attemptCode(t, err, "operator_capability_binding_required")
	if _, err = f.e.Configure(t.Context(), scope, "owner-config", config); err != nil {
		t.Fatal("writer execution config refused", err)
	}
	policy := policyInput(0)
	if _, err := f.e.ConfigurePolicy(t.Context(), scope, "owner-policy", policy); err != nil {
		t.Fatal("writer policy refused", err)
	}
	if configured, err := f.e.ReadExecutionConfiguration(t.Context(), scope); err != nil || configured.Mode != "stacked" || !configured.Paused || configured.CapabilityID != initial.CapabilityID {
		t.Fatal("writer did not change only project configuration", configured, err)
	}
	if _, err := f.store.DB().Exec(`UPDATE grants SET can_write=0 WHERE credential_id='exec-writer-cred'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.Configure(t.Context(), scope, "owner-config", config)
	attemptCode(t, err, "forbidden")
	_, err = f.e.ConfigurePolicy(t.Context(), scope, "owner-policy", policy)
	attemptCode(t, err, "forbidden")
}
