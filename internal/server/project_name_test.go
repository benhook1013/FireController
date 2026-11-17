package server_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProjectNameRejectsControlsBeforeProviderAndWrites(t *testing.T) {
	for label, name := range map[string]string{
		"LF": "One\nTwo", "tab": "One\tTwo", "C0": "One\x01Two", "NUL": "One\x00Two", "DEL": "One\x7fTwo", "C1": "One\u0085Two",
		"leading_space": " One", "trailing_space": "One ", "empty": "", "over_200_bytes": strings.Repeat("x", 201),
	} {
		t.Run(label, func(t *testing.T) {
			f := setup(t)
			body, err := json.Marshal(map[string]string{"name": name, "repository": "owner/one"})
			if err != nil {
				t.Fatal(err)
			}
			beforeProjects, beforeEvents, beforeReceipts := count(t, f.store, "projects"), count(t, f.store, "events"), count(t, f.store, "receipts")
			w := request(f.handler, "POST", "/api/v1/projects", f.admin, "invalid-name", "", string(body))
			result := require(t, w, 400)
			failure, ok := result["error"].(map[string]any)
			if !ok || failure["code"] != "validation_failed" {
				t.Fatal("incorrect name refusal", result)
			}
			if f.calls.Load() != 0 || count(t, f.store, "projects") != beforeProjects || count(t, f.store, "events") != beforeEvents || count(t, f.store, "receipts") != beforeReceipts {
				t.Fatal("invalid name reached provider or mutated state")
			}
		})
	}
}

func TestProjectNameAcceptsBoundedTrimmedOrdinaryAndUnicode(t *testing.T) {
	for i, name := range []string{"Ordinary project", "雪の作業場", strings.Repeat("é", 100)} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			f := setup(t)
			body, err := json.Marshal(map[string]string{"name": name, "repository": "owner/one"})
			if err != nil {
				t.Fatal(err)
			}
			result := require(t, request(f.handler, "POST", "/api/v1/projects", f.admin, "valid-name", "", string(body)), 200)
			id, ok := result["id"].(string)
			if !ok || id == "" {
				t.Fatal("missing project receipt")
			}
			read := require(t, request(f.handler, "GET", "/api/v1/projects/"+id, f.admin, "", "", ""), 200)
			if read["name"] != name || f.calls.Load() != 1 || count(t, f.store, "projects") != 1 || count(t, f.store, "events") != 1 || count(t, f.store, "receipts") != 1 {
				t.Fatal("valid name changed or effects duplicated", read)
			}
		})
	}
}
