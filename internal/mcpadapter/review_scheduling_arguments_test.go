package mcpadapter

import (
	"encoding/json"
	"testing"
)

func TestSchedulingOptionalCLIArguments(t *testing.T) {
	for _, value := range []string{"", "true", "false"} {
		raw := `{"expected_revision":0,"automatic_hosted":false,"idempotency_key":"cli-setting"`
		if value != "" {
			raw += `,"automatic_cli":` + value
		}
		args, err := reviewSchedulingArguments("review_scheduling_configure", json.RawMessage(raw+"}"))
		if err != nil || (args.reviewScheduling.AutomaticCLI == nil) != (value == "") {
			t.Fatalf("lost optional CLI state %q: %+v %v", value, args, err)
		}
		if value != "" && *args.reviewScheduling.AutomaticCLI != (value == "true") {
			t.Fatal("wrong CLI state")
		}
	}
	for _, raw := range []string{
		`{"expected_revision":0,"automatic_hosted":false,"automatic_cli":null,"idempotency_key":"key"}`,
		`{"expected_revision":0,"automatic_hosted":false,"automatic_cli":"true","idempotency_key":"key"}`,
		`{"expected_revision":0,"automatic_hosted":false,"automatic_cli":true,"automatic_cli":false,"idempotency_key":"key"}`,
		`{"expected_revision":0,"automatic_cli":true,"idempotency_key":"key"}`,
	} {
		if _, err := reviewSchedulingArguments("review_scheduling_configure", json.RawMessage(raw)); err == nil {
			t.Fatal("accepted malformed CLI configuration", raw)
		}
	}
}

func TestSchedulingPauseReasonIsOmittedOnResume(t *testing.T) {
	for _, raw := range []string{
		`{"pull_number":7,"expected_revision":2,"paused":false,"idempotency_key":"resume"}`,
		`{"pull_number":7,"expected_revision":2,"paused":true,"reason":"","idempotency_key":"pause-empty-reason"}`,
		`{"pull_number":7,"expected_revision":2,"paused":true,"reason":"Fixing","idempotency_key":"pause-reason"}`,
	} {
		if _, err := reviewSchedulingArguments("review_pull_scheduling_update", json.RawMessage(raw)); err != nil {
			t.Fatalf("rejected valid scheduling update %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"pull_number":7,"expected_revision":2,"paused":false,"reason":"no longer allowed","idempotency_key":"resume-reason"}`,
		`{"pull_number":7,"expected_revision":2,"paused":false,"reason":"","idempotency_key":"resume-empty-reason"}`,
		`{"pull_number":7,"expected_revision":2,"paused":false,"reason":null,"idempotency_key":"resume-null-reason"}`,
	} {
		if _, err := reviewSchedulingArguments("review_pull_scheduling_update", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted reason on resume: %s", raw)
		}
	}
}
