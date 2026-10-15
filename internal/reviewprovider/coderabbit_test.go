package reviewprovider

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

var syntheticCLIContext = CLIContext{BaseBranch: "origin/main", WorkingDirectory: "/owned/synthetic-workspace"}

func cliJSONLines(t *testing.T, events ...any) []byte {
	t.Helper()
	var capture bytes.Buffer
	for _, event := range events {
		if err := json.NewEncoder(&capture).Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	return capture.Bytes()
}

func cliContextEvent() map[string]any {
	return map[string]any{"type": "review_context", "reviewType": "committed", "currentBranch": "HEAD", "baseBranch": syntheticCLIContext.BaseBranch, "workingDirectory": syntheticCLIContext.WorkingDirectory}
}

func cliFindingEvent() map[string]any {
	return map[string]any{"type": "finding", "title": "  Synthetic finding 🔥 �  ", "comment": "Exact prose\n\t<untrusted> & literal \\uD800", "severity": "major", "fileName": "src/example.go", "startLine": 3, "endLine": 5, "codegenInstructions": map[string]any{"command": "UNTRUSTED_INSTRUCTION"}, "suggestions": []string{"UNTRUSTED_SUGGESTION"}}
}

func cliCompleteEvent(count int) map[string]any {
	return map[string]any{"type": "complete", "status": "review_completed", "outcome": "completed", "findings": count, "reviewedFiles": []string{"src/example.go"}}
}

func requireCLIInvalid(t *testing.T, capture []byte, exit int, expected CLIContext) {
	t.Helper()
	result, err := ParseCLIReview(capture, exit, expected)
	var failure *Error
	if !errors.As(err, &failure) || failure.Code != "provider_invalid_response" || result.Complete {
		t.Fatalf("invalid evidence credited or wrong error: complete=%t err=%v", result.Complete, err)
	}
	for _, secret := range []string{"PRIVATE_DIAGNOSTIC", "private-token", "/owned/"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("parser reflected private capture or context")
		}
	}
}

func TestCLIReviewExactCompletedEvidenceAndPlainFindings(t *testing.T) {
	finding := cliFindingEvent()
	capture := cliJSONLines(t,
		map[string]any{"type": "heartbeat", "message": "PRIVATE_DIAGNOSTIC"}, cliContextEvent(),
		map[string]any{"type": "progress", "status": "review_completed", "outcome": "completed"},
		finding, cliCompleteEvent(1), map[string]any{"type": "future_metadata", "nested": map[string]any{"value": true}},
	)
	result, err := ParseCLIReview(capture, 0, syntheticCLIContext)
	if err != nil || !result.Complete || result.Outcome != "completed" || result.ReportedFindings != 1 || len(result.Findings) != 1 || len(result.ReviewedFiles) != 1 {
		t.Fatalf("valid capture: %+v %v", result, err)
	}
	got := result.Findings[0]
	if got.Title != finding["title"] || got.Detail != finding["comment"] || got.Path != finding["fileName"] || got.Severity != "high" || got.StartLine != 3 || got.EndLine != 5 {
		t.Fatal("plain source prose/path/line identity changed")
	}
	encoded, _ := json.Marshal(result)
	for _, excluded := range []string{"UNTRUSTED_INSTRUCTION", "UNTRUSTED_SUGGESTION", "PRIVATE_DIAGNOSTIC", syntheticCLIContext.WorkingDirectory} {
		if bytes.Contains(encoded, []byte(excluded)) {
			t.Fatal("provider instructions or diagnostics escaped derived plain evidence")
		}
	}
	// Files are source observations, not reconstructed from finding locations.
	complete := cliCompleteEvent(1)
	complete["reviewedFiles"] = []string{}
	result, err = ParseCLIReview(cliJSONLines(t, cliContextEvent(), finding, complete), 0, syntheticCLIContext)
	if err != nil || !result.Complete || len(result.ReviewedFiles) != 0 {
		t.Fatal("reviewed file membership was inferred from findings")
	}
}

func TestCLIReviewSeverityAndOptionalLocations(t *testing.T) {
	for source, expected := range map[string]string{"trivial": "info", "minor": "low", "major": "high", "critical": "critical", "info": "info", "low": "low", "medium": "medium", "high": "high", "new-provider-level": "unknown"} {
		t.Run(source, func(t *testing.T) {
			finding := map[string]any{"type": "finding", "title": "Title", "comment": "", "severity": source}
			result, err := ParseCLIReview(cliJSONLines(t, cliContextEvent(), finding, cliCompleteEvent(1)), 0, syntheticCLIContext)
			if err != nil || !result.Complete || result.Findings[0].Severity != expected || result.Findings[0].Path != "" || result.Findings[0].StartLine != 0 || result.Findings[0].EndLine != 0 {
				t.Fatalf("normalized level or unknown location: %+v %v", result, err)
			}
		})
	}
	complete := cliCompleteEvent(0)
	complete["reviewedFiles"] = []string{}
	result, err := ParseCLIReview(cliJSONLines(t, cliContextEvent(), complete), 0, syntheticCLIContext)
	if err != nil || !result.Complete || result.Findings == nil || result.ReviewedFiles == nil {
		t.Fatal("explicit empty completed capture was not preserved")
	}
}

func TestCLIReviewNoncreditOutcomesRetainBoundedEvidence(t *testing.T) {
	for _, test := range []struct {
		name    string
		event   any
		exit    int
		outcome string
	}{
		{"exit-zero-only", nil, 0, "uncertain"}, {"nonzero-partial", nil, 2, "failed"},
		{"actual-rate-shape", map[string]any{"type": "error", "errorType": "rate_limit", "message": "PRIVATE_DIAGNOSTIC", "recoverable": true, "details": map[string]any{}, "metadata": map[string]any{"waitTime": "1 minute", "policyGuidance": "untrusted"}}, 1, "rate_limited"},
		{"rate-type", map[string]any{"type": "rate_limited"}, 0, "rate_limited"},
		{"cancelled", map[string]any{"type": "error", "errorType": "canceled"}, 1, "cancelled"},
		{"timeout", map[string]any{"type": "error", "errorType": "timeout"}, 1, "uncertain"},
		{"other-error", map[string]any{"type": "error", "errorType": "provider_failure"}, 0, "failed"},
		{"failed-complete", map[string]any{"type": "complete", "status": "review_failed", "outcome": "failed"}, 0, "failed"},
		{"cancelled-complete", map[string]any{"type": "complete", "outcome": "cancelled"}, 0, "cancelled"},
		{"rate-complete", map[string]any{"type": "complete", "state": "rate_limit"}, 0, "rate_limited"},
		{"unknown-complete", map[string]any{"type": "complete", "status": "new_status", "outcome": "new_outcome"}, 0, "uncertain"},
		{"nonzero-with-completed-event", cliCompleteEvent(1), 3, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []any{cliFindingEvent()}
			if test.event != nil {
				events = append(events, test.event)
			}
			result, err := ParseCLIReview(cliJSONLines(t, events...), test.exit, CLIContext{})
			if err != nil || result.Complete || result.Outcome != test.outcome || len(result.Findings) != 1 {
				t.Fatalf("partial evidence/outcome: %+v %v", result, err)
			}
		})
	}
	for _, kind := range []string{"heartbeat", "status", "progress", "unknown_metadata"} {
		result, err := ParseCLIReview(cliJSONLines(t, map[string]any{"type": kind, "status": "review_completed", "outcome": "completed"}), 0, syntheticCLIContext)
		if err != nil || result.Complete || result.Outcome != "uncertain" {
			t.Fatal("metadata granted completion")
		}
	}
}

func TestCLIReviewRequiresExactContextAndOneConsistentCompletion(t *testing.T) {
	for _, field := range []string{"reviewType", "currentBranch", "baseBranch", "workingDirectory"} {
		context := cliContextEvent()
		context[field] = "mismatched"
		requireCLIInvalid(t, cliJSONLines(t, context, cliCompleteEvent(0)), 0, syntheticCLIContext)
	}
	requireCLIInvalid(t, cliJSONLines(t, cliContextEvent(), cliCompleteEvent(0)), 0, CLIContext{})
	for _, events := range [][]any{
		{cliCompleteEvent(0)}, {cliContextEvent(), cliContextEvent(), cliCompleteEvent(0)},
		{cliFindingEvent(), cliContextEvent(), cliCompleteEvent(1)},
		{cliContextEvent(), cliCompleteEvent(0), cliFindingEvent()},
		{cliContextEvent(), cliCompleteEvent(0), cliCompleteEvent(0)},
		{cliContextEvent(), cliCompleteEvent(0), map[string]any{"type": "error", "errorType": "rate_limit"}},
		{map[string]any{"type": "rate_limit", "outcome": "failed"}},
		{map[string]any{"type": "complete", "status": "review_completed", "outcome": "failed"}},
		{map[string]any{"type": "complete", "status": "review_failed", "outcome": "cancelled"}},
		{cliContextEvent(), map[string]any{"type": "complete", "status": "review_completed", "findings": 0, "reviewedFiles": []string{}}},
		{cliContextEvent(), map[string]any{"type": "complete", "outcome": "completed", "findings": 0, "reviewedFiles": []string{}}},
	} {
		requireCLIInvalid(t, cliJSONLines(t, events...), 0, syntheticCLIContext)
	}
	for _, field := range []string{"findings", "reviewedFiles"} {
		complete := cliCompleteEvent(0)
		delete(complete, field)
		requireCLIInvalid(t, cliJSONLines(t, cliContextEvent(), complete), 0, syntheticCLIContext)
	}
	complete := cliCompleteEvent(1)
	requireCLIInvalid(t, cliJSONLines(t, cliContextEvent(), complete), 0, syntheticCLIContext)
}

func TestCLIReviewStrictJSONUnicodeAndKnownTypes(t *testing.T) {
	for _, raw := range []string{
		`{"type":"finding","title":"Title","comment":"truncated"`,
		`{"type":"status","type":"complete"}`, `{"type":"status","metadata":{"x":1,"x":2}}`,
		`{"type":"finding","title":"bad\ud800","comment":"detail"}`, `{"type":"status","metadata":"bad\udc00"}`,
		`{"type":"finding","title":"Title","comment":"bad\u0000"}`, `{"type":"finding","title":"bad\u007f","comment":"detail"}`,
		`{"type":"finding","title":null,"comment":"detail"}`, `{"type":"finding","title":"Title","comment":42}`,
		`{"type":"finding","title":"Title"}`, `{"type":"finding","title":"Title","comment":"detail","startLine":0}`,
		`{"type":"finding","title":"Title","comment":"detail","endLine":null}`, `{"type":"finding","title":"Title","comment":"detail","startLine":1.5}`,
		`{"type":"finding","title":"Title","comment":"detail","startLine":9223372036854775808}`,
		`{"type":"complete","findings":1e999}`, `{"type":"complete","findings":-1}`, `{"type":"complete","findings":101}`,
		`{"type":"complete","reviewedFiles":null}`, `{"type":"complete","reviewedFiles":[null]}`,
		`{"type":"error","errorType":42}`, `{"type":"error","recoverable":null}`, `{"type":null}`, `[]`, `null`,
	} {
		requireCLIInvalid(t, []byte(raw), 0, syntheticCLIContext)
	}
	requireCLIInvalid(t, []byte("{\"type\":\"status\",\"message\":\"\xffPRIVATE_DIAGNOSTIC\"}"), 0, syntheticCLIContext)
	result, err := ParseCLIReview([]byte(" \t\r\n{\"type\":\"finding\",\"title\":\"Valid \\ud83d\\udd25 �\",\"comment\":\"literal \\\\uD800\"}\n"), 0, syntheticCLIContext)
	if err != nil || len(result.Findings) != 1 || result.Findings[0].Title != "Valid 🔥 �" || result.Complete {
		t.Fatal("valid Unicode or literal escape changed")
	}
}

func TestCLIReviewFindingAndFileBounds(t *testing.T) {
	for _, path := range []string{"/absolute.go", "../outside.go", "src/../outside.go", "./relative.go", "C:/private.go", `src\file.go`, "src//file.go", "bad\nfile.go", strings.Repeat("x", 1025)} {
		finding := cliFindingEvent()
		finding["fileName"] = path
		requireCLIInvalid(t, cliJSONLines(t, finding), 0, syntheticCLIContext)
		complete := cliCompleteEvent(0)
		complete["reviewedFiles"] = []string{path}
		requireCLIInvalid(t, cliJSONLines(t, cliContextEvent(), complete), 0, syntheticCLIContext)
	}
	for key, value := range map[string]any{"title": strings.Repeat("x", 501), "comment": strings.Repeat("🔥", (16<<10)/4+1), "startLine": 6, "severity": "bad\x00level"} {
		finding := cliFindingEvent()
		finding[key] = value
		requireCLIInvalid(t, cliJSONLines(t, finding), 0, syntheticCLIContext)
	}
	complete := cliCompleteEvent(0)
	complete["reviewedFiles"] = []string{"src/example.go", "src/example.go"}
	requireCLIInvalid(t, cliJSONLines(t, cliContextEvent(), complete), 0, syntheticCLIContext)
	var events []any
	events = append(events, cliContextEvent())
	for i := 0; i < 100; i++ {
		events = append(events, cliFindingEvent())
	}
	completed := append(append([]any{}, events...), cliCompleteEvent(100))
	result, err := ParseCLIReview(cliJSONLines(t, completed...), 0, syntheticCLIContext)
	if err != nil || !result.Complete || len(result.Findings) != 100 {
		t.Fatal("maximum findings not accepted")
	}
	events = append(events, cliFindingEvent())
	requireCLIInvalid(t, cliJSONLines(t, events...), 0, syntheticCLIContext)
}

func TestCLIReviewCaptureLineAndEventBounds(t *testing.T) {
	requireCLIInvalid(t, bytes.Repeat([]byte{' '}, cliCaptureMax+1), 0, syntheticCLIContext)
	line := []byte(`{"type":"heartbeat","padding":"` + strings.Repeat("x", cliLineMax-len(`{"type":"heartbeat","padding":""}`)) + `"}`)
	if len(line) != cliLineMax {
		t.Fatal("test line size")
	}
	if result, err := ParseCLIReview(line, 0, syntheticCLIContext); err != nil || result.Complete {
		t.Fatal("exact maximum line rejected or credited")
	}
	requireCLIInvalid(t, append(line, ' '), 0, syntheticCLIContext)
	event := []byte("{\"type\":\"heartbeat\"}\n")
	if result, err := ParseCLIReview(bytes.Repeat(event, cliEventsMax), 0, syntheticCLIContext); err != nil || result.Complete {
		t.Fatal("maximum metadata events rejected or credited")
	}
	requireCLIInvalid(t, bytes.Repeat(event, cliEventsMax+1), 0, syntheticCLIContext)
	for i := 0; i < 2; i++ {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			capture := []byte("{\"type\":\"status\",\"message\":\"PRIVATE_DIAGNOSTIC private-token /owned/path\"}\n")
			if i == 1 {
				capture = append(capture, []byte("not JSON")...)
			}
			result, err := ParseCLIReview(capture, 0, syntheticCLIContext)
			if result.Complete || i == 0 && err != nil || i == 1 && err == nil {
				t.Fatal("incomplete/malformed result")
			}
		})
	}
}
