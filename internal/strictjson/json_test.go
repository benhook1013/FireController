package strictjson

import (
	"encoding/json"
	"testing"
)

func TestDecodeRawMessagePreservesValidatedNestedJSON(t *testing.T) {
	for _, raw := range []string{`{ "name" : "Project", "headings": {"jobs":"Tasks"} }`, `[1,{"name":"Project"}]`, `"text"`, `null`} {
		var body struct {
			Value json.RawMessage `json:"value"`
		}
		if err := Decode([]byte(`{"value":`+raw+`}`), &body); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if string(body.Value) != raw {
			t.Fatalf("nested bytes changed: got %s, want %s", body.Value, raw)
		}
	}
	var body struct {
		Value *json.RawMessage `json:"value"`
	}
	if err := Decode([]byte(`{"value":{"name":"Project"}}`), &body); err != nil || body.Value == nil {
		t.Fatalf("pointer to exact RawMessage: %v", err)
	}
}

func TestDecodeRawMessageRetainsWholeDocumentStrictness(t *testing.T) {
	for _, raw := range []string{
		`{"value":{"name":}}`,
		`{"value":{"name":"a","name":"b"}}`,
		`{"value":{"name":"a","\u006eame":"b"}}`,
		`{"value":{"name":"\ud800"}}`,
		`{"value":{"name":"\udc00"}}`,
		`{"Value":{}}`,
		`{"value":{},"extra":true}`,
	} {
		var body struct {
			Value json.RawMessage `json:"value"`
		}
		if err := Decode([]byte(raw), &body); err == nil {
			t.Fatalf("accepted invalid body %s", raw)
		}
	}
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	if err := Decode(append([]byte(`{"value":"`), 0xff, '"', '}'), &body); err == nil {
		t.Fatal("accepted invalid UTF-8 inside RawMessage")
	}
}

func TestDecodeRawMessageDoesNotRelaxOtherSliceTypes(t *testing.T) {
	type rawLike json.RawMessage
	for _, dst := range []any{&struct {
		Value []byte `json:"value"`
	}{}, &struct {
		Value rawLike `json:"value"`
	}{}} {
		if err := Decode([]byte(`{"value":{"name":"Project"}}`), dst); err == nil {
			t.Fatalf("accepted object into non-RawMessage %T", dst)
		}
	}
}
