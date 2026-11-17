package client

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

func completeShape(data []byte, dst any) bool { return requiredShape(data, reflect.TypeOf(dst)) }
func requiredShape(data []byte, t reflect.Type) bool {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return false
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if json.Unmarshal(data, &object) != nil {
			return false
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.PkgPath != "" || field.Tag.Get("json") == "-" {
				continue
			}
			tag := strings.Split(field.Tag.Get("json"), ",")
			raw, present := object[tag[0]]
			// Existing job pages omit the terminal cursor; the typed result still
			// emits the common cursor field and validates nonterminal cursors.
			optional := field.Name == "Cursor" || len(tag) > 1 && tag[1] == "omitempty"
			if !present {
				if optional {
					continue
				}
				return false
			}
			if !requiredShape(raw, field.Type) {
				return false
			}
		}
	case reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return false
		}
		for _, raw := range items {
			if !requiredShape(raw, t.Elem()) {
				return false
			}
		}
	}
	return true
}

func (c *Client) hasSensitive(value any) bool { return c.sensitiveValue(reflect.ValueOf(value)) }
func (c *Client) sensitiveValue(value reflect.Value) bool {
	if !value.IsValid() {
		return false
	}
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return false
		}
		return c.sensitiveValue(value.Elem())
	case reflect.String:
		return c.sensitive(value.String())
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if c.sensitiveValue(value.Field(i)) {
				return true
			}
		}
	case reflect.Slice:
		for i := 0; i < value.Len(); i++ {
			if c.sensitiveValue(value.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			if c.sensitiveValue(iter.Key()) || c.sensitiveValue(iter.Value()) {
				return true
			}
		}
	}
	return false
}
