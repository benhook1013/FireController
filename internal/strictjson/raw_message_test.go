package strictjson

import (
	"encoding/json"
	"testing"
)

func TestRawMessageRetainsStrictNestedJSON(t *testing.T) {
	type envelope struct {
		Facts json.RawMessage `json:"facts"`
	}
	for _, value := range []string{`{"ID":47,"Title":"Literal <source>"}`, `[{"ID":201}]`, `"source"`, `123`, `true`, `null`} {
		var out envelope
		if err := Decode([]byte(`{"facts":`+value+`}`), &out); err != nil || string(out.Facts) != value {
			t.Fatalf("valid raw JSON refused or changed: %v", err)
		}
	}
	for _, body := range []string{
		`{"facts":{"ID":47,"ID":48}}`,
		`{"facts":[{"ID":47,"ID":48}]}`,
		`{"facts":{"text":"\ud800"}}`,
		`{"facts":{},"Facts":{}}`,
		`{"facts":{},"extra":{}}`,
	} {
		var out envelope
		if Decode([]byte(body), &out) == nil {
			t.Fatal("raw JSON bypassed strict encoding or envelope validation")
		}
	}
}
