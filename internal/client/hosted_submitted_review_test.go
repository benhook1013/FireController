package client_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func submittedControlEvidenceObject(t *testing.T) map[string]any {
	t.Helper()
	value := sampleHostedControlEvidence()
	value.Capture.Hosted.PublicationID, value.Capture.Hosted.ProviderAttemptID = "", ""
	value.Capture.Hosted.ReviewIDs = []int64{11}
	value.Capture.Findings, value.Capture.ReportedFindings = value.Capture.Findings[:1], 1
	var object map[string]any
	if err := json.Unmarshal(standingControlJSON(t, value), &object); err != nil {
		t.Fatal(err)
	}
	object["capture"].(map[string]any)["hosted"].(map[string]any)["submitted_review"] = map[string]any{
		"provider_run_id": "33333333-3333-3333-3333-333333333333", "acknowledgement_comment_id": float64(31),
	}
	return object
}

func TestReviewSubmittedEvidenceRetainsRealIdentityThroughHTTP(t *testing.T) {
	object := submittedControlEvidenceObject(t)
	got, err := readControlEvidenceWire(t, standingControlJSON(t, object))
	if err != nil {
		t.Fatal("valid positive submitted review rejected by bound evidence read", err)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(standingControlJSON(t, got), &roundTrip); err != nil || !reflect.DeepEqual(roundTrip, object) {
		t.Fatal("typed HTTP read lost submitted identity, literal finding or source scope", err)
	}
	if !got.Capture.Complete || got.Capture.ReportedFindings != 1 || len(got.Capture.Findings) != 1 || got.Capture.Findings[0].Source.ReviewID != 11 || got.Capture.Hosted.PublicationID != "" || got.Capture.Hosted.ProviderAttemptID != "" {
		t.Fatal("submitted review invented batch identity or lost positive finding")
	}
}

func TestReviewSubmittedEvidenceRefusesConflictingIdentityAndWire(t *testing.T) {
	cases := map[string]func(map[string]any, map[string]any){
		"publication": func(_ map[string]any, h map[string]any) { h["publication_id"] = "11111111-1111-1111-1111-111111111111" },
		"provider attempt": func(_ map[string]any, h map[string]any) {
			h["provider_attempt_id"] = "22222222-2222-2222-2222-222222222222"
		},
		"multiple reviews": func(_ map[string]any, h map[string]any) { h["review_ids"] = []int64{11, 12} },
		"missing review":   func(_ map[string]any, h map[string]any) { h["review_ids"] = []int64{} },
		"zero inventory":   func(c map[string]any, _ map[string]any) { c["findings"], c["reported_findings"] = []any{}, 0 },
		"mixed standing": func(_ map[string]any, h map[string]any) {
			h["standing_summary"] = map[string]any{"comment_id": 32, "provider_run_id": "44444444-4444-4444-4444-444444444444", "updated_at": "2026-10-15T00:00:01Z"}
		},
		"null variant":       func(_ map[string]any, h map[string]any) { h["submitted_review"] = nil },
		"wrong variant type": func(_ map[string]any, h map[string]any) { h["submitted_review"] = "unsupported" },
		"ack is trigger": func(_ map[string]any, h map[string]any) {
			h["submitted_review"].(map[string]any)["acknowledgement_comment_id"] = h["trigger_comment_id"]
		},
		"negative ack": func(_ map[string]any, h map[string]any) {
			h["submitted_review"].(map[string]any)["acknowledgement_comment_id"] = -1
		},
		"fractional ack": func(_ map[string]any, h map[string]any) {
			h["submitted_review"].(map[string]any)["acknowledgement_comment_id"] = 1.5
		},
		"malformed run": func(_ map[string]any, h map[string]any) {
			h["submitted_review"].(map[string]any)["provider_run_id"] = "not-a-run"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			object := submittedControlEvidenceObject(t)
			capture := object["capture"].(map[string]any)
			change(capture, capture["hosted"].(map[string]any))
			got, err := readControlEvidenceWire(t, standingControlJSON(t, object))
			checkError(t, err, "invalid_response")
			if !reflect.DeepEqual(got, client.ReviewEvidence{}) {
				t.Fatal("invalid submitted response exposed partial evidence")
			}
		})
	}
	for _, field := range []string{"provider_run_id", "acknowledgement_comment_id"} {
		for _, mode := range []string{"missing", "null", "alias", "unknown", "duplicate"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				object := submittedControlEvidenceObject(t)
				inner := object["capture"].(map[string]any)["hosted"].(map[string]any)["submitted_review"].(map[string]any)
				old := inner[field]
				switch mode {
				case "missing":
					delete(inner, field)
				case "null":
					inner[field] = nil
				case "alias":
					delete(inner, field)
					inner[strings.ToUpper(field)] = old
				case "unknown":
					inner["unexpected"] = true
				}
				data := standingControlJSON(t, object)
				if mode == "duplicate" {
					key, _ := json.Marshal(field)
					value, _ := json.Marshal(old)
					pair := append(append(key, ':'), value...)
					data = bytes.Replace(data, pair, append(append(append([]byte{}, pair...), ','), pair...), 1)
				}
				_, err := readControlEvidenceWire(t, data)
				checkError(t, err, "invalid_response")
			})
		}
	}
}
