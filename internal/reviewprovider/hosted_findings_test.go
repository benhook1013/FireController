package reviewprovider

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func hfMarker(n int) string { return fmt.Sprintf("<!-- cr-comment:v1:%024x -->", n) }

func hfInline(n int) string {
	return "_⚠️ Potential issue_ | _🟠 Major_\n\n**Retain the envelope.**\n\nIndependent rationale.\n\n" + hfMarker(n)
}

// These fixtures are independently authored. They preserve the observed nested
// category grammar, not any private provider finding, project path or prose.
func hfOutside(n int, path, location, title string) string {
	return "> <details>\n> <summary><em>🟡 Minor</em> · Shortened… · <code>queue.go:1</code></summary><blockquote>\n>\n> `" + path + ":" + location + "`\n> _📐 Maintainability & Code Quality_ | _🟡 Minor_\n>\n> **" + title + "**\n>\n> Preserve this literal <em>detail</em> & its spacing.\n>\n> <details>\n> <summary>🤖 Prompt for AI Agents</summary>\n> ```\n> Repeated synthetic guidance; not another source.\n> " + hfMarker(999) + "\n> ```\n> </details>\n>\n> " + hfMarker(n) + "\n>\n> _Source: Path instructions_\n> </blockquote></details>"
}

func hfOutsideCategory(count int, items ...string) string {
	return "> [!CAUTION]\n> Some comments are outside the diff and are listed below.\n>\n> **⚠️ Outside diff range comments (" + fmt.Sprint(count) + ")**\n>\n" + strings.Join(items, "\n>\n")
}

func hfDuplicateItem(n int, location, title string) string {
	return "`" + location + "`: _⚠️ Potential issue_ | _🔵 Trivial_ | _💤 Low value_\n\n**" + title + "**\n\nIndependent duplicate explanation.\n\n<details>\n<summary>Proposed wording</summary>\n```diff\n+ Synthetic wording.\n```\n</details>\n\n<details>\n<summary>🤖 Prompt for AI Agents</summary>\n```\nRepeated guidance.\n" + hfMarker(998) + "\n```\n</details>\n\n" + hfMarker(n)
}

func hfDuplicateGroup(path string, count int, items ...string) string {
	return "<details>\n<summary>" + path + " (" + fmt.Sprint(count) + ")</summary><blockquote>\n\n" + strings.Join(items, "\n\n") + "\n\n</blockquote></details>"
}

func hfDuplicateCategory(count int, groups ...string) string {
	return "<details>\n<summary>♻️ Duplicate comments (" + fmt.Sprint(count) + ")</summary><blockquote>\n\n" + strings.Join(groups, "\n\n") + "\n\n</blockquote></details>"
}

type hfFixture struct {
	snapshot    HostedPublicationSnapshot
	intent      HostedTriggerIntent
	poster, bot Author
	earliest    time.Time
}

func hfNew(t *testing.T, roots []string, summary string) hfFixture {
	t.Helper()
	now := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	f := hfFixture{intent: HostedTriggerIntent{OperationID: "review_operation_" + strings.Repeat("1", 32), AttemptID: "review_attempt_" + strings.Repeat("2", 32), HeadSHA: strings.Repeat("a", 40)}, poster: Author{ID: 101, Login: "synthetic-poster", Kind: "User"}, bot: Author{ID: 202, Login: "synthetic-reviewer", Kind: "Bot"}, earliest: now.Add(-time.Second)}
	body, err := HostedTriggerBody(f.intent)
	if err != nil {
		t.Fatal(err)
	}
	f.snapshot = HostedPublicationSnapshot{Complete: true, ObservedAt: now.Add(3 * time.Second), IssueComments: []IssueComment{{ID: 301, Body: body, Author: f.poster, CreatedAt: now, UpdatedAt: now}}, Reviews: []HostedReview{{ID: 401, Body: fmt.Sprintf("**Actionable comments posted: %d**\n\n", len(roots)) + summary + "\n\n<!-- coderabbit-review-publication v1 publication=11111111-1111-1111-1111-111111111111 attempt=22222222-2222-2222-2222-222222222222 batch=1/1 -->", State: "commented", HeadSHA: f.intent.HeadSHA, Author: f.bot, SubmittedAt: now.Add(time.Second)}}}
	for i, b := range roots {
		f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, ReviewComment{ID: int64(501 + i), ReviewID: 401, Body: b, Author: f.bot, Path: "engine/work.go", HeadSHA: f.intent.HeadSHA, OriginalHeadSHA: f.intent.HeadSHA, CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)})
	}
	return f
}

func (f hfFixture) extract(t *testing.T) HostedFindingInventory {
	t.Helper()
	got, err := ExtractHostedFindings(f.snapshot, f.intent, f.poster, f.bot, f.earliest)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func hfWant(t *testing.T, f hfFixture, complete bool, count int) HostedFindingInventory {
	t.Helper()
	got := f.extract(t)
	if got.Publication.State != "attributed" || got.Complete != complete || len(got.Findings) != count {
		t.Fatalf("publication=%s complete=%v reason=%s findings=%d; want %v/%d", got.Publication.State, got.Complete, got.Reason, len(got.Findings), complete, count)
	}
	if got.Reason == "" {
		t.Fatal("missing safe reason")
	}
	return got
}

func TestHostedFindingObservedOutsideAndDuplicateTemplates(t *testing.T) {
	outside := hfOutsideCategory(1, hfOutside(2, "engine/queue.go", "12-15", "Preserve the complete queue record."))
	duplicate := hfDuplicateCategory(1, hfDuplicateGroup("design/queue.md", 1, hfDuplicateItem(3, "27-27", "Explain the immutable boundary.")))
	f := hfNew(t, []string{hfInline(1)}, outside+"\n\n"+duplicate+"\n\n<details>\n<summary>🤖 Prompt to fix review comments</summary>\n```\nOutside diff comments:\nRepeated instructions, not findings.\nDuplicate comments:\nMore repeated instructions.\n"+hfMarker(9999)+"\n```\n</details>")
	got := hfWant(t, f, true, 3)
	if got.Reason != "finding_inventory_complete" {
		t.Fatal(got.Reason)
	}
	a, b, c := got.Findings[0], got.Findings[1], got.Findings[2]
	if a.Kind != "inline" || a.Severity != "high" || a.Title != "Retain the envelope." || a.SourceKey != fmt.Sprintf("hosted-comment:501:%024x", 1) {
		t.Fatalf("inline fields: %+v", a)
	}
	if b.Kind != "outside_diff" || b.Title != "Preserve the complete queue record." || b.Path != "engine/queue.go" || b.Severity != "low" || *b.StartLine != 12 || *b.EndLine != 15 || b.CommentID != 0 || b.ReviewID != 401 {
		t.Fatalf("outside fields: %+v", b)
	}
	if c.Kind != "duplicate" || c.Path != "design/queue.md" || c.Severity != "info" || *c.StartLine != 27 || *c.EndLine != 27 || c.Title != "Explain the immutable boundary." {
		t.Fatalf("duplicate fields: %+v", c)
	}
	if !strings.HasSuffix(b.Detail, "> "+hfMarker(2)) || !strings.HasPrefix(b.Detail, "> <details>\n") || !strings.Contains(b.Detail, "> Preserve this literal <em>detail</em> & its spacing.") {
		t.Fatal("outside detail byte span changed")
	}
	start := strings.Index(f.snapshot.Reviews[0].Body, "> <details>")
	end := strings.Index(f.snapshot.Reviews[0].Body, "> "+hfMarker(2)) + len("> "+hfMarker(2))
	if b.Detail != f.snapshot.Reviews[0].Body[start:end] {
		t.Fatal("detail is not exact original byte span")
	}
	if c.SourceKey != fmt.Sprintf("hosted-review:401:duplicate:%024x", 3) || c.Detail != strings.Split(hfDuplicateItem(3, "27-27", "Explain the immutable boundary."), hfMarker(3))[0]+hfMarker(3) {
		t.Fatal("duplicate identity/span changed")
	}
}

func TestHostedFindingControlledMultipleGroupsItemsAndRootCounts(t *testing.T) {
	outside := hfOutsideCategory(2, hfOutside(10, "a/one.go", "1", "First full title."), hfOutside(11, "b/two.go", "2-4", "Second full title."))
	duplicate := hfDuplicateCategory(3, hfDuplicateGroup("c/three.go", 2, hfDuplicateItem(12, "5-6", "Third title."), hfDuplicateItem(13, "7-7", "Fourth title.")), hfDuplicateGroup("d/four.go", 1, hfDuplicateItem(14, "8-9", "Fifth title.")))
	f := hfNew(t, []string{hfInline(1) + "\n\n" + hfInline(2)}, outside+"\n\n"+duplicate)
	got := hfWant(t, f, true, 7)
	if len(got.Publication.Roots) != 1 {
		t.Fatal("atomic finding count replaced root count")
	}
	for _, v := range got.Findings {
		if v.Fingerprint == fmt.Sprintf("%024x", 998) || v.Fingerprint == fmt.Sprintf("%024x", 999) {
			t.Fatal("prompt marker inflated count")
		}
	}
}

func TestHostedFindingControlledCompletePublicationBatches(t *testing.T) {
	f := hfNew(t, []string{hfInline(1)}, hfOutsideCategory(1, hfOutside(2, "a/source.go", "4", "Complete title.")))
	f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
	second := f.snapshot.Reviews[0]
	second.ID = 402
	second.Body = hfDuplicateCategory(1, hfDuplicateGroup("b/source.go", 1, hfDuplicateItem(3, "5-5", "Duplicate title."))) + "\n<!-- coderabbit-review-publication v1 publication=11111111-1111-1111-1111-111111111111 attempt=22222222-2222-2222-2222-222222222222 batch=2/2 -->"
	f.snapshot.Reviews = append(f.snapshot.Reviews, second)
	got := hfWant(t, f, true, 3)
	if got.Findings[2].ReviewID != 402 || len(got.Publication.Batches) != 2 {
		t.Fatal("second batch identity lost")
	}
	f.snapshot.Reviews = f.snapshot.Reviews[:1]
	got = f.extract(t)
	if got.Complete || len(got.Findings) != 0 || got.Reason != "missing_publication_batches" || len(got.Publication.Batches) != 1 {
		t.Fatal("partial publication was treated as extractable complete inventory")
	}
}

func TestHostedFindingStructuredLocationAndCopyIsolation(t *testing.T) {
	f := hfNew(t, []string{hfInline(1)}, "")
	a, b, c, d := int64(30), int64(40), int64(10), int64(20)
	root := &f.snapshot.ReviewComments[0]
	root.StartLine = &a
	root.Line = &b
	root.OriginalStartLine = &c
	root.OriginalLine = &d
	root.Side = "RIGHT"
	root.StartSide = "RIGHT"
	got := hfWant(t, f, true, 1)
	finding := got.Findings[0]
	if *finding.StartLine != 10 || *finding.EndLine != 20 || finding.Side != "RIGHT" {
		t.Fatal("original location not preferred")
	}
	*finding.StartLine = 99
	*finding.EndLine = 99
	*got.Publication.Roots[0].OriginalLine = 98
	if c != 10 || d != 20 {
		t.Fatal("returned pointers alias caller metadata")
	}
	root.OriginalStartLine = nil
	root.OriginalLine = nil
	got = hfWant(t, f, true, 1)
	if *got.Findings[0].StartLine != 30 || *got.Findings[0].EndLine != 40 {
		t.Fatal("current location fallback changed")
	}
	root.StartLine = nil
	root.Line = nil
	root.Side = ""
	root.StartSide = ""
	got = hfWant(t, f, true, 1)
	if got.Findings[0].StartLine != nil || got.Findings[0].EndLine != nil || got.Findings[0].Side != "" {
		t.Fatal("invented unknown location")
	}
}

func TestHostedFindingExplicitSeverityAndFallbackTitle(t *testing.T) {
	for label, want := range map[string]string{"Trivial": "info", "Minor": "low", "Major": "high", "Critical": "critical"} {
		f := hfNew(t, []string{"_" + label + "_\n\n" + hfMarker(1)}, "")
		got := hfWant(t, f, true, 1)
		if got.Findings[0].Severity != want || got.Findings[0].Title != got.Findings[0].SourceKey {
			t.Fatal("explicit mapping/fallback changed")
		}
	}
	for _, body := range []string{"Major failures appear in this rationale.\n" + hfMarker(1), "_Major_\n_Minor_\n" + hfMarker(1)} {
		got := hfWant(t, hfNew(t, []string{body}, ""), true, 1)
		if got.Findings[0].Severity != "unknown" {
			t.Fatal("severity inferred from conflicting/prose evidence")
		}
	}
}

func TestHostedFindingMetadataStylesPreserveSource(t *testing.T) {
	for _, test := range []struct {
		name, metadata, severity string
	}{
		{"italic", "_Category_ | _🟡 Minor_ | _⚡ Quick win_", "low"},
		{"bold", "**Category** | **🔵 Trivial** | **💤 Low value**", "info"},
		{"mixed bold severity", "_Category_ | **🟠 Major** | _⚡ Quick win_", "high"},
		{"mixed italic severity", "**Category** | _🔴 Critical_ | **Heavy lift**", "critical"},
		{"malformed category", "**Category_ | **🟡 Minor** | _Quick win_", "unknown"},
		{"malformed severity", "_Category_ | **🟡 Minor_ | _Quick win_", "unknown"},
		{"unwrapped field", "Category | **🟡 Minor** | **Quick win**", "unknown"},
		{"unwrapped severity", "_Category_ | Minor | _Quick win_", "unknown"},
		{"nested severity wrappers", "_Category_ | _**Minor**_ | _Quick win_", "unknown"},
		{"unknown severity", "**Category** | **Unclassified** | **Quick win**", "unknown"},
		{"conflicting fields", "**Category** | **🟡 Minor** | _🔴 Critical_", "unknown"},
		{"conflicting styles", "**Category** | **🟡 Minor** | **Quick win**\n_Category_ | _🔴 Critical_ | _Heavy lift_", "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := test.metadata + "\n\n**Keep this source.**\n\nLiteral detail <b>text</b> & Ω.\n\n" + hfMarker(1)
			got := hfWant(t, hfNew(t, []string{body}, ""), true, 1).Findings[0]
			if got.Severity != test.severity || got.Title != "Keep this source." || got.Detail != body || got.SourceKey != fmt.Sprintf("hosted-comment:501:%024x", 1) {
				t.Fatalf("metadata changed severity/source identity: severity=%q title=%q", got.Severity, got.Title)
			}
		})
	}
}

func TestHostedFindingBoldMetadataInExamplesIsNotAuthoritative(t *testing.T) {
	example := "**Category** | **🔴 Critical** | **Heavy lift**\n\n**Example headline**\n" + hfMarker(99)
	for name, hidden := range map[string]string{
		"fence":   "```text\n" + example + "\n```\n",
		"prompt":  "<details>\n<summary>🤖 Prompt for AI Agents</summary>\n" + example + "\n</details>\n",
		"example": "<details>\n<summary>Example</summary>\n" + example + "\n</details>\n",
	} {
		t.Run(name, func(t *testing.T) {
			body := hidden + "**Category** | **🟡 Minor** | **Quick win**\n\n**Keep the actual headline.**\n\n" + hfMarker(1)
			got := hfWant(t, hfNew(t, []string{body}, ""), true, 1).Findings[0]
			if got.Severity != "low" || got.Title != "Keep the actual headline." || got.Fingerprint != fmt.Sprintf("%024x", 1) || got.Detail != body {
				t.Fatal("hidden bold metadata became authoritative or source bytes changed")
			}
		})
	}
}

func TestHostedFindingHiddenExamplesCannotSupplyFieldsOrMarkers(t *testing.T) {
	for name, hidden := range map[string]string{
		"fence":                "```text\n_Major_\n**Forged title**\n" + hfMarker(90) + "\n```\n",
		"tilde":                "~~~\n" + hfMarker(90) + "\n~~~\n",
		"indented":             "    " + hfMarker(90) + "\n",
		"quote":                "> " + hfMarker(90) + "\n",
		"pre":                  "<pre>\n" + hfMarker(90) + "\n</pre>\n",
		"blockquote":           "<blockquote>\n" + hfMarker(90) + "\n</blockquote>\n",
		"blockquoteAttributes": "<blockquote class=\"example\">\n" + hfMarker(90) + "\n</blockquote>\n",
		"blockquoteCompact":    "<blockquote><p>Example</p>\n" + hfMarker(90) + "\n</blockquote>\n",
		"comment":              "prefix <!-- example\n" + hfMarker(90) + "\n-->\n",
		"prompt":               "<details>\n<summary>🤖 Prompt for AI Agents</summary>\n" + hfMarker(90) + "\n</details>\n",
		"promptCompact":        "<details><summary>🤖 Prompt for AI Agents</summary>\n" + hfMarker(90) + "\n</details>\n",
	} {
		t.Run(name, func(t *testing.T) {
			got := hfWant(t, hfNew(t, []string{hidden + hfInline(1)}, ""), true, 1)
			if got.Findings[0].Fingerprint != fmt.Sprintf("%024x", 1) || got.Findings[0].Title != "Retain the envelope." {
				t.Fatal("example became authoritative")
			}
		})
	}
	outside := hfOutside(2, "engine/task.go", "5", "Real title.")
	outside = strings.Replace(outside, "> \n> "+hfMarker(2), "> > "+hfMarker(90)+"\n> \n> "+hfMarker(2), 1)
	hfWant(t, hfNew(t, nil, hfOutsideCategory(1, outside)), true, 1)
}

func TestHostedFindingUnsupportedMarkersCountsContextsAndTrailers(t *testing.T) {
	base := hfOutside(2, "engine/task.go", "5", "Real title.")
	cases := map[string]struct {
		roots   []string
		summary string
		pieces  int
	}{
		"markerlessRoot":       {roots: []string{"**An unmarked finding.**"}},
		"malformed":            {roots: []string{strings.Replace(hfInline(1), "v1:", "v2:", 1)}},
		"shortFingerprint":     {roots: []string{strings.Replace(hfInline(1), fmt.Sprintf("%024x", 1), "abcdef", 1)}},
		"uppercaseFingerprint": {roots: []string{strings.Replace(hfInline(1), fmt.Sprintf("%024x", 1), strings.Repeat("A", 24), 1)}},
		"repeatWithinRoot":     {roots: []string{hfInline(1) + "\n" + hfInline(1)}, pieces: 1},
		"repeatAcrossRoots":    {roots: []string{hfInline(1), hfInline(1)}, pieces: 1},
		"repeatAcrossKinds":    {roots: []string{hfInline(2)}, summary: hfOutsideCategory(1, base), pieces: 1},
		"trailingRootProse":    {roots: []string{hfInline(1) + "\nA separate unmarked obligation."}, pieces: 1},
		"outsideCount":         {summary: hfOutsideCategory(2, base), pieces: 1},
		"outsideMissingTitle":  {summary: hfOutsideCategory(1, strings.Replace(base, "> **Real title.**", "> Rationale without title.", 1)), pieces: 1},
		"outsideInvalidPath":   {summary: hfOutsideCategory(1, hfOutside(2, "../escape", "5", "Real title.")), pieces: 1},
		"outsideZeroLine":      {summary: hfOutsideCategory(1, hfOutside(2, "engine/a.go", "0", "Real title.")), pieces: 1},
		"outsideReversed":      {summary: hfOutsideCategory(1, hfOutside(2, "engine/a.go", "9-3", "Real title.")), pieces: 1},
		"outsideOverflow":      {summary: hfOutsideCategory(1, hfOutside(2, "engine/a.go", "2147483648", "Real title.")), pieces: 1},
		"outsideTrailer":       {summary: hfOutsideCategory(1, strings.Replace(base, "> _Source:", "> New unmarked issue.\n> _Source:", 1)), pieces: 1},
		"unclassifiedSummary":  {summary: hfMarker(77)},
		"unknownCategory":      {summary: "<details>\n<summary>Other findings (1)</summary>\n" + hfMarker(77) + "\n</details>"},
		"unquotedOutside":      {summary: strings.ReplaceAll(hfOutsideCategory(1, base), "> ", "")},
		"duplicateTotal":       {summary: hfDuplicateCategory(2, hfDuplicateGroup("a/b.go", 1, hfDuplicateItem(3, "1-2", "Known title."))), pieces: 1},
		"duplicatePathCount":   {summary: hfDuplicateCategory(1, hfDuplicateGroup("a/b.go", 2, hfDuplicateItem(3, "1-2", "Known title."))), pieces: 1},
		"duplicateBadGroup":    {summary: hfDuplicateCategory(1, hfDuplicateGroup("../bad", 1, hfDuplicateItem(3, "1-2", "Known title."))), pieces: 1},
		"duplicateReversed":    {summary: hfDuplicateCategory(1, hfDuplicateGroup("a/b.go", 1, hfDuplicateItem(3, "8-2", "Known title."))), pieces: 1},
		"unclosedFence":        {roots: []string{hfInline(1) + "\n```"}, pieces: 1},
		"unclosedDetails":      {summary: strings.TrimSuffix(hfOutsideCategory(1, base), "</blockquote></details>")},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := hfWant(t, hfNew(t, c.roots, c.summary), false, c.pieces)
			for _, finding := range got.Findings {
				if finding.Path != "" && !validRepoPath(finding.Path) {
					t.Fatal("invalid source location promoted to structured path")
				}
				if finding.StartLine != nil && (*finding.StartLine < 1 || *finding.StartLine > 2147483647) || finding.EndLine != nil && (*finding.EndLine < 1 || *finding.EndLine > 2147483647) {
					t.Fatal("invalid structured line escaped incomplete parsing")
				}
			}
		})
	}
}

func TestHostedFindingSourceMetadataTrailingAndCRLFArePreserved(t *testing.T) {
	body := "\n" + hfInline(1) + "\n\n_Source: Project instructions_\n"
	f := hfNew(t, []string{strings.ReplaceAll(body, "\n", "\r\n")}, "")
	got := hfWant(t, f, true, 1)
	want := f.snapshot.ReviewComments[0].Body[:strings.Index(f.snapshot.ReviewComments[0].Body, hfMarker(1))+len(hfMarker(1))+1]
	if got.Findings[0].Detail != want {
		t.Fatal("CRLF/leading whitespace was rewritten")
	}
}

func TestHostedFindingEditedConfirmationIsOnlyBoundedSourceMetadata(t *testing.T) {
	confirmation := "<!-- This is an auto-generated reply by CodeRabbit -->\n\n✅ Confirmed as addressed by @synthetic-author\n\n<!-- This is an auto-generated comment by CodeRabbit -->"
	root := hfInline(1) + "\n\n_Source: Path instructions_\n\n" + confirmation
	f := hfNew(t, []string{root}, "")
	got := hfWant(t, f, true, 1)
	if got.Findings[0].Detail != hfInline(1) || got.Findings[0].Title != "Retain the envelope." || !strings.Contains(got.Publication.Roots[0].Body, confirmation) {
		t.Fatal("provider confirmation changed source finding or raw retention")
	}
	for _, bad := range []string{
		"✅ Confirmed as addressed by @synthetic-author",
		strings.TrimSuffix(confirmation, "<!-- This is an auto-generated comment by CodeRabbit -->"),
		strings.Replace(confirmation, "@synthetic-author", "@synthetic-author plus another obligation", 1),
		strings.Replace(confirmation, "✅ Confirmed as addressed", "✅ Assessed as accepted", 1),
		confirmation + "\nAdditional unmarked finding.",
	} {
		hfWant(t, hfNew(t, []string{hfInline(1) + "\n\n" + bad}, ""), false, 1)
	}
}

func TestHostedFindingCommittableSuggestionQuotesCannotHideOwnedMarker(t *testing.T) {
	wrapper := "<details>\n<summary>📝 Committable suggestion</summary>\n\n> Advisory metadata.\n> " + hfMarker(99) + "\n\n```suggestion\nIndependent suggested text.\n```\n\n</details>"
	root := "_🟡 Minor_\n\n**Keep the full original finding.**\n\n<details>\n<summary>Proposed fix</summary>\n```diff\n- prior synthetic text\n+ revised synthetic text\n```\n</details>\n\n" + wrapper + "\n\n" + hfMarker(1)
	got := hfWant(t, hfNew(t, []string{root}, ""), true, 1)
	if got.Findings[0].Fingerprint != fmt.Sprintf("%024x", 1) || got.Findings[0].Detail != root {
		t.Fatal("quoted advisory marker became source or detail was rewritten")
	}
	if strings.Contains(got.Findings[0].SourceKey, fmt.Sprintf("%024x", 99)) {
		t.Fatal("quoted example supplied identity")
	}
}

func TestHostedFindingLimitsRemainVisibleWithoutTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		count      int
	}{
		{"title", strings.Replace(hfInline(1), "Retain the envelope.", strings.Repeat("界", 167), 1), 0},
		{"detail", strings.Repeat("x", 16<<10) + "\n" + hfMarker(1), 0},
	} {
		t.Run(tc.name, func(t *testing.T) { hfWant(t, hfNew(t, []string{tc.body}, ""), false, tc.count) })
	}
	var bodies []string
	for i := 1; i <= 101; i++ {
		bodies = append(bodies, hfInline(i))
	}
	f := hfNew(t, []string{strings.Join(bodies, "\n\n")}, "")
	got := hfWant(t, f, false, 100)
	if got.Reason != "finding_inventory_limit" || len(got.Publication.Roots) != 1 || got.Publication.Roots[0].Body != f.snapshot.ReviewComments[0].Body {
		t.Fatal("limit lost raw source or fabricated aggregate")
	}
	hfWant(t, hfNew(t, []string{strings.Join(bodies[:100], "\n\n")}, ""), true, 100)
}

func TestHostedFindingRechecksPublicationAndRetainsMarkerlessChatter(t *testing.T) {
	f := hfNew(t, []string{hfInline(1)}, "")
	chatter := f.snapshot.Reviews[0]
	chatter.ID = 402
	chatter.Body = "Status update without findings. ```"
	f.snapshot.Reviews = append(f.snapshot.Reviews, chatter)
	hfWant(t, f, true, 1)
	f.snapshot.Reviews[1].Body = hfMarker(90)
	hfWant(t, f, false, 1)
	f.snapshot.Reviews[1].Body = hfOutsideCategory(1, hfOutside(2, "a/b.go", "3", "Supplemental title."))
	hfWant(t, f, true, 2)
	f.snapshot.Complete = false
	got := f.extract(t)
	if got.Complete || len(got.Findings) != 0 || got.Publication.State == "attributed" {
		t.Fatal("trusted stale attribution on incomplete observation")
	}
	f = hfNew(t, []string{hfInline(1)}, "")
	f.snapshot.Reviews[0].HeadSHA = strings.Repeat("b", 40)
	got = f.extract(t)
	if got.Complete || len(got.Findings) != 0 {
		t.Fatal("wrong head supplied finding inventory")
	}
	f = hfNew(t, nil, "")
	f.bot.ID = f.poster.ID
	f.bot.Kind = f.poster.Kind
	got, err := ExtractHostedFindings(f.snapshot, f.intent, f.poster, f.bot, f.earliest)
	if err == nil || got.Complete || got.Reason != "publication_unavailable" {
		t.Fatal("invalid selector accepted")
	}
}

func TestHostedFindingMalformedInputsNeverMutateOrPanic(t *testing.T) {
	for _, body := range []string{"", "\x00", "<details>", "</details>", strings.Repeat(">", 1000), "<!-- cr-comment:v1: -->", "\xff", "```\n> <details>\n", "<pre><pre></pre>"} {
		f := hfNew(t, nil, body)
		before := fmt.Sprintf("%#v", f.snapshot)
		_, _ = ExtractHostedFindings(f.snapshot, f.intent, f.poster, f.bot, f.earliest)
		if fmt.Sprintf("%#v", f.snapshot) != before {
			t.Fatal("decoder modified input")
		}
	}
	f := hfNew(t, []string{hfInline(1)}, hfOutsideCategory(1, hfOutside(2, "a/b.go", "1", "Title.")))
	first := f.extract(t)
	second := f.extract(t)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("decoder output not deterministic")
	}
}
