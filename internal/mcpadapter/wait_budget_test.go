package mcpadapter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestModelWaitBoundsLeaveHostDeadlineMargin(t *testing.T) {
	parsers := map[string]struct {
		parse func(json.RawMessage) (arguments, error)
		input map[string]any
	}{
		"wait_for_messages": {messageWaitArguments, map[string]any{"cursor": "cursor"}},
		"review_operation_read": {func(raw json.RawMessage) (arguments, error) {
			return reviewControlArguments("review_operation_read", raw)
		}, map[string]any{"operation_id": "review_operation_" + strings.Repeat("a", 32)}},
		"review_request": {func(raw json.RawMessage) (arguments, error) {
			return reviewControlArguments("review_request", raw)
		}, map[string]any{"pull_number": 2, "head_sha": strings.Repeat("a", 40), "expected_configuration_revision": 1, "idempotency_key": "request", "channel": "hosted"}},
		"review_source_refresh": {func(raw json.RawMessage) (arguments, error) {
			return reviewControlArguments("review_source_refresh", raw)
		}, map[string]any{"pull_number": 2, "head_sha": strings.Repeat("a", 40), "expected_configuration_revision": 1, "idempotency_key": "source"}},
		"review_checkpoint_publish": {func(raw json.RawMessage) (arguments, error) {
			return checkpointArguments("review_checkpoint_publish", raw)
		}, map[string]any{"evidence_id": "review_evidence_" + strings.Repeat("a", 32), "expected_revision": 0, "idempotency_key": "publish"}},
		"review_checkpoint_recover": {func(raw json.RawMessage) (arguments, error) {
			return checkpointArguments("review_checkpoint_recover", raw)
		}, map[string]any{"publication_id": "review_publication_" + strings.Repeat("a", 32), "expected_revision": 1, "idempotency_key": "recover"}},
	}
	definitions := append([]definition{messageWaitDefinition()}, reviewExecutionDefinitions()...)
	definitions = append(definitions, checkpointDefinitions()...)
	for name, test := range parsers {
		t.Run(name, func(t *testing.T) {
			for _, wait := range []int{50, 51, 60} {
				test.input["wait_seconds"] = wait
				raw, err := json.Marshal(test.input)
				if err != nil {
					t.Fatal(err)
				}
				_, err = test.parse(raw)
				if (err == nil) != (wait == 50) {
					t.Fatalf("wait %d: error=%v; only 50 fits the adapter budget", wait, err)
				}
			}
			found := false
			for _, definition := range definitions {
				if definition.name == name {
					found = true
					properties := definition.schema["properties"].(map[string]any)
					if maximum := properties["wait_seconds"].(map[string]any)["maximum"]; maximum != 50 {
						t.Fatal("discovery advertises unsafe wait maximum", maximum)
					}
				}
			}
			if !found {
				t.Fatal("tool missing from discovery")
			}
		})
	}
	for _, budget := range []time.Duration{client.MessageWaitBudget(50), client.SourceRefreshBudget(50)} {
		if budget > 55*time.Second {
			t.Fatal("adapter wait leaves less than five seconds under a 60-second host deadline", budget)
		}
	}
	if client.ValidateReviewOperationWait(900) != nil {
		t.Fatal("adapter cap narrowed CLI operation waits")
	}
	if client.ValidateMessageWait("cursor", 100, 60) != nil || client.ValidateReviewSourceWait(60) != nil {
		t.Fatal("adapter bound narrowed the CLI/typed-client contract")
	}
}
