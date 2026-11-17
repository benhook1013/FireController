package reviewprovider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestHostedDecoratedSeverityNativeAndRetainedDisplay(t *testing.T) {
	for label, want := range map[string]string{"Trivial": "info", "Minor": "low", "Major": "high", "Critical": "critical"} {
		for _, dark := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_dark_%t", label, dark), func(t *testing.T) {
				icon := standingTestPicture(dark)
				inline := "_Category_ | **" + icon + " " + label + "** | _Quick win_\n\n**Keep the original finding.**\n\nLiteral source detail & Ω.\n\n" + hfMarker(1)
				outside := hfOutside(2, "engine/queue.go", "12-15", "Keep the outside finding.")
				outside = strings.Replace(outside, "<em>🟡 Minor</em>", "<strong>"+icon+" "+label+"</strong>", 1)
				outside = strings.Replace(outside, "_🟡 Minor_", "_"+icon+" "+label+"_", 1)
				f := hfNew(t, []string{inline}, hfOutsideCategory(1, outside))
				before, _ := json.Marshal(f.snapshot)
				got := hfWant(t, f, true, 2)
				for _, finding := range got.Findings {
					if finding.Severity != want || HostedDisplaySeverity(finding.Detail) != want {
						t.Fatal("explicit decorated metadata lost severity", finding.Kind, finding.Severity)
					}
					// Existing unknown captures retain their original stored value;
					// the helper supplies presentation metadata only.
					retained := finding
					retained.Severity = "unknown"
					if HostedDisplaySeverity(retained.Detail) != want || retained.Severity != "unknown" || retained.Detail != finding.Detail {
						t.Fatal("display derivation changed retained source")
					}
				}
				after, _ := json.Marshal(f.snapshot)
				if string(before) != string(after) || got.Findings[0].Detail != inline {
					t.Fatal("severity interpretation rewrote original source bytes")
				}
			})
		}
	}
}

func TestHostedDisplaySeverityMetadataScopeAndConflicts(t *testing.T) {
	icon := standingTestPicture(true)
	major := "_Category_ | _" + icon + " Major_"
	minor := "_Category_ | **" + icon + " Minor**"
	prompt := "<details>\n<summary>🤖 Prompt for AI Agents</summary>\n" + minor + "\n</details>"
	outside := strings.Replace(hfOutside(1, "engine/queue.go", "12-15", "Keep the record."), "_🟡 Minor_", "_"+icon+" Major_", 1)
	for _, test := range []struct {
		name, detail, want string
	}{
		{"emoji_compatibility", "_Category_ | _🟠 Major_\n\n**Actual title.**", "high"},
		{"bare_wrapped_label", "_Major_", "high"},
		{"bold_wrapped_label", "**Critical**", "critical"},
		{"crlf", strings.ReplaceAll(major+"\n\n**Actual title.**", "\n", "\r\n"), "high"},
		{"narrative", "A Major defect could happen here.", "unknown"},
		{"plain_label", "Major", "unknown"},
		{"title_then_metadata_example", "**Actual title.**\n\n" + major, "unknown"},
		{"metadata_then_narrative_example", major + "\n\n**Actual title.**\n\n" + minor, "high"},
		{"code_fence", "```text\n" + major + "\n```", "unknown"},
		{"unclosed_code_fence", "```\n" + major, "unknown"},
		{"indented_code", "    _Major_", "unknown"},
		{"html_pre", "<pre>\n" + major + "\n</pre>", "unknown"},
		{"html_quote", "<blockquote>\n" + major + "\n</blockquote>", "unknown"},
		{"ordinary_quote", "> " + major, "unknown"},
		{"nested_quote", "> > " + major, "unknown"},
		{"comment", "<!--\n" + major + "\n-->", "unknown"},
		{"prompt", prompt, "unknown"},
		{"unclosed_prompt", "<details>\n<summary>🤖 Prompt for AI Agents</summary>\n" + major, "unknown"},
		{"prompt_before_real_metadata", prompt + "\n" + major + "\n\n**Actual title.**", "high"},
		{"prompt_conflict_not_authority", major + "\n" + prompt + "\n\n**Actual title.**", "high"},
		{"conflicting_metadata", major + "\n" + minor + "\n\n**Actual title.**", "unknown"},
		{"conflicting_single_fields", "**Major**\n**Minor**", "unknown"},
		{"outside_header_body_conflict", outside, "unknown"},
		{"unknown_label", "_Category_ | _Severe_", "unknown"},
		{"unknown_icon", strings.Replace(major, `alt=""`, `alt="Severity"`, 1), "unknown"},
		{"repeated_picture", strings.Replace(major, icon, icon+icon, 1), "unknown"},
		{"unclosed_picture", strings.Replace(major, "</picture>", "", 1), "unknown"},
		{"unknown_wrapper", strings.Replace(major, icon, "<span>"+icon+"</span>", 1), "unknown"},
		{"oversized", major + strings.Repeat(" ", 4097), "unknown"},
		{"empty", "", "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := HostedDisplaySeverity(test.detail); got != test.want {
				t.Fatal("display severity", got, "want", test.want)
			}
		})
	}
}

func TestHostedDecoratedSeverityNativeExamplesAndConflicts(t *testing.T) {
	icon := standingTestPicture(false)
	major, minor := "_"+icon+" Major_", "**"+icon+" Minor**"
	for _, test := range []struct {
		name, metadata, want string
	}{
		{"conflict", major + "\n" + minor, "unknown"},
		{"code_only", "```\n" + major + "\n```", "unknown"},
		{"prompt_only", "<details>\n<summary>🤖 Prompt for AI Agents</summary>\n" + major + "\n</details>", "unknown"},
		{"hidden_conflict", major + "\n<details>\n<summary>Example</summary>\n" + minor + "\n</details>", "high"},
		{"malformed_picture", strings.Replace(major, `https://`, `http://`, 1), "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := test.metadata + "\n\n**Retain the finding.**\n\nOriginal detail.\n\n" + hfMarker(1)
			got := hfWant(t, hfNew(t, []string{body}, ""), true, 1)
			if got.Findings[0].Severity != test.want || got.Findings[0].Detail != body {
				t.Fatal("native severity used an example, conflict or malformed icon", got.Findings[0].Severity)
			}
		})
	}
}

func TestHostedMiddleDotMetadataNativeAndDisplay(t *testing.T) {
	for label, want := range map[string]string{"Major": "high", "Minor": "low", "Trivial": "info", "Critical": "critical"} {
		detail := "<!-- coderabbit-ai-generated:v1 -->\n\n**" + standingTestPicture(false) + " " + label + "** · **Functional Correctness** · **Quick win**\n\n**Keep a useful finding.**\n\nUseful issue prose.\n\n" + hfMarker(1)
		f := hfNew(t, []string{detail}, "")
		got := hfWant(t, f, true, 1)
		if len(got.Findings) != 1 || got.Findings[0].Severity != want || HostedDisplaySeverity(detail) != want {
			t.Fatalf("%s: %+v, display %s", label, got.Findings, HostedDisplaySeverity(detail))
		}
		if got.Findings[0].Detail != detail {
			t.Fatal("original bytes changed")
		}
	}
	if HostedDisplaySeverity("**Major** · **Minor** · **Quick win**") != "unknown" {
		t.Fatal("conflicting severity accepted")
	}
	if HostedDisplaySeverity("**Title.**\n\n**Major** · **Category** · **Quick win**") != "unknown" {
		t.Fatal("narrative metadata inferred")
	}
	if HostedDisplaySeverity("**Major** · **Category** | **Quick win**") != "unknown" {
		t.Fatal("mixed separator grammar accepted")
	}
}

func TestHostedDisplaySeverityKeepsLeadingMetadataBeforeBoundedDiagnostic(t *testing.T) {
	detail := "**Category** | **" + standingTestPicture(false) + " Minor** | **Quick win**\n\n<details>\n<summary>Supported by static analysis</summary>\n" + strings.Repeat("Diagnostic output. ", 500)
	if HostedDisplaySeverity(detail[:4096]) != "low" {
		t.Fatal("truncated diagnostic erased proven leading metadata")
	}
	if HostedDisplaySeverity("<details>\n<summary>Example</summary>\n**Major**") != "unknown" {
		t.Fatal("unclosed example supplied metadata")
	}
}
