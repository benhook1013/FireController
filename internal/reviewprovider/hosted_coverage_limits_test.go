package reviewprovider

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func unavailableTestIcon() string {
	return `<picture><source media="(prefers-color-scheme: dark)" srcset="https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/file-unavailable-dark.svg"><img src="https://storage.googleapis.com/coderabbit_public_assets/review-comment-icons-v1/file-unavailable.svg" width="16" height="16" align="absmiddle" alt=""></picture> `
}

func unavailableTestBlock(count string, paths ...string) string {
	return "<details><summary>" + unavailableTestIcon() + "Files not fully reviewed because their changes were unavailable: " + count + "</summary><dl><dd>\n\n* " + strings.Join(paths, "\n* ") + "\n\n</dd></dl></details>"
}

func unavailableTestInfo(body string) string {
	return "<details>\n<summary>ℹ️ Review info</summary>\n<dl><dd>\n" + body + "\n</dd></dl>\n</details>"
}

func TestHostedUnavailableFilesRetainBoundedCoverageWithoutChangingCapture(t *testing.T) {
	const path = "design/project-management/implementation-tracking/player-access-and-session.md"
	const want = "CodeRabbit reported 1 file not fully reviewed because its changes were unavailable: " + path + "."
	for _, decoration := range []string{"picture", "plain", "review-info-picture"} {
		t.Run(decoration, func(t *testing.T) {
			body := unavailableTestInfo(unavailableTestBlock("1", path))
			if decoration == "plain" {
				body = strings.Replace(body, unavailableTestIcon(), "", 1)
			}
			if decoration == "review-info-picture" {
				body = strings.Replace(body, "<summary>ℹ️ Review info</summary>", syntheticReviewInfoIcon(), 1)
			}
			f := hfNew(t, []string{hfInline(1)}, body)
			before := f.snapshot.Reviews[0].Body
			inventory := hfWant(t, f, true, 1)
			if !reflect.DeepEqual(inventory.CoverageLimits, []string{want}) || inventory.Publication.Batches[0].Body != before || f.snapshot.Reviews[0].Body != before {
				t.Fatal("provider coverage limitation was lost or source was rewritten", inventory.CoverageLimits)
			}
			if inventory.Findings[0].SourceKey != "hosted-comment:501:000000000000000000000001" {
				t.Fatal("coverage metadata changed actual finding identity")
			}
		})
	}
}

func TestHostedUnavailableFilesKeepCountWhenPathsExceedPlainMetadataBound(t *testing.T) {
	one, two := strings.Repeat("a", 600)+".go", strings.Repeat("b", 600)+".go"
	f := hfNew(t, []string{hfInline(1)}, unavailableTestInfo(unavailableTestBlock("2", one, two)))
	inventory := hfWant(t, f, true, 1)
	want := "CodeRabbit reported 2 files not fully reviewed because their changes were unavailable. See retained review metadata for file paths."
	if !reflect.DeepEqual(inventory.CoverageLimits, []string{want}) || !strings.Contains(inventory.Publication.Batches[0].Body, one) || !strings.Contains(inventory.Publication.Batches[0].Body, two) {
		t.Fatal("bounded coverage metadata lost declared count or raw paths", inventory.CoverageLimits)
	}
}

func TestHostedUnavailableFilesMalformedOptionalMetadataRemainsConservative(t *testing.T) {
	valid := unavailableTestBlock("1", "source.go")
	for name, body := range map[string]string{
		"count mismatch":       unavailableTestBlock("2", "source.go"),
		"duplicate path":       unavailableTestBlock("2", "source.go", "source.go"),
		"traversal":            unavailableTestBlock("1", "../source.go"),
		"absolute":             unavailableTestBlock("1", "/source.go"),
		"markup":               unavailableTestBlock("1", "<script>source.go</script>"),
		"noncanonical count":   strings.Replace(valid, "unavailable: 1", "unavailable: 01", 1),
		"unbounded count":      strings.Replace(valid, "unavailable: 1", "unavailable: 10001", 1),
		"missing list wrapper": strings.Replace(valid, "<dl><dd>", "", 1),
		"quoted file":          strings.Replace(valid, "* source.go", "> * source.go", 1),
		"fenced file":          strings.Replace(valid, "* source.go", "```\n* source.go\n```", 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := hfNew(t, []string{hfInline(1)}, unavailableTestInfo(body))
			inventory := hfWant(t, f, true, 1)
			if !reflect.DeepEqual(inventory.CoverageLimits, []string{hostedCoverageUnknown}) {
				t.Fatal("malformed optional coverage claimed a proven file inventory", inventory.CoverageLimits)
			}
		})
	}
}

func TestHostedUnavailableFilesCannotBeClaimedByQuotedOrForeignScopes(t *testing.T) {
	block := unavailableTestBlock("1", "spoofed.go")
	for name, body := range map[string]string{
		"quoted":                  unavailableTestInfo("> " + strings.ReplaceAll(block, "\n", "\n> ")),
		"fenced":                  unavailableTestInfo("```html\n" + block + "\n```"),
		"html pre":                unavailableTestInfo("<pre>\n" + block + "\n</pre>"),
		"html quote":              unavailableTestInfo("<blockquote>\n" + block + "\n</blockquote>"),
		"other HTML ancestor":     unavailableTestInfo("<div>\n" + block + "\n</div>"),
		"nested example":          unavailableTestInfo("<details>\n<summary>Example</summary>\n" + block + "\n</details>"),
		"nested prompt":           unavailableTestInfo("<details>\n<summary>🤖 Prompt to fix review comments</summary>\n" + block + "\n</details>"),
		"inside prompt":           "<details>\n<summary>🤖 Prompt to fix review comments</summary>\n" + unavailableTestInfo(block) + "\n</details>",
		"unknown metadata header": strings.Replace(unavailableTestInfo(block), "Review info", "Review info example", 1),
		"outside metadata":        block,
	} {
		t.Run(name, func(t *testing.T) {
			f := hfNew(t, []string{hfInline(1)}, body)
			if got := f.extract(t); len(got.CoverageLimits) != 0 {
				t.Fatal("quoted/example prose supplied coverage metadata", got.CoverageLimits)
			}
		})
	}
	f := hfNew(t, []string{hfInline(1)}, unavailableTestInfo(block))
	f.snapshot.Reviews[0].Author.ID++
	if got := f.extract(t); len(got.CoverageLimits) != 0 {
		t.Fatal("foreign provider supplied coverage metadata", got.CoverageLimits)
	}
}

func TestHostedUnavailableFilesDeduplicateAndBoundMetadataAcrossSections(t *testing.T) {
	block := unavailableTestBlock("1", "source.go")
	f := hfNew(t, []string{hfInline(1)}, unavailableTestInfo(block+"\n"+block))
	if got := hfWant(t, f, true, 1); len(got.CoverageLimits) != 1 {
		t.Fatal("repeated metadata duplicated limitation", got.CoverageLimits)
	}
	var sections []string
	for i := 0; i < 17; i++ {
		sections = append(sections, unavailableTestBlock("1", fmt.Sprintf("source-%d.go", i)))
	}
	f = hfNew(t, []string{hfInline(1)}, unavailableTestInfo(strings.Join(sections, "\n")))
	got := hfWant(t, f, true, 1)
	if len(got.CoverageLimits) != 16 || got.CoverageLimits[15] != hostedCoverageMore {
		t.Fatal("coverage metadata exceeded retained run limits or hid overflow", got.CoverageLimits)
	}
}
