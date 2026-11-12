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
	text := definition.description + string(data)
	if strings.Count(text, "observed_base_sha as base_sha") < 2 || !strings.Contains(text, "Every response ends the subscription") {
		t.Fatal("discovery omits the implicit-base rearm pin or returned-wait boundary", text)
	}
}
