// Package strictjson validates the bounded JSON used by local profiles and API requests.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ErrEncoding = errors.New("invalid Unicode encoding")
var ErrSyntax = errors.New("invalid strict JSON")

func Validate(data []byte) error {
	if !utf8.Valid(data) {
		return ErrEncoding
	}
	if !json.Valid(data) {
		return ErrSyntax
	}
	if !pairedUnicodeEscapes(data) {
		return ErrEncoding
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if !uniqueValue(decoder, 0) {
		return ErrSyntax
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrSyntax
	}
	return nil
}

func Decode(data []byte, dst any) error {
	if err := Validate(data); err != nil {
		return err
	}
	if !exactFields(data, reflect.TypeOf(dst)) {
		return ErrSyntax
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(dst) != nil {
		return ErrSyntax
	}
	return nil
}

func exactFields(data []byte, t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			return false
		}
		declared := make(map[string]reflect.Type)
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			declared[name] = f.Type
		}
		for name, raw := range fields {
			field, ok := declared[name]
			if !ok || !exactFields(raw, field) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if json.Unmarshal(data, &values) != nil {
			return false
		}
		for _, raw := range values {
			if !exactFields(raw, t.Elem()) {
				return false
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if json.Unmarshal(data, &values) != nil {
			return false
		}
		for _, raw := range values {
			if !exactFields(raw, t.Elem()) {
				return false
			}
		}
	}
	return true
}

// Validate checks JSON syntax before this original-escape scan.
func pairedUnicodeEscapes(data []byte) bool {
	inString := false
	for i := 0; i < len(data); {
		switch data[i] {
		case '"':
			inString = !inString
			i++
		case '\\':
			if !inString || data[i+1] != 'u' {
				i += 2
				continue
			}
			unit, _ := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
			i += 6
			if unit >= 0xdc00 && unit <= 0xdfff {
				return false
			}
			if unit >= 0xd800 && unit <= 0xdbff {
				if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
					return false
				}
				low, _ := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
				if low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		default:
			i++
		}
	}
	return true
}

func uniqueValue(decoder *json.Decoder, depth int) bool {
	if depth > 64 {
		return false
	}
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		return true
	}
	switch delimiter {
	case '{':
		keys := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return false
			}
			name, ok := key.(string)
			if !ok || keys[name] || !uniqueValue(decoder, depth+1) {
				return false
			}
			keys[name] = true
		}
	case '[':
		for decoder.More() {
			if !uniqueValue(decoder, depth+1) {
				return false
			}
		}
	default:
		return false
	}
	end, err := decoder.Token()
	return err == nil && (delimiter == '{' && end == json.Delim('}') || delimiter == '[' && end == json.Delim(']'))
}
