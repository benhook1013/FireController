package reviewprovider

import (
	"reflect"
	"strings"
	"testing"
)

func TestHostedAdditionalDecorativeIconsPreserveInventory(t *testing.T) {
	for _, dark := range []bool{false, true} {
		icon := standingTestPicture(dark)
		item := strings.Replace(hfAdditionalItem("24-24", "Retain the optional observation."), "**Trivial**", "**"+icon+" Trivial**", 1)
		body := hfAdditionalCategory(1, hfAdditionalGroup("source/file.go", "24-24", 1, item))
		body = strings.Replace(body, "🔇 Additional comments:", icon+" Additional comments:", 1)
		body += "\n\n<details>\n<summary>" + icon + " Review info</summary>\n<dl>\n<dd>\nSynthetic provider metadata.\n</dd>\n</dl>\n</details>"
		f := hfNew(t, []string{hfInline(1)}, body)
		before := append([]HostedReview(nil), f.snapshot.Reviews...)
		got := hfWant(t, f, true, 2)
		if got.Findings[1].Detail != item || got.Findings[1].Severity != "info" || got.Findings[1].Kind != "additional" || !reflect.DeepEqual(before, f.snapshot.Reviews) {
			t.Fatal("decorative icon changed original inventory", got)
		}
	}
}

func TestHostedAdditionalIconsCannotHideUnknownInventory(t *testing.T) {
	icon := standingTestPicture(true)
	body := strings.Replace(hfAdditionalCategory(1, hfAdditionalGroup("source/file.go", "24-24", 1, hfAdditionalItem("24-24", "Retain the observation."))), "🔇", icon, 1)
	for name, bad := range map[string]string{
		"wrong label":       strings.Replace(body, "Additional comments:", "Other comments:", 1),
		"unknown attribute": strings.Replace(body, `alt=""`, `alt="" title="extra"`, 1),
		"nonempty alt":      strings.Replace(body, `alt=""`, `alt="hidden finding"`, 1),
		"unknown source":    strings.Replace(body, `(prefers-color-scheme: dark)`, `other`, 1),
		"extra picture":     strings.Replace(body, icon, icon+icon, 1),
		"count mismatch":    strings.Replace(body, "Additional comments: 1", "Additional comments: 2", 1),
		"unknown wrapper":   strings.Replace(body, "<dl>", "<section>", 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := hfNew(t, []string{hfInline(1)}, bad)
			if f.extract(t).Complete {
				t.Fatal("unknown inventory accepted")
			}
		})
	}
}
