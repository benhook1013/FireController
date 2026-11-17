package reviewprovider

import (
	"fmt"
	"strings"
	"testing"
)

// Independently authored metadata with fake source identities and paths. The
// configuration is display metadata, not head/comparison/completion authority.
const standingExpandedConfig = `- **Source mode**: Auto
- **Repository**: [example/project](https://github.com/example/project)
- **Branch**: [feature/config](https://github.com/example/project/tree/feature/config)
- **File**: [.coderabbit.yaml](<https://github.com/example/project/blob/CONFIG_HEAD/.coderabbit.yaml>)
- **Commit**: [CONFIG_SHORT](<https://github.com/example/project/commit/CONFIG_HEAD>)
- **Layers**: Repository configuration
`

func standingExpandedBody(head, base string) string {
	return strings.Replace(standingTestBody(head, base), "- **Configuration used**: Repository: synthetic/project/.coderabbit.yaml\n", strings.NewReplacer("CONFIG_HEAD", head, "CONFIG_SHORT", head[:7]).Replace(standingExpandedConfig), 1)
}

func TestHostedStandingExpandedConfigurationPreservesExactZeroInventory(t *testing.T) {
	for _, crlf := range []bool{false, true} {
		t.Run(fmt.Sprintf("CRLF_%t", crlf), func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			body := standingExpandedBody(intent.HeadSHA, strings.Repeat("b", 40))
			files := make([]string, 32)
			for i := range files {
				files[i] = fmt.Sprintf("* `pkg/source%02d.go`", i)
			}
			body = strings.Replace(body, "processing: 2", "processing: 32", 1)
			body = strings.Replace(body, "* `pkg/alpha.go`\n* `docs/guide.md`", strings.Join(files, "\n"), 1)
			body = strings.Replace(body, "`build/generated.js` is excluded by `!build/**`", "`build/archive.gz` is excluded by `!**/*.gz`", 1)
			if crlf {
				body = strings.ReplaceAll(body, "\n", "\r\n")
			}
			s.IssueComments[1].Body = body
			inventory, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || !inventory.Complete || inventory.Findings == nil || len(inventory.Findings) != 0 || inventory.Publication.StandingZero == nil {
				t.Fatal("expanded configuration lost proven zero inventory", inventory.Reason, err)
			}
			zero := inventory.Publication.StandingZero
			if zero.ProviderRunID != standingTestRun || zero.HeadSHA != intent.HeadSHA || zero.BaseSHA != strings.Repeat("b", 40) || zero.SelectedFiles != 32 || zero.IgnoredFiles != 1 || zero.Summary.ID != 601 || zero.Acknowledgement.ID != 602 || !inventory.Publication.FinishedAt.Equal(s.IssueComments[2].UpdatedAt) {
				t.Fatal("configuration replaced authoritative source facts", zero)
			}
			if comparison, ok := ResolveHostedComparison(inventory.Publication, intent.HeadSHA, strings.Repeat("b", 40), strings.Repeat("d", 40)); !ok || comparison != strings.Repeat("b", 40) {
				t.Fatal("expanded metadata lost exact comparison authority")
			}
			if _, ok := ResolveHostedComparison(inventory.Publication, intent.HeadSHA, strings.Repeat("d", 40), strings.Repeat("e", 40)); ok {
				t.Fatal("expanded metadata granted an unrelated comparison")
			}
			if len(inventory.Publication.Batches) != 0 || inventory.Publication.PublicationID != "" || inventory.Publication.ProviderAttemptID != "" {
				t.Fatal("standing zero invented publication/review identity")
			}
		})
	}
}

func TestHostedStandingExpandedConfigurationDoesNotWeakenProof(t *testing.T) {
	changes := map[string]func(string) string{
		"unknown source mode": func(b string) string { return strings.Replace(b, "Source mode**: Auto", "Source mode**: Unknown", 1) },
		"missing field":       func(b string) string { return strings.Replace(b, "- **Layers**: Repository configuration\n", "", 1) },
		"empty field": func(b string) string {
			return strings.Replace(b, "- **Layers**: Repository configuration", "- **Layers**: ", 1)
		},
		"unknown field": func(b string) string { return strings.Replace(b, "- **Layers**: ", "- **Unrecognized metadata**: ", 1) },
		"duplicate field": func(b string) string {
			return strings.Replace(b, "- **Layers**: Repository configuration", "- **Layers**: Repository configuration\n- **Layers**: Another layer", 1)
		},
		"oversized metadata": func(b string) string {
			return strings.Replace(b, "Repository configuration", strings.Repeat("x", 1001), 1)
		},
		"mixed legacy metadata": func(b string) string {
			return strings.Replace(b, "- **Review profile**: ", "- **Configuration used**: Extra configuration\n- **Review profile**: ", 1)
		},
		"unknown findings section": func(b string) string {
			return strings.Replace(b, standingRecentEnd, "<details><summary>Unrecognized review findings</summary></details>\n"+standingRecentEnd, 1)
		},
		"positive finding claim": func(b string) string {
			return strings.Replace(b, "- **Layers**: Repository configuration", "- **Layers**: Findings: 1", 1)
		},
		"wrong structured commit": func(b string) string {
			b = strings.Replace(b, "/commit/"+strings.Repeat("c", 40), "/commit/"+strings.Repeat("f", 40), 1)
			return strings.Replace(b, "["+strings.Repeat("c", 7)+"]", "["+strings.Repeat("f", 7)+"]", 1)
		},
		"wrong structured file head": func(b string) string {
			return strings.Replace(b, "/blob/"+strings.Repeat("c", 40), "/blob/"+strings.Repeat("f", 40), 1)
		},
		"noncommit URL": func(b string) string { return strings.Replace(b, "/commit/", "/tree/", 1) },
		"foreign commit host": func(b string) string {
			return strings.Replace(b, "https://github.com/example/project/commit/", "https://foreign.invalid/example/project/commit/", 1)
		},
		"partial commit SHA": func(b string) string {
			return strings.Replace(b, "/commit/"+strings.Repeat("c", 40), "/commit/"+strings.Repeat("c", 7), 1)
		},
		"wrong run":          func(b string) string { return strings.Replace(b, standingTestRun, "invalid-run", 1) },
		"selected inventory": func(b string) string { return strings.Replace(b, "processing: 2", "processing: 3", 1) },
		"ignored inventory":  func(b string) string { return strings.Replace(b, "path filters: 1", "path filters: 2", 1) },
		"wrong coverage": func(b string) string {
			return strings.Replace(b, `"coveredCommitId":"`+strings.Repeat("c", 40), `"coveredCommitId":"`+strings.Repeat("d", 40), 1)
		},
		"incomplete comparison": func(b string) string { return strings.Replace(b, "from the base of the PR", "in selected files", 1) },
		"wrong selected head": func(b string) string {
			return strings.ReplaceAll(b, strings.Repeat("c", 40), strings.Repeat("d", 40))
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			body := standingExpandedBody(intent.HeadSHA, strings.Repeat("b", 40))
			s.IssueComments[1].Body = change(body)
			if body == s.IssueComments[1].Body {
				t.Fatal("fixture did not alter its intended boundary")
			}
			inventory, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || inventory.Complete || inventory.Publication.StandingZero != nil || inventory.Publication.State == "attributed" {
				t.Fatal("expanded configuration concealed unproven inventory", inventory.Reason, err)
			}
		})
	}
	for _, boundary := range []string{"missing acknowledgement", "foreign acknowledgement", "wrong acknowledgement"} {
		t.Run(boundary, func(t *testing.T) {
			s, intent, poster, bot, earliest := standingTestFixture(t)
			s.IssueComments[1].Body = standingExpandedBody(intent.HeadSHA, strings.Repeat("b", 40))
			switch boundary {
			case "missing acknowledgement":
				s.IssueComments = s.IssueComments[:2]
			case "foreign acknowledgement":
				s.IssueComments[2].Author.ID++
			case "wrong acknowledgement":
				s.IssueComments[2].Body += "\nOne unresolved finding."
			}
			inventory, err := ExtractHostedFindings(s, intent, poster, bot, earliest)
			if err != nil || inventory.Complete || inventory.Publication.StandingZero != nil {
				t.Fatal("configuration bypassed attribution or comparison", inventory.Reason, err)
			}
		})
	}
}
