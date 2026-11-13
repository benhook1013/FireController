package client_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestPublicationChangesRejectAmbiguousOrImplicitSelections(t *testing.T) {
	good := `{"expected_revision":1,"job_refs":{"upsert":[{"key":"release","job_id":"job_` + strings.Repeat("a", 32) + `","summary_revision":2}]}}`
	if _, err := client.DecodePublicationChanges([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{}`, `{"expected_revision":1}`, `{"expected_revision":0,"description":"text"}`,
		`{"expected_revision":1,"description":null}`, `{"expected_revision":1,"description":"text","enabled":true}`,
		`{"expected_revision":1,"Description":"text"}`, `{"expected_revision":1,"description":"a","description":"b"}`,
		`{"expected_revision":1,"job_refs":{}}`, `{"expected_revision":1,"job_refs":{"remove":null}}`,
		`{"expected_revision":1,"job_refs":{"remove":["missing","missing"]}}`,
		strings.Replace(good, `"summary_revision":2`, `"summary_revision":0`, 1),
		strings.Replace(good, `"summary_revision":2`, `"summary_revision":null`, 1),
		strings.Replace(good, `"summary_revision":2`, `"summary_revision":2,"latest":true`, 1),
		strings.Replace(good, `}]}}`, `}],"remove":["release"]}}`, 1),
		`{"expected_revision":1,"reviews":{"upsert":[{"number":3,"title":"Approved","url":"https://github.com/example/project/pull/4"}]}}`,
		strings.Repeat(" ", client.MaxPublicationChangesBytes) + good,
	} {
		if _, err := client.DecodePublicationChanges([]byte(raw)); err == nil {
			t.Fatal("admitted ambiguous publication edits", raw)
		}
	}
	cleared, err := client.DecodePublicationChanges([]byte(`{"expected_revision":1,"description":"","reviews":{"remove":[3]}}`))
	if err != nil || cleared.Description == nil || *cleared.Description != "" {
		t.Fatal("explicit clear lost", cleared, err)
	}
	raw, _ := json.Marshal(cleared)
	if strings.Contains(string(raw), "job_refs") {
		t.Fatal("omitted collection became edit", string(raw))
	}
}
