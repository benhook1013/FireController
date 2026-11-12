package mcpadapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCIWatchDescriptionPinsReturnedBaseAndEndsSubscription(t *testing.T) {
	// Marshal the public definition so both the tool description and input
	// field guidance are checked without requiring credentials or a server.
	definition := ciWatchDefinition()
	data, err := json.Marshal(definition.schema)
	if err != nil {
		t.Fatal(err)
	}
	const rearm = "observed_base_sha as base_sha"
	if !strings.Contains(definition.description, rearm) || !strings.Contains(string(data), rearm) || !strings.Contains(definition.description, "Every response ends the subscription") {
		t.Fatal("discovery omits the implicit-base rearm pin or returned-wait boundary", definition.description, string(data))
	}
}
