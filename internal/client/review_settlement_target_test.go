package client_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

func TestReviewOperationStatusSettlementTargetStrictWire(t *testing.T) {
	good := `{"attempt_revision":3,"repository_id":801,"head_sha":"` + strings.Repeat("a", 40) + `","external_id":"123456789"}`
	cases := []struct {
		name, raw        string
		valid, noAttempt bool
	}{
		{name: "legacy absent", valid: true},
		{name: "current target", raw: good, valid: true},
		{name: "empty CLI external ID", raw: strings.Replace(good, `"123456789"`, `""`, 1), valid: true},
		{name: "null", raw: `null`},
		{name: "missing required fields", raw: `{}`},
		{name: "unknown private field", raw: strings.TrimSuffix(good, `}`) + `,"capture":"PRIVATE_SENTINEL"}`},
		{name: "duplicate", raw: strings.TrimSuffix(good, `}`) + `,"attempt_revision":4}`},
		{name: "alias", raw: strings.Replace(good, `attempt_revision`, `Attempt_revision`, 1)},
		{name: "zero revision", raw: strings.Replace(good, `revision":3`, `revision":0`, 1)},
		{name: "max revision", raw: strings.Replace(good, `revision":3`, `revision":9223372036854775807`, 1)},
		{name: "zero repository", raw: strings.Replace(good, `repository_id":801`, `repository_id":0`, 1)},
		{name: "short head", raw: strings.Replace(good, strings.Repeat("a", 40), "abc", 1)},
		{name: "uppercase head", raw: strings.Replace(good, strings.Repeat("a", 40), strings.Repeat("A", 40), 1)},
		{name: "private external", raw: strings.Replace(good, `123456789`, `/PRIVATE_SENTINEL`, 1)},
		{name: "unpaired Unicode", raw: strings.Replace(good, `123456789`, `\ud800`, 1)},
		{name: "unicode external", raw: strings.Replace(good, `123456789`, `雪`, 1)},
		{name: "oversize external", raw: strings.Replace(good, `123456789`, strings.Repeat("x", 129), 1)},
		{name: "no attempt binding", raw: good, noAttempt: true},
	}
	for _, field := range []string{"attempt_revision", "repository_id", "head_sha", "external_id"} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(good), &fields)
		fields[field] = json.RawMessage(`null`)
		raw, _ := json.Marshal(fields)
		cases = append(cases, struct {
			name, raw        string
			valid, noAttempt bool
		}{name: "null " + field, raw: string(raw)})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := compactStatusFixture()
			if tc.name == "empty CLI external ID" {
				v.Channel = "cli"
				v.Stage = "uncertain"
				v.Reason = "observation_pending"
			}
			if tc.noAttempt {
				v.AttemptID = ""
			}
			data, _ := json.Marshal(v)
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(data, &fields)
			if tc.raw != "" {
				fields["settlement_target"] = json.RawMessage(tc.raw)
			}
			data, _ = json.Marshal(fields)
			reads := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/instance" {
					handshake(w)
					return
				}
				reads++
				if r.Method != "GET" || r.URL.Path != "/api/v1/projects/project-one/reviews/operations/"+executionOperationID+"/status" || r.Header.Get("Authorization") != "Bearer "+testToken {
					t.Error("target read binding changed")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(data)
			}))
			defer s.Close()
			got, err := newClient(t, s).ReviewOperationStatus(t.Context(), executionOperationID)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				out, _ := json.Marshal(got)
				var result map[string]json.RawMessage
				_ = json.Unmarshal(out, &result)
				if tc.raw != "" && string(result["settlement_target"]) != tc.raw {
					t.Fatal("typed settlement guards lost or changed")
				}
			} else {
				var failure *client.Error
				if !errors.As(err, &failure) || failure.Code != "invalid_response" || strings.Contains(failure.Message, "PRIVATE_SENTINEL") {
					t.Fatal("invalid target accepted or leaked")
				}
			}
			if reads != 1 {
				t.Fatal("read retried")
			}
		})
	}
}
