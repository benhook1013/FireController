package reviewprovider

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

// CLIContext identifies the comparison and owned detached workspace selected by
// the runner. The runner separately verifies its immutable Git object identity.
type CLIContext struct {
	BaseBranch       string
	WorkingDirectory string
}

type CLIFinding struct {
	Title, Detail, Severity, Path string
	StartLine, EndLine            int64
}

// CLIReview contains derived plain evidence, never provider instructions or
// diagnostics. Complete describes a valid completed capture, not policy credit.
type CLIReview struct {
	Outcome          string
	Complete         bool
	Findings         []CLIFinding
	ReviewedFiles    []string
	ReportedFindings int64
}

const (
	cliCaptureMax = 16 << 20
	cliLineMax    = 256 << 10
	cliEventsMax  = 4096
	cliFindingMax = 100
)

// ParseCLIReview parses bounded structured JSONL without running commands or
// consulting configuration, the filesystem, credentials or provider state.
func ParseCLIReview(capture []byte, exitCode int, expected CLIContext) (CLIReview, error) {
	result := CLIReview{Outcome: "uncertain", Findings: []CLIFinding{}, ReviewedFiles: []string{}}
	if exitCode != 0 {
		result.Outcome = "failed"
	}
	invalid := func() (CLIReview, error) {
		return CLIReview{}, &Error{Code: "provider_invalid_response", Message: "The CLI capture is not valid structured review evidence."}
	}
	if len(capture) > cliCaptureMax || !utf8.Valid(capture) {
		return invalid()
	}
	contexts, events, terminals := 0, 0, 0
	contextValid, findingsBeforeContext := false, false
	terminal := ""
	for len(capture) > 0 {
		line, rest, _ := bytes.Cut(capture, []byte{'\n'})
		capture = rest
		if len(line) > cliLineMax {
			return invalid()
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		events++
		if events > cliEventsMax || strictjson.Validate(line) != nil {
			return invalid()
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) != nil || fields == nil {
			return invalid()
		}
		text := make(map[string]string)
		for _, key := range []string{"type", "status", "outcome", "state", "errorType", "reviewType", "currentBranch", "baseBranch", "workingDirectory", "title", "comment", "severity", "fileName", "message"} {
			if raw, exists := fields[key]; exists {
				var value string
				if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
					return invalid()
				}
				text[key] = value
			}
		}
		if text["type"] == "" {
			return invalid()
		}
		if raw, exists := fields["recoverable"]; exists {
			var value bool
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
				return invalid()
			}
		}
		numbers := make(map[string]int64)
		for _, key := range []string{"startLine", "endLine", "findings"} {
			if raw, exists := fields[key]; exists {
				var value int64
				if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value < 0 || key == "findings" && value > cliFindingMax || key != "findings" && value == 0 {
					return invalid()
				}
				numbers[key] = value
			}
		}
		var files []string
		if raw, exists := fields["reviewedFiles"]; exists {
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &files) != nil || files == nil {
				return invalid()
			}
			seen := make(map[string]bool)
			for _, file := range files {
				if !cliPortablePath(file, false) || seen[file] {
					return invalid()
				}
				seen[file] = true
			}
		}
		switch text["type"] {
		case "review_context":
			contexts++
			if contexts > 1 || terminals != 0 || findingsBeforeContext {
				return invalid()
			}
			contextValid = cliProse(expected.BaseBranch, cliLineMax, true) && cliProse(expected.WorkingDirectory, cliLineMax, true) && text["reviewType"] == "committed" && text["currentBranch"] == "HEAD" && text["baseBranch"] == expected.BaseBranch && text["workingDirectory"] == expected.WorkingDirectory
		case "finding":
			if terminals != 0 || len(result.Findings) == cliFindingMax {
				return invalid()
			}
			if contexts == 0 {
				findingsBeforeContext = true
			}
			if _, exists := fields["comment"]; !exists || !cliProse(text["title"], 500, true) || !cliProse(text["comment"], 16<<10, false) || !cliProse(text["severity"], cliLineMax, false) || !cliPortablePath(text["fileName"], true) || numbers["endLine"] != 0 && numbers["startLine"] > numbers["endLine"] {
				return invalid()
			}
			result.Findings = append(result.Findings, CLIFinding{Title: text["title"], Detail: text["comment"], Severity: cliSeverity(text["severity"]), Path: text["fileName"], StartLine: numbers["startLine"], EndLine: numbers["endLine"]})
		case "complete", "error", "rate_limit", "rate_limited":
			terminals++
			if terminals != 1 {
				return invalid()
			}
			var valid bool
			terminal, valid = cliTerminal(text)
			if !valid {
				return invalid()
			}
			if terminal == "completed" {
				_, countPresent := fields["findings"]
				_, filesPresent := fields["reviewedFiles"]
				if exitCode == 0 && (contexts != 1 || !contextValid || findingsBeforeContext) || !countPresent || numbers["findings"] != int64(len(result.Findings)) || !filesPresent {
					return invalid()
				}
				result.ReportedFindings, result.ReviewedFiles = numbers["findings"], files
			}
		default:
			// Heartbeats, progress, status and unknown metadata cannot establish
			// completion, even if their prose or fields mention success.
		}
	}
	if terminal != "" && terminal != "completed" {
		result.Outcome = terminal
	} else if terminal == "completed" && exitCode == 0 {
		result.Outcome, result.Complete = "completed", true
	}
	return result, nil
}

func cliTerminal(fields map[string]string) (string, bool) {
	var outcome string
	switch fields["type"] {
	case "rate_limit", "rate_limited":
		outcome = "rate_limited"
	case "error":
		outcome = cliTerminalState(fields["errorType"])
		if outcome == "" || outcome == "completed" {
			outcome = "failed"
		}
	}
	for _, key := range []string{"status", "outcome", "state", "errorType"} {
		value := cliTerminalState(fields[key])
		if value == "" {
			continue
		}
		if outcome != "" && outcome != value {
			return "", false
		}
		outcome = value
	}
	if outcome == "completed" && (fields["type"] != "complete" || fields["status"] != "review_completed" || fields["outcome"] != "completed") {
		return "", false
	}
	if outcome == "" {
		outcome = "uncertain"
	}
	return outcome, true
}

func cliTerminalState(value string) string {
	switch value {
	case "completed", "review_completed":
		return "completed"
	case "failed", "review_failed":
		return "failed"
	case "cancelled", "canceled":
		return "cancelled"
	case "rate_limit", "rate_limited":
		return "rate_limited"
	case "uncertain", "timeout":
		return "uncertain"
	}
	return ""
}

func cliProse(value string, max int, required bool) bool {
	if len(value) > max || !utf8.ValidString(value) || required && strings.TrimSpace(value) == "" {
		return false
	}
	return strings.IndexFunc(value, func(c rune) bool { return c < 0x20 && c != '\t' && c != '\r' && c != '\n' || c == 0x7f }) < 0
}

func cliPortablePath(value string, empty bool) bool {
	if value == "" {
		return empty
	}
	if len(value) > 1024 || !utf8.ValidString(value) || strings.ContainsAny(value, "\\:") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func cliSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "trivial", "info":
		return "info"
	case "minor", "low":
		return "low"
	case "medium":
		return "medium"
	case "major", "high":
		return "high"
	case "critical":
		return "critical"
	}
	return "unknown"
}
