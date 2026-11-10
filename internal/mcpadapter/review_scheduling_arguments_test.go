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
