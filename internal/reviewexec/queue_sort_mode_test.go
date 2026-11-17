package reviewexec

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestQueueSortModeLegacyAutoManualExactSavedOrder(t *testing.T) {
	if queueSortMode(Configuration{}) != "auto" {
		t.Fatal("legacy must default Auto")
	}
	c := Configuration{Queue: []int64{9, 2, 1}, QueueSortMode: "manual"}
	got, err := orderedConfiguredQueue(t.Context(), nil, "project", c)
	if err != nil || !slices.Equal(got, c.Queue) {
		t.Fatal(got, err)
	}
	got[0] = 99
	if c.Queue[0] != 9 {
		t.Fatal("manual mutated saved queue")
	}
	c.QueueSortMode = "unexpected"
	if _, err = orderedConfiguredQueue(t.Context(), nil, "project", c); err == nil {
		t.Fatal("unknown mode accepted")
	}
}
func TestQueueSortModeChangesInvalidateSourceConfiguration(t *testing.T) {
	a := Configuration{Queue: []int64{1, 2}}
	b := a
	b.QueueSortMode = "auto"
	if !sameSourceConfiguration(a, b) || !sameSourceTargetConfiguration(a, b, 2, 1) {
		t.Fatal("absent and explicit Auto differ")
	}
	b.QueueSortMode = "manual"
	if sameSourceConfiguration(a, b) || sameSourceTargetConfiguration(a, b, 2, 1) {
		t.Fatal("mode toggle hidden from queued source validation")
	}
}

func TestQueueSortModeLegacyConfigurationUpdatePreservesManual(t *testing.T) {
	f := newAttemptFixture(t)
	f.e.queueSortingWrites = true
	f.configure(t, attemptProject, "test-capability", 0, false)
	c := Configuration{ExpectedRevision: new(int64(1)), CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, QueueSortMode: "manual"}
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "manual", c); err != nil {
		t.Fatal(err)
	}
	c.ExpectedRevision = new(int64(2))
	c.QueueSortMode = ""
	c.Paused = true
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "legacy-pause", c); err != nil {
		t.Fatal(err)
	}
	got, err := f.e.ReadExecutionConfiguration(t.Context(), f.scope(t, attemptProject, f.writer, false))
	if err != nil || got.QueueSortMode != "manual" || !got.Paused {
		t.Fatal(got, err)
	}
}

func TestQueueSortWriteGatePreservesLegacyRetainedBytes(t *testing.T) {
	f := newAttemptFixture(t)
	f.configure(t, attemptProject, "test-capability", 0, false)
	read := func() string {
		var body string
		if err := f.store.DB().QueryRow(`SELECT body_json FROM review_execution_config WHERE project_id=? ORDER BY revision DESC LIMIT 1`, attemptProject).Scan(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	before := read()
	for _, mode := range []string{"auto", "manual"} {
		c := Configuration{ExpectedRevision: new(int64(1)), CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, QueueSortMode: mode}
		if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "disabled-"+mode, c); err == nil {
			t.Fatal("disabled writer accepted", mode)
		}
		if read() != before {
			t.Fatal("disabled write changed retained bytes")
		}
	}
	c := Configuration{ExpectedRevision: new(int64(1)), CapabilityID: "test-capability", Mode: "independent", Queue: []int64{2, 4}, Channels: []string{"cli", "hosted"}, Paused: true}
	if _, err := f.e.Configure(t.Context(), f.scope(t, attemptProject, f.admin, true), "legacy-pause-off", c); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(read()), &fields); err != nil {
		t.Fatal(err)
	}
	if _, found := fields["queue_sort_mode"]; found {
		t.Fatal("legacy update serialized new field")
	}
}
