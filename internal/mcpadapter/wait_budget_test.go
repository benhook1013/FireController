package mcpadapter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/client"
)

func TestModelWaitBoundsAndIdentityBudget(t *testing.T) {
	parsers := map[string]struct {
		parse func(json.RawMessage) (arguments, error)
		input map[string]any
	}{
		"wait_for_messages": {messageWaitArguments, map[string]any{"cursor": "cursor"}},
		"review_start": {func(raw json.RawMessage) (arguments, error) {
			return reviewRunArguments("review_start", raw)
		}, map[string]any{"channel": "hosted", "idempotency_key": "start"}},
		"review_selection_read": {func(raw json.RawMessage) (arguments, error) {
			return reviewRunArguments("review_selection_read", raw)
		}, map[string]any{"selection_id": "review_selection_" + strings.Repeat("a", 32)}},
		"review_operation_read": {func(raw json.RawMessage) (arguments, error) {
			return parseArguments("review_operation_read", raw)
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
		"history_collection_enqueue": {func(raw json.RawMessage) (arguments, error) {
			return parseArguments("history_collection_enqueue", raw)
		}, map[string]any{"pull_numbers": []int{2}, "idempotency_key": "history"}},
		"history_collection_read": {func(raw json.RawMessage) (arguments, error) {
			return parseArguments("history_collection_read", raw)
		}, map[string]any{"collection_id": "history_collection_" + strings.Repeat("a", 32)}},
	}
	definitions := append([]definition{messageWaitDefinition()}, reviewExecutionDefinitions()...)
	definitions = append(definitions, checkpointDefinitions()...)
	definitions = append(definitions, reviewRunDefinitions()...)
	definitions = append(definitions, existingManagementDefinitions()...)
	for name, test := range parsers {
		t.Run(name, func(t *testing.T) {
			for _, wait := range []int{0, 60, 900, 901} {
				test.input["wait_seconds"] = wait
				raw, err := json.Marshal(test.input)
				if err != nil {
					t.Fatal(err)
				}
				_, err = test.parse(raw)
				if (err == nil) != (wait <= maxToolWaitSeconds && (name != "wait_for_messages" || wait > 0)) {
					t.Fatalf("wait %d: error=%v; finite tool wait bounds changed", wait, err)
				}
			}
			found := false
			for _, definition := range definitions {
				if definition.name == name {
					found = true
					properties := definition.schema["properties"].(map[string]any)
					if maximum := properties["wait_seconds"].(map[string]any)["maximum"]; maximum != maxToolWaitSeconds {
						t.Fatal("discovery advertises unsafe wait maximum", maximum)
					}
				}
			}
			if !found {
				t.Fatal("tool missing from discovery")
			}
		})
	}
	for _, name := range []string{"wait_for_messages", "review_start", "review_selection_read", "review_request", "review_operation_read", "review_source_refresh", "review_checkpoint_publish", "review_checkpoint_recover"} {
		args := arguments{messageWaitSeconds: 900, reviewSourceWait: 900}
		if got := toolCallBudget(name, args); got != 915*time.Second {
			t.Fatal("quiet wait lacks identity and response headroom", name, got)
		}
	}
	if got := toolCallBudget("workers_list", arguments{}); got != 15*time.Second {
		t.Fatal("ordinary call budget changed", got)
	}
	for _, name := range []string{"history_collection_enqueue", "history_collection_read"} {
		raw, _ := json.Marshal(map[string]any{"wait_seconds": 900, "collection_id": "history_collection_" + strings.Repeat("a", 32)})
		if name == "history_collection_enqueue" {
			raw, _ = json.Marshal(map[string]any{"wait_seconds": 900, "pull_numbers": []int{2}, "idempotency_key": "history"})
		}
		args, err := parseArguments(name, raw)
		if err != nil || toolCallBudget(name, args) != 915*time.Second {
			t.Fatal("history budget diverged", name, err)
		}
	}
	if client.MessageWaitDefaultSeconds != 300 || client.MessageWaitMaxSeconds != 900 || client.ValidateMessageWait("cursor", 100, 900) != nil || client.ValidateReviewSourceWait(900) != nil {
		t.Fatal("CLI/client wait bounds diverged")
	}
	args, err := messageWaitArguments(json.RawMessage(`{"cursor":"cursor"}`))
	if err != nil || args.messageWaitSeconds != 300 {
		t.Fatal("inbox default changed", args.messageWaitSeconds, err)
	}
}
