package presentationmodel

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeDefaultsStrictInputAndCopies(t *testing.T) {
	c, err := Decode([]byte(`{"name":"  Workspace  "}`))
	if err != nil || c.Name != "Workspace" || c.BrandName != "FireController" || c.Tagline != "Project workspace" || c.Headings == nil || c.Theme == nil {
		t.Fatalf("defaults: %#v %v", c, err)
	}
	for _, raw := range []string{`null`, `{}`, `{"name":null}`, `{"Name":"one"}`, `{"name":"one","name":"two"}`, `{"name":"one","unknown":1}`, `{"name":"one","brand_name":" "}`, `{"name":"one","tagline":null}`, `{"name":"one","headings":null}`, `{"name":"one","theme":null}`, `{"name":"one","headings":{"jobs":null}}`, `{"name":"one","headings":{"other":"x"}}`, `{"name":"one","theme":{"color":"#000000"}}`, `{"name":"one","theme":{"canvas_text":"#+00000"}}`, `{"name":"one","theme":{"canvas_text":"#000"}}`, `{"name":"one","headings":{"jobs":"a\nb"}}`, `{"name":"one","headings":{"jobs":"\ud800"}}`, `{"name":"one","brand_name":"\nBrand"}`} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	in := Config{Name: "  Name  ", Headings: map[string]string{"jobs": "  Work  "}, Theme: map[string]string{"canvas_text": "#00000A"}}
	before, _ := json.Marshal(in)
	got, err := Normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) || got.Headings["jobs"] != "Work" || got.Theme["canvas_text"] != "#00000a" {
		t.Fatalf("normalization mutated input or failed: %#v", got)
	}
	got.Headings["jobs"] = "changed"
	if in.Headings["jobs"] != "  Work  " {
		t.Fatal("maps alias caller")
	}
	refs := Defaults()
	refs.Theme["canvas_text"] = "broken"
	if Defaults().Theme["canvas_text"] != "#252833" {
		t.Fatal("reference leaked")
	}
	for _, c := range []Config{{Name: strings.Repeat("雪", 67)}, {Name: "a\x00"}, {Name: "one", Tagline: strings.Repeat("x", 121)}, {Name: "one", Headings: map[string]string{"jobs": strings.Repeat("x", 241)}}} {
		if _, e := Normalize(c); e == nil {
			t.Error("accepted out of bounds", c)
		}
	}
}
func TestChangedPairContrast(t *testing.T) {
	if contrast("#000000", "#ffffff") != 21 {
		t.Fatal("sRGB contrast")
	}
	if _, err := Normalize(Config{Name: "one"}); err != nil {
		t.Fatal("unchanged references exempt", err)
	}
	// Explicit reference values remain overrides and cannot bypass checks by
	// matching the existing appearance. Its reference focus has low contrast.
	if _, err := Normalize(Config{Name: "one", Theme: map[string]string{"focus": Defaults().Theme["focus"]}}); err == nil {
		t.Fatal("explicit low-contrast reference focus accepted")
	}
	for _, theme := range []map[string]string{{"canvas_text": "#e8e7e4"}, {"surface_muted": "#f7f6f3"}, {"header_text": "#a21f29"}, {"hero_muted": "#a11c26"}, {"active_text": "#f3dcd4"}, {"accent_text": "#a51e28"}, {"accent_background": "#e8e7e4"}} {
		if _, err := Normalize(Config{Name: "one", Theme: theme}); err == nil {
			t.Errorf("low contrast accepted %#v", theme)
		}
	}
	theme := map[string]string{"focus": "#808080", "accent_background": "#ffffff", "accent_text": "#000000", "accent_soft_background": "#000000"}
	for _, f := range []string{"canvas", "surface", "header", "hero", "active", "blocked", "parked", "completed"} {
		theme[f+"_background"] = "#000000"
		theme[f+"_text"] = "#ffffff"
		if f == "canvas" || f == "surface" || f == "header" || f == "hero" {
			theme[f+"_muted"] = "#ffffff"
		}
	}
	c, err := Normalize(Config{Name: "one", Theme: theme})
	if err != nil || !reflect.DeepEqual(c.Theme, theme) {
		t.Fatalf("valid complete theme: %v", err)
	}
}

func TestSoftAccentSurfaceRequiresReadableTextAndLinks(t *testing.T) {
	for _, theme := range []map[string]string{
		{"accent_soft_background": "#000000"},
		{"accent_soft_background": "#ffffff", "surface_text": "#ffffff"},
		{"accent_soft_background": "#a51e28"},
	} {
		if _, err := Normalize(Config{Name: "one", Theme: theme}); err == nil {
			t.Fatalf("unreadable selected/hover surface accepted: %v", theme)
		}
	}
}
