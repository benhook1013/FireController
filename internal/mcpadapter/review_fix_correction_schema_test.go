package mcpadapter

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

func TestReviewFixCorrectionCatalogParityProjectAugmentationAndBudget(t *testing.T) {
	const name = "review_finding_correct_fix"
	stdio := multiProjectUnitCatalog(t, multiProjectUnitSession(t, boundServer(profile.Binding{})))
	remote := multiProjectUnitCatalog(t, multiProjectUnitSession(t, toolServer(profile.Binding{}, &remoteMCP{})))
	multi := multiProjectUnitCatalog(t, multiProjectUnitSession(t, connectorToolServer(profile.Binding{}, &remoteMCP{}, true)))
	if len(stdio) != len(remote) || len(multi) != len(stdio)+1 || stdio[name] == nil || remote[name] == nil || multi[name] == nil {
		t.Fatal("correction is not present at transport catalog parity")
	}
	schema := multiProjectUnitSchema(t, stdio[name])
	if !reflect.DeepEqual(schema, multiProjectUnitSchema(t, remote[name])) {
		t.Fatal("stdio and remote correction schemas differ")
	}
	props := schema["properties"].(map[string]any)
	sha := props["expected_sha"].(map[string]any)
	if len(props) != 6 || len(schema["required"].([]any)) != 6 || schema["additionalProperties"] != false || sha["minLength"] != float64(40) || sha["maxLength"] != float64(40) || sha["pattern"] != "^[a-f0-9]{40}$" {
		t.Fatal("correction schema weakened strict required guards")
	}
	selected := multiProjectUnitSchema(t, multi[name])
	if len(selected["properties"].(map[string]any)) != 7 || len(selected["required"].([]any)) != 7 || selected["required"].([]any)[6] != "project" || selected["additionalProperties"] != false {
		t.Fatal("correction bypassed existing remote project schema augmentation")
	}
	if !mutationTool(name) || toolCallBudget(name, arguments{}) != 15*time.Second {
		t.Fatal("correction lost mutating classification or existing bootstrap/command budget")
	}
	if !strings.Contains(stdio[name].Description, "original frozen counts") {
		t.Fatal("correction discovery lost frozen-count boundary")
	}
}
