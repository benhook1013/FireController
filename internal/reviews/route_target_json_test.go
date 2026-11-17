package reviews

import (
	"encoding/json"
	"testing"
)

func TestRouteReceiverRequiresExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{`{"target_pull_number":0}`, true},
		{`{"target_pull_number":4}`, true},
		{`{}`, false},
		{`{"target_pull_number":null}`, false},
		{`{"target_pull_number":0,"target_pull_number":4}`, false},
	} {
		var route FindingRoute
		err := json.Unmarshal([]byte(tc.body), &route)
		if (err == nil) != tc.ok {
			t.Fatalf("%s: %v", tc.body, err)
		}
	}
}
