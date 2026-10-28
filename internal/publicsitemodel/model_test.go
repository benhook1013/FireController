package publicsitemodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/presentationmodel"
)

func approvedManifest() Manifest {
	return Manifest{
		Slug:         "public-demo",
		Presentation: presentationmodel.Config{Name: "Approved project", Tagline: "Public progress"},
		Description:  "Approved public description.",
		Links:        []Link{{Key: "source", Label: "Project source", URL: "https://example.com/project?view=public#overview"}},
		ProjectMap:   []Card{{Key: "foundation", Title: "Foundation", Description: "Public milestone", Phase: "First release", Status: "active", Links: []Link{{Key: "guide", Label: "Guide", URL: "http://example.com/guide"}}}},
		Progress:     []Card{{Key: "release", Title: "Release", Status: "planned"}},
		WorkerLanes:  []WorkerLane{{Key: "build", Label: "Build lane", Headline: "Approved assignment", Status: "active", Milestones: []string{"Public milestone"}}},
		Reviews:      []EnrolledReview{{Number: 9, Title: "Approved old PR title", URL: "https://github.com/example/project/pull/9"}, {Number: 2, Title: "Approved current PR title", URL: "https://github.com/example/project/pull/2"}},
	}
}

func publicRoundFact(index int) RoundFacts {
	return RoundFacts{Key: fmt.Sprintf("public_round_%032x", index), Completed: true, Finalized: true, Complete: true, Found: 3, Accepted: 1, Routed: 1, ObservedAt: "2026-10-17T01:00:00Z"}
}

func encodedContent(t *testing.T, manifest Manifest, facts []ReviewFacts) []byte {
	t.Helper()
	content, err := BuildContent(manifest, facts)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPublicContentAllowlistEnrollmentAndMergedHistory(t *testing.T) {
	manifest := approvedManifest()
	duration := int64(1200)
	first, second := publicRoundFact(1), publicRoundFact(2)
	first.DurationMilliseconds = &duration
	second.ObservedAt = "2026-10-16T01:00:00Z"
	unknown := publicRoundFact(3)
	unknown.ObservedAt = ""
	unknown.Complete = false
	private := RoundFacts{Key: "PRIVATE_ATTEMPT_ID", Found: -1, ObservedAt: "PRIVATE_WORKSPACE_TIME"}
	facts := []ReviewFacts{
		{Number: 2, Lifecycle: "open", CLI: []RoundFacts{first, private, second}, Hosted: []RoundFacts{unknown}},
		{Number: 77, Lifecycle: "PRIVATE_LIFECYCLE", CLI: []RoundFacts{{Key: "PRIVATE_RUN_ID", Completed: true, Finalized: true, Found: -1}}},
		{Number: 9, Lifecycle: "merged"},
	}
	content, err := BuildContent(manifest, facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(content.Reviews) != 2 || content.Reviews[0].Number != 9 || content.Reviews[0].Lifecycle != "merged" || content.Reviews[1].Number != 2 || content.Reviews[0].Title != manifest.Reviews[0].Title || content.Reviews[1].URL != manifest.Reviews[1].URL {
		t.Fatal("enrollment order, frozen labels or merged history changed", content.Reviews)
	}
	current := content.Reviews[1]
	if len(current.CLI) != 2 || current.CLI[0].Key != second.Key || current.CLI[1].Key != first.Key || len(current.Hosted) != 1 {
		t.Fatal("channel chronology or distinct equal-count rounds lost", current)
	}
	if current.Hosted[0].ObservedAt != "" || current.Hosted[0].DurationMilliseconds != nil || current.Hosted[0].Complete {
		t.Fatal("unknown time/duration or partial label invented", current.Hosted[0])
	}
	raw, _ := json.Marshal(content)
	for _, forbidden := range []string{"PRIVATE_", "actor", "attempt", "snapshot", "configuration", "taper", "readiness", "coverage", "recorded_at", "updated_at", "revision"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatal("private state or inferred policy exposed", forbidden, string(raw))
		}
	}
	// The DTO itself is an allowlist: review outputs have no field capable of
	// carrying a changed source title, source link, actor or raw finding body.
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire) != 8 {
		t.Fatal("unexpected public top-level fields", string(raw), err)
	}
	var reviews []map[string]json.RawMessage
	if err := json.Unmarshal(wire["reviews"], &reviews); err != nil || len(reviews[1]) != 6 {
		t.Fatal("unexpected review fields", reviews, err)
	}
	var rounds []map[string]json.RawMessage
	if err := json.Unmarshal(reviews[1]["cli"], &rounds); err != nil || len(rounds[0]) != 7 {
		t.Fatal("unexpected round fields", rounds, err)
	}
	// Caller mutation after construction cannot rewrite an approved projection.
	manifest.ProjectMap[0].Links[0].Label = "PRIVATE_LATER_LINK"
	manifest.WorkerLanes[0].Milestones[0] = "PRIVATE_LATER_MILESTONE"
	duration = 9999
	if content.ProjectMap[0].Links[0].Label == "PRIVATE_LATER_LINK" || content.WorkerLanes[0].Milestones[0] == "PRIVATE_LATER_MILESTONE" || *current.CLI[1].DurationMilliseconds != 1200 {
		t.Fatal("public content aliases caller input")
	}
}

func TestPublicContentPrivateActivityDoesNotChangeBytes(t *testing.T) {
	manifest := approvedManifest()
	one, two := publicRoundFact(1), publicRoundFact(2)
	baseline := []ReviewFacts{{Number: 2, Lifecycle: "open", CLI: []RoundFacts{one, two}}}
	before := encodedContent(t, manifest, baseline)
	changed := []ReviewFacts{
		{Number: 999, Lifecycle: "private-status-changed", Hosted: []RoundFacts{{Key: "PRIVATE_CAPTURE", Completed: true, Finalized: true, Found: -7}}},
		{Number: 2, Lifecycle: "open", CLI: []RoundFacts{{Key: "PRIVATE_ACTIVE_1", Completed: false, Finalized: true}, {Key: "PRIVATE_DRAFT_2", Completed: true, Finalized: false}, two, one}},
	}
	if after := encodedContent(t, manifest, changed); !bytes.Equal(before, after) {
		t.Fatalf("private activity or source input order changed public bytes:\n%s\n%s", before, after)
	}
	// An eligible public round is a real public change even with identical counts.
	changed[1].CLI = append(changed[1].CLI, publicRoundFact(3))
	if bytes.Equal(before, encodedContent(t, manifest, changed)) {
		t.Fatal("distinct completed public round was deduplicated by counts")
	}
}

func TestPublicContentStrictCanonicalDecode(t *testing.T) {
	raw := encodedContent(t, approvedManifest(), []ReviewFacts{{Number: 2, CLI: []RoundFacts{publicRoundFact(1)}}})
	content, err := DecodeContent(raw)
	if err != nil || content.Reviews[1].CLI[0].Found != 3 {
		t.Fatal("canonical public projection rejected", err)
	}
	if !reflect.DeepEqual(EditorialManifest(content), mustNormalizeManifest(t, approvedManifest())) {
		t.Fatal("editorial selection changed")
	}
	for _, bad := range [][]byte{
		append([]byte(" "), raw...),
		bytes.Replace(raw, []byte(`"duration_milliseconds":null`), []byte(`"duration_milliseconds":-1`), 1),
		bytes.Replace(raw, []byte(`"public_round_00000000000000000000000000000001"`), []byte(`"review_run_private"`), 1),
		bytes.Replace(raw, []byte(`"slug":"public-demo"`), []byte(`"slug":"public-demo","private_snapshot":"sentinel"`), 1),
		bytes.Replace(raw, []byte(`"project_map":[`), []byte(`"project_map":null,"unused":[`), 1),
	} {
		if _, err := DecodeContent(bad); err == nil {
			t.Fatal("noncanonical or unsafe content accepted")
		}
	}
}

func mustNormalizeManifest(t *testing.T, m Manifest) Manifest {
	t.Helper()
	out, err := NormalizeManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPublicManifestLiteralTextAndStrictDecode(t *testing.T) {
	manifest := approvedManifest()
	markup := `<script>alert("literal public text")</script>`
	manifest.Description = markup
	manifest.Presentation.Headings = map[string]string{"hero_title": markup}
	manifest.ProjectMap[0].Title = markup
	normalized, err := NormalizeManifest(manifest)
	if err != nil || normalized.Description != markup || normalized.Presentation.Headings["hero_title"] != markup || normalized.ProjectMap[0].Title != markup {
		t.Fatal("literal markup was stripped or interpreted", normalized, err)
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(raw)
	if err != nil || decoded.Description != markup {
		t.Fatal("safe plain text did not survive strict wire", decoded, err)
	}
	minimal, err := DecodeManifest([]byte(`{"slug":"minimal","presentation":{"name":"Public project"}}`))
	if err != nil || minimal.Links == nil || minimal.Reviews == nil || minimal.Presentation.BrandName != "FireController" {
		t.Fatal("explicit minimal selection failed normalization", minimal, err)
	}
	for _, bad := range []string{
		`{"slug":"minimal","slug":"other","presentation":{"name":"Public project"}}`,
		`{"Slug":"minimal","presentation":{"name":"Public project"}}`,
		`{"slug":"minimal","presentation":{"name":"Public project"},"private_snapshot_id":"PRIVATE"}`,
		`{"slug":"minimal","presentation":{"name":"Public project","css":"color:red"}}`,
		`{"slug":"minimal","presentation":{"name":"Public project","theme":{"canvas_background":"url(javascript:alert(1))"}}}`,
		`{"slug":"minimal","presentation":{"name":"Public project"},"description":null}`,
		`{"slug":"minimal","presentation":{"name":"Public project"},"reviews":null}`,
		`{"slug":"minimal","presentation":{"name":"Public project","theme":{"canvas_text":null}}}`,
		`{"slug":"minimal","presentation":{"name":"\ud800"}}`,
		strings.Repeat(" ", MaxManifestBytes) + `{}`,
	} {
		if _, err := DecodeManifest([]byte(bad)); err == nil {
			t.Fatal("unsafe or malformed public manifest accepted", bad[:min(len(bad), 160)])
		}
	}
}

func TestPublicManifestBoundsDuplicatesAndApprovedURLs(t *testing.T) {
	for name, change := range map[string]func(*Manifest){
		"bad-slug":            func(m *Manifest) { m.Slug = "../private" },
		"long-slug":           func(m *Manifest) { m.Slug = strings.Repeat("a", 65) },
		"duplicate-card-key":  func(m *Manifest) { m.Progress[0].Key = m.ProjectMap[0].Key },
		"duplicate-lane-key":  func(m *Manifest) { m.WorkerLanes[0].Key = m.ProjectMap[0].Key },
		"duplicate-link-key":  func(m *Manifest) { m.Links = append(m.Links, m.Links[0]) },
		"duplicate-review":    func(m *Manifest) { m.Reviews = append(m.Reviews, m.Reviews[0]) },
		"long-title":          func(m *Manifest) { m.Reviews[0].Title = strings.Repeat("x", 241) },
		"long-description":    func(m *Manifest) { m.Description = strings.Repeat("x", (8<<10)+1) },
		"control-headline":    func(m *Manifest) { m.WorkerLanes[0].Headline = "hidden\x00private" },
		"too-many-cards":      func(m *Manifest) { m.ProjectMap = make([]Card, MaxCards+1) },
		"too-many-lanes":      func(m *Manifest) { m.WorkerLanes = make([]WorkerLane, MaxWorkerLanes+1) },
		"too-many-reviews":    func(m *Manifest) { m.Reviews = make([]EnrolledReview, MaxReviews+1) },
		"too-many-links":      func(m *Manifest) { m.Links = make([]Link, MaxLinks+1) },
		"too-many-milestones": func(m *Manifest) { m.WorkerLanes[0].Milestones = make([]string, MaxMilestones+1) },
		"encoded-not-raw-byte-bound": func(m *Manifest) {
			m.ProjectMap = make([]Card, 6)
			for i := range m.ProjectMap {
				m.ProjectMap[i] = Card{Key: fmt.Sprintf("card-%d", i), Title: "Public", Description: strings.Repeat("<", 8<<10)}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := approvedManifest()
			change(&m)
			if _, err := NormalizeManifest(m); err == nil {
				t.Fatal("invalid public selection accepted")
			}
		})
	}
	for _, unsafe := range []string{"javascript:alert(1)", "data:text/html,private", "//example.com/private", "https://user:password@example.com/private", `https://example.com\private`, "https://example.com/\nprivate", "file:///private", "https:///missing-host"} {
		m := approvedManifest()
		m.Links[0].URL = unsafe
		if _, err := NormalizeManifest(m); err == nil {
			t.Fatal("unsafe public URL accepted", unsafe)
		}
	}
	for _, unsafe := range []string{"http://github.com/example/project/pull/9", "https://github.com.evil/example/project/pull/9", "https://github.com/example/project/pull/2", "https://github.com/example/project/pull/9?token=private", "https://github.com/example/project/pull/9#private", "https://github.com/example%2fprivate/project/pull/9"} {
		m := approvedManifest()
		m.Reviews[0].URL = unsafe
		if _, err := NormalizeManifest(m); err == nil {
			t.Fatal("wrong PR destination accepted", unsafe)
		}
	}
}

func TestPublicContentRejectsInvalidEligibleFactsWithoutInventingUnknowns(t *testing.T) {
	for name, change := range map[string]func(*RoundFacts){
		"source-id":         func(r *RoundFacts) { r.Key = "review_run_private" },
		"negative-found":    func(r *RoundFacts) { r.Found = -1 },
		"inexact-found":     func(r *RoundFacts) { r.Found = 1 << 53 },
		"impossible-counts": func(r *RoundFacts) { r.Accepted = r.Found; r.Routed = 1 },
		"bad-time":          func(r *RoundFacts) { r.ObservedAt = "PRIVATE_SNAPSHOT_TIME" },
		"utc-year-overflow": func(r *RoundFacts) { r.ObservedAt = "9999-12-31T23:59:59-01:00" },
		"negative-duration": func(r *RoundFacts) { v := int64(-1); r.DurationMilliseconds = &v },
	} {
		t.Run(name, func(t *testing.T) {
			r := publicRoundFact(1)
			change(&r)
			if _, err := BuildContent(approvedManifest(), []ReviewFacts{{Number: 2, CLI: []RoundFacts{r}}}); err == nil {
				t.Fatal("invalid eligible fact accepted")
			}
		})
	}
	for name, facts := range map[string][]ReviewFacts{
		"duplicate-facts":   {{Number: 2}, {Number: 2}},
		"duplicate-rounds":  {{Number: 2, CLI: []RoundFacts{publicRoundFact(1)}, Hosted: []RoundFacts{publicRoundFact(1)}}},
		"unknown-lifecycle": {{Number: 2, Lifecycle: "PRIVATE_INTERNAL_ATTEMPT"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildContent(approvedManifest(), facts); err == nil {
				t.Fatal("ambiguous public facts accepted")
			}
		})
	}
	content, err := BuildContent(approvedManifest(), nil)
	if err != nil || content.Reviews[0].Lifecycle != "unknown" || len(content.Reviews[0].CLI) != 0 {
		t.Fatal("missing facts invented lifecycle or reviews", content, err)
	}
	manifest := approvedManifest()
	manifest.ProjectMap = make([]Card, 5)
	for i := range manifest.ProjectMap {
		manifest.ProjectMap[i] = Card{Key: fmt.Sprintf("card-%d", i), Title: "Public", Description: strings.Repeat("<", 8<<10)}
	}
	if _, err := NormalizeManifest(manifest); err != nil {
		t.Fatal("fixture exceeds manifest before public facts", err)
	}
	rounds := make([]RoundFacts, MaxChannelRounds)
	for i := range rounds {
		rounds[i] = publicRoundFact(i + 1)
	}
	if _, err := BuildContent(manifest, []ReviewFacts{{Number: 2, CLI: rounds}}); err == nil {
		t.Fatal("encoded projection exceeded its full byte bound")
	}
	if _, err := BuildContent(approvedManifest(), []ReviewFacts{{Number: 2, CLI: append(rounds, publicRoundFact(MaxChannelRounds+1))}}); err == nil {
		t.Fatal("unbounded round history accepted")
	}
}

func TestPublicManifestRejectsPrivateReviewGroups(t *testing.T) {
	m := approvedManifest()
	m.Presentation.ReviewGroups = []presentationmodel.ReviewGroup{{Title: "Private operations", PullNumbers: []int64{7}}}
	if _, err := NormalizeManifest(m); err == nil {
		t.Fatal("private queue groups entered public manifest")
	}
}

func TestPublicManifestRejectsPrivateWorkerOrder(t *testing.T) {
	m := approvedManifest()
	m.Presentation.WorkerOrder = []string{"worker_private"}
	if _, err := NormalizeManifest(m); err == nil {
		t.Fatal("private worker order accepted for publication")
	}
}
