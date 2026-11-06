package reviewprovider

import (
	"encoding/json"
	"strings"
	"testing"
)

// Synthetic generated grammar only. Omitted numeric metadata is not a zero;
// the exact submitted batch and its attached, decoded source prove one finding.
func omittedInlineFixture(t *testing.T) hfFixture {
	t.Helper()
	f := hfNew(t, []string{hfInline(1)}, "")
	f.snapshot.Reviews[0].Body = "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated comment by CodeRabbit for review status -->\n\n<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated comment by CodeRabbit for review status -->\n\n" + publicationTestMarker(1, 1) + "\n\n<!-- autofix_checkbox_start -->\n- [ ] <!-- {\"checkboxId\":\"33333333-3333-3333-3333-333333333333\"} --> Fix comments: Fix CodeRabbit comments on this PR\n<!-- autofix_checkbox_end -->"
	return f
}

func TestHostedInlineOnlyOmittedCountPositiveInventory(t *testing.T) {
	f := omittedInlineFixture(t)
	before, _ := json.Marshal(f.snapshot)
	got := f.extract(t)
	if got.Publication.State != "attributed" || !got.Complete || len(got.Findings) != 1 || got.Findings[0].Kind != "inline" || got.Findings[0].CommentID != 501 || got.Findings[0].ReviewID != 401 || got.Findings[0].SourceKey != "hosted-comment:501:000000000000000000000001" {
		t.Fatalf("positive omitted-count source blocked: state=%s reason=%s complete=%t findings=%d", got.Publication.State, got.Reason, got.Complete, len(got.Findings))
	}
	after, _ := json.Marshal(f.snapshot)
	if string(before) != string(after) {
		t.Fatal("count proof rewrote source")
	}
	if replay := f.extract(t); len(replay.Findings) != 1 || replay.Findings[0].SourceKey != got.Findings[0].SourceKey {
		t.Fatal("original source identity changed")
	}
}

func TestHostedOmittedInlineCountCannotInventInventoryOrBypassBindings(t *testing.T) {
	for name, change := range map[string]func(*hfFixture){
		"zero roots":    func(f *hfFixture) { f.snapshot.ReviewComments = []ReviewComment{} },
		"unknown roots": func(f *hfFixture) { f.snapshot.ReviewComments = nil },
		"bare finished": func(f *hfFixture) {
			f.snapshot.ReviewComments = []ReviewComment{}
			f.snapshot.Reviews[0].Body = "Review complete.\n" + publicationTestMarker(1, 1)
		},
		"malformed count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: unknown**\n" + f.snapshot.Reviews[0].Body
		},
		"explicit contradictory zero": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: 0**\n" + f.snapshot.Reviews[0].Body
		},
		"duplicate count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: 1**\n**Actionable comments posted: 1**\n" + f.snapshot.Reviews[0].Body
		},
		"lowercase malformed count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**actionable comments posted: 1**\n" + f.snapshot.Reviews[0].Body
		},
		"unsupported root": func(f *hfFixture) { f.snapshot.ReviewComments[0].Body = "An unstructured observation." },
		"quoted root": func(f *hfFixture) {
			f.snapshot.ReviewComments[0].Body = "> " + strings.ReplaceAll(f.snapshot.ReviewComments[0].Body, "\n", "\n> ")
		},
		"foreign root actor":    func(f *hfFixture) { f.snapshot.ReviewComments[0].Author.ID++ },
		"foreign original head": func(f *hfFixture) { f.snapshot.ReviewComments[0].OriginalHeadSHA = strings.Repeat("b", 40) },
		"unbound root":          func(f *hfFixture) { f.snapshot.ReviewComments[0].ReviewID++ },
		"fresh orphan": func(f *hfFixture) {
			orphan := f.snapshot.ReviewComments[0]
			orphan.ID++
			orphan.ReviewID++
			f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, orphan)
		},
		"partial source": func(f *hfFixture) { f.snapshot.Complete = false },
		"quoted batch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "> " + strings.ReplaceAll(f.snapshot.Reviews[0].Body, "\n", "\n> ")
		},
		"missing batch": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "batch=1/1", "batch=1/2", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := omittedInlineFixture(t)
			change(&f)
			got := f.extract(t)
			if got.Complete || got.Publication.State == "attributed" {
				t.Fatal("unproven source became attributed", got.Publication.State, got.Reason)
			}
		})
	}
}

func TestExternalHostedOmittedCountPositiveInlineInventory(t *testing.T) {
	f := omittedInlineFixture(t)
	f.snapshot.IssueComments = []IssueComment{}
	got := externalDiscover(t, f)
	if len(got) != 1 || !got[0].Inventory.Complete || len(got[0].Inventory.Findings) != 1 || got[0].Inventory.Findings[0].CommentID != 501 || got[0].TriggerCommentID != 0 || got[0].NativeOperationID != "" {
		t.Fatal("external positive source lost or invented native credit", got)
	}
	for name, change := range map[string]func(*hfFixture){
		"empty": func(f *hfFixture) { f.snapshot.ReviewComments = []ReviewComment{} },
		"malformed count": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: unknown**\n" + f.snapshot.Reviews[0].Body
		},
		"explicit zero": func(f *hfFixture) {
			f.snapshot.Reviews[0].Body = "**Actionable comments posted: 0**\n" + f.snapshot.Reviews[0].Body
		},
		"unsupported root": func(f *hfFixture) { f.snapshot.ReviewComments[0].Body = "Unsupported root" },
		"orphan": func(f *hfFixture) {
			root := f.snapshot.ReviewComments[0]
			root.ID++
			root.ReviewID = 999
			f.snapshot.ReviewComments = append(f.snapshot.ReviewComments, root)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := omittedInlineFixture(t)
			f.snapshot.IssueComments = []IssueComment{}
			change(&f)
			runs := externalDiscover(t, f)
			if len(runs) != 1 || runs[0].Inventory.Complete {
				t.Fatal("unproven external source became complete", runs)
			}
		})
	}
}
