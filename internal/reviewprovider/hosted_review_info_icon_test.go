package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

func syntheticReviewInfoIcon() string {
	return `<summary><picture><img src="https://assets.example.test/icons/info.svg" width="16" height="16" align="absmiddle" alt=""></picture> Review info</summary>`
}

func TestHostedFindingReviewInfoIconPreservesExactInventory(t *testing.T) {
	for _, header := range []string{
		syntheticReviewInfoIcon(),
		strings.ReplaceAll(syntheticReviewInfoIcon(), `"16"`, `"24"`),
	} {
		f := hfNew(t, []string{hfInline(1)}, "<details>\n"+header+"\n<dl><dd>\nSynthetic provider metadata.\n</dd></dl>\n</details>")
		before := append([]HostedReview(nil), f.snapshot.Reviews...)
		got := hfWant(t, f, true, 1)
		if got.Findings[0].SourceKey != "hosted-comment:501:000000000000000000000001" || got.Findings[0].Title != "Retain the envelope." || !strings.HasSuffix(got.Findings[0].Detail, hfMarker(1)) || !reflect.DeepEqual(before, f.snapshot.Reviews) {
			t.Fatal("presentation changed identity, finding bytes or raw summary", got)
		}
		// A recognized metadata header cannot hide an extra finding marker.
		f.snapshot.Reviews[0].Body = strings.Replace(f.snapshot.Reviews[0].Body, "Synthetic provider metadata.", hfMarker(2), 1)
		if got = f.extract(t); got.Complete || got.Reason != "finding_format_unsupported" {
			t.Fatal("metadata decoration concealed a visible finding", got)
		}
	}
}

func TestHostedFindingReviewInfoIconRejectsUnknownStructures(t *testing.T) {
	valid := syntheticReviewInfoIcon()
	for name, header := range map[string]string{
		"other label":          strings.Replace(valid, " Review info", " Review findings", 1),
		"example label":        strings.Replace(valid, " Review info", " Review info example", 1),
		"unknown wrapper":      strings.ReplaceAll(strings.ReplaceAll(valid, "<picture>", "<div>"), "</picture>", "</div>"),
		"broken picture":       strings.Replace(valid, "</picture>", "", 1),
		"extra image":          strings.Replace(valid, "</picture>", `<img src="https://assets.example.test/extra.svg"></picture>`, 1),
		"nonempty alternative": strings.Replace(valid, `alt=""`, `alt="Extra finding"`, 1),
		"unknown attribute":    strings.Replace(valid, `alt=""`, `alt="" onclick="unknown"`, 1),
		"active source":        strings.Replace(valid, "https://assets.example.test/icons/info.svg", "javascript:unknown", 1),
		"trailer":              valid + " Additional findings",
	} {
		t.Run(name, func(t *testing.T) {
			f := hfNew(t, []string{hfInline(1)}, "<details>\n"+header+"\nUnclassified source content.\n</details>")
			if got := f.extract(t); got.Complete {
				t.Fatal("unknown summary became complete", got)
			}
		})
	}
}
