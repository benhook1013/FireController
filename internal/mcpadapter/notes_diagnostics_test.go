package mcpadapter

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNoteEncodedArgumentLimitIsIndependentOfBodyBytes(t *testing.T) {
	compact := `{"note_id":"note-one","idempotency_key":"original-key","expected_revision":1,"body":"x"}`
	// Whitespace contributes encoded request bytes without changing the decoded
	// one-byte body or any declared field.
	oversized := json.RawMessage("{" + strings.Repeat(" ", maxNoteArgumentsBytes) + compact[1:])
	_, err := noteArguments("note_update", oversized)
	var d *Diagnostic
	if !errors.As(err, &d) || d.Code != "invalid_arguments" || !strings.Contains(d.Message, "131072 encoded JSON bytes") || !strings.Contains(d.Message, "16384 decoded UTF-8 bytes") || !strings.Contains(d.Action, "No request was sent") {
		t.Fatal(err)
	}
	if details, ok := d.Details.(map[string]any); !ok || details["field"] != "arguments" {
		t.Fatal("payload diagnostic lost its field", d)
	}
	got, err := noteArguments("note_update", json.RawMessage(compact))
	if err != nil || got.notes.update.Body == nil || *got.notes.update.Body != "x" || got.notes.update.Phase != nil || got.notes.update.Status != nil {
		t.Fatal("valid request/omitted fields changed", got, err)
	}
}
