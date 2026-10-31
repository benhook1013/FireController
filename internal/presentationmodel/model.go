// Package presentationmodel owns project appearance values without storage or transport dependencies.
package presentationmodel

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/strictjson"
)

const MaxConfigBytes = 16 << 10

// Config retains explicit overrides only; missing map keys use reference defaults.
type Config struct {
	Name                string            `json:"name"`
	BrandName           string            `json:"brand_name"`
	Tagline             string            `json:"tagline"`
	Headings            map[string]string `json:"headings"`
	Theme               map[string]string `json:"theme"`
	ReviewGroups        []ReviewGroup     `json:"review_groups,omitempty"`
	SuggestedMaxPRFiles int64             `json:"suggested_max_pr_files,omitempty"`
	WorkerOrder         []string          `json:"worker_order,omitempty"`
}

// ReviewGroup labels explicitly selected PRs without changing review order or policy.
type ReviewGroup struct {
	Title       string  `json:"title"`
	PullNumbers []int64 `json:"pull_numbers"`
}

type Reference struct {
	BrandName string            `json:"brand_name"`
	Tagline   string            `json:"tagline"`
	Headings  map[string]string `json:"headings"`
	Theme     map[string]string `json:"theme"`
}

//go:embed defaults.json
var defaultsJSON []byte
var reference = func() Reference {
	var r Reference
	if err := json.Unmarshal(defaultsJSON, &r); err != nil {
		panic("invalid presentation defaults")
	}
	r.BrandName = "FireController"
	r.Tagline = "Project workspace"
	return r
}()

// Defaults returns independent copies of the shared reference values.
func Defaults() Reference {
	return Reference{reference.BrandName, reference.Tagline, copyMap(reference.Headings), copyMap(reference.Theme)}
}
func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func invalid() error {
	return errors.New("use a bounded project presentation with valid text, keys, colors and contrast")
}
func named(s string, n int) bool {
	return s != "" && len(s) <= n && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

// Normalize copies maps, trims text and validates only contrast pairs affected by
// explicit overrides. An explicit reference-colored override is still explicit.
func Normalize(in Config) (Config, error) {
	for _, s := range []string{in.Name, in.BrandName, in.Tagline} {
		if !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
			return Config{}, invalid()
		}
	}
	out := Config{Name: strings.TrimSpace(in.Name), BrandName: strings.TrimSpace(in.BrandName), Tagline: strings.TrimSpace(in.Tagline), Headings: copyMap(in.Headings), Theme: copyMap(in.Theme)}
	if in.SuggestedMaxPRFiles < 0 || in.SuggestedMaxPRFiles > 100000 {
		return Config{}, invalid()
	}
	out.SuggestedMaxPRFiles = in.SuggestedMaxPRFiles
	if len(in.WorkerOrder) > 100 {
		return Config{}, invalid()
	}
	seenWorkers := map[string]bool{}
	for _, id := range in.WorkerOrder {
		if len(id) == 0 || len(id) > 128 || seenWorkers[id] || strings.IndexFunc(id, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
		}) >= 0 {
			return Config{}, invalid()
		}
		seenWorkers[id] = true
		out.WorkerOrder = append(out.WorkerOrder, id)
	}
	if len(in.ReviewGroups) > 40 {
		return Config{}, invalid()
	}
	titles, pulls := map[string]bool{}, map[int64]bool{}
	for _, group := range in.ReviewGroups {
		title := strings.TrimSpace(group.Title)
		if !named(title, 160) || titles[title] || len(group.PullNumbers) == 0 {
			return Config{}, invalid()
		}
		titles[title] = true
		copy := ReviewGroup{Title: title, PullNumbers: append([]int64(nil), group.PullNumbers...)}
		for _, number := range copy.PullNumbers {
			if number < 1 || number > 2147483647 || pulls[number] {
				return Config{}, invalid()
			}
			pulls[number] = true
			if len(pulls) > 1000 {
				return Config{}, invalid()
			}
		}
		out.ReviewGroups = append(out.ReviewGroups, copy)
	}
	if out.BrandName == "" {
		out.BrandName = reference.BrandName
	}
	if out.Tagline == "" {
		out.Tagline = reference.Tagline
	}
	if !named(out.Name, 200) || !named(out.BrandName, 120) || !named(out.Tagline, 120) {
		return Config{}, invalid()
	}
	for k, v := range out.Headings {
		if _, ok := reference.Headings[k]; !ok {
			return Config{}, invalid()
		}
		if !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return Config{}, invalid()
		}
		v = strings.TrimSpace(v)
		if !named(v, 240) {
			return Config{}, invalid()
		}
		out.Headings[k] = v
	}
	for k, v := range out.Theme {
		if _, ok := reference.Theme[k]; !ok || len(v) != 7 || v[0] != '#' {
			return Config{}, invalid()
		}
		if strings.IndexFunc(v[1:], func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') }) >= 0 {
			return Config{}, invalid()
		}
		if _, err := strconv.ParseUint(v[1:], 16, 24); err != nil {
			return Config{}, invalid()
		}
		out.Theme[k] = strings.ToLower(v)
	}
	pair := func(a, b string, min float64) bool {
		av, ac := out.Theme[a]
		bv, bc := out.Theme[b]
		if !ac && !bc {
			return true
		}
		if !ac {
			av = reference.Theme[a]
		}
		if !bc {
			bv = reference.Theme[b]
		}
		return contrast(av, bv) >= min
	}
	for _, family := range []string{"canvas", "surface", "header", "hero"} {
		for _, fg := range []string{"text", "muted"} {
			if !pair(family+"_"+fg, family+"_background", 4.5) {
				return Config{}, invalid()
			}
		}
	}
	for _, family := range []string{"active", "blocked", "parked", "completed", "merged"} {
		if !pair(family+"_text", family+"_background", 4.5) {
			return Config{}, invalid()
		}
	}
	if !pair("merged_rail_text", "merged_rail", 4.5) {
		return Config{}, invalid()
	}
	if !pair("accent_text", "accent_background", 4.5) || !pair("accent_background", "canvas_background", 4.5) || !pair("accent_background", "surface_background", 4.5) {
		return Config{}, invalid()
	}
	for _, fg := range []string{"surface_text", "surface_muted", "accent_background"} {
		if !pair(fg, "accent_soft_background", 4.5) {
			return Config{}, invalid()
		}
	}
	for _, bg := range []string{"canvas_background", "surface_background", "header_background", "hero_background", "accent_background", "accent_soft_background", "active_background", "blocked_background", "parked_background", "completed_background"} {
		if !pair("focus", bg, 3) {
			return Config{}, invalid()
		}
	}
	// New palette roles must not invalidate retained themes that predate them.
	for _, background := range []string{"merged_background", "merged_rail"} {
		if _, explicit := out.Theme[background]; explicit && !pair("focus", background, 3) {
			return Config{}, invalid()
		}
	}
	raw, err := json.Marshal(out)
	if err != nil || len(raw) > MaxConfigBytes {
		return Config{}, invalid()
	}
	return out, nil
}
func luminance(color string) float64 {
	n, _ := strconv.ParseUint(color[1:], 16, 24)
	channel := func(v uint64) float64 {
		x := float64(v) / 255
		if x <= 0.04045 {
			return x / 12.92
		}
		return math.Pow((x+0.055)/1.055, 2.4)
	}
	return .2126*channel((n>>16)&255) + .7152*channel((n>>8)&255) + .0722*channel(n&255)
}
func contrast(a, b string) float64 {
	x, y := luminance(a), luminance(b)
	if x < y {
		x, y = y, x
	}
	return (x + .05) / (y + .05)
}

// Decode rejects unknown/duplicate fields, malformed Unicode and explicit scalar
// nulls before normalization. It accepts omitted optional values as defaults.
func Decode(raw []byte) (Config, error) {
	var c Config
	if len(raw) > 256<<10 || strictjson.Decode(raw, &c) != nil {
		return Config{}, invalid()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return Config{}, invalid()
	}
	if _, ok := fields["name"]; !ok {
		return Config{}, invalid()
	}
	for _, key := range []string{"review_groups", "worker_order", "suggested_max_pr_files"} {
		if value, ok := fields[key]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Config{}, invalid()
		}
	}
	for _, key := range []string{"name", "brand_name", "tagline"} {
		if v, ok := fields[key]; ok {
			var text string
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, &text) != nil || strings.TrimSpace(text) == "" {
				return Config{}, invalid()
			}
		}
	}
	for _, key := range []string{"headings", "theme"} {
		var values map[string]json.RawMessage
		if v, ok := fields[key]; ok {
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return Config{}, invalid()
			}
			if json.Unmarshal(v, &values) != nil {
				return Config{}, invalid()
			}
			for _, s := range values {
				if bytes.Equal(bytes.TrimSpace(s), []byte("null")) {
					return Config{}, invalid()
				}
			}
		}
	}
	return Normalize(c)
}
