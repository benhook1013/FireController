// Package publicsitemodel owns explicitly approved anonymous project content. It
// deliberately has no dependency on authenticated coordination/review DTOs.
package publicsitemodel

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/strictjson"
)

const (
	MaxManifestBytes = 256 << 10
	MaxContentBytes  = 256 << 10
	MaxReviews       = 100
	MaxCards         = 100 // independently for map and progress
	MaxWorkerLanes   = 50
	MaxLinks         = 12 // per selected link collection, matching project maps
	MaxMilestones    = 20
	MaxChannelRounds = 100
	// Counts and milliseconds must remain exact in browser JSON numbers.
	maxJSONInteger = 1<<53 - 1
)

var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var roundKeyPattern = regexp.MustCompile(`^public_round_[a-f0-9]{32}$`)
var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
var jobIDPattern = regexp.MustCompile(`^job_[a-f0-9]{32}$`)
var mapEntryIDPattern = regexp.MustCompile(`^map_entry_[a-f0-9]{32}$`)
var workerIDPattern = regexp.MustCompile(`^worker_[a-f0-9]{32}$`)

func ValidSlug(slug string) bool { return slugPattern.MatchString(slug) }

// EditorialManifest selects only the already-public editorial values. Callers
// that need independently owned mutable values should normalize the result.
func EditorialManifest(content Content) Manifest {
	m := Manifest{Slug: content.Slug, Presentation: content.Presentation, Description: content.Description, Links: content.Links, ProjectMap: content.ProjectMap, Progress: content.Progress, WorkerLanes: content.WorkerLanes, Reviews: make([]EnrolledReview, len(content.Reviews))}
	for i, review := range content.Reviews {
		m.Reviews[i] = EnrolledReview{Number: review.Number, Title: review.Title, URL: review.URL}
	}
	return m
}

// All keys below are operator-authored public references, never copied private
// source identifiers. Link keys are unique in their collection; card/lane keys
// are unique across the selected map, progress and worker lanes.
type Link struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	URL   string `json:"url"`
}

type Card struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Phase       string `json:"phase"`
	Status      string `json:"status"`
	Links       []Link `json:"links"`
}

type WorkerLane struct {
	Key        string   `json:"key"`
	Label      string   `json:"label"`
	Headline   string   `json:"headline"`
	Status     string   `json:"status"`
	Milestones []string `json:"milestones"`
}

type EnrolledReview struct {
	Number int64  `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
}

type JobRef struct {
	Key             string `json:"key"`
	JobID           string `json:"job_id"`
	SummaryRevision int64  `json:"summary_revision"`
}

type MapRef struct {
	Key      string `json:"key"`
	EntryID  string `json:"entry_id"`
	Revision int64  `json:"revision"`
}

type WorkerRef struct {
	Key      string `json:"key"`
	WorkerID string `json:"worker_id"`
	Label    string `json:"label"`
}

// Manifest is a complete approved editorial selection. It carries no private
// source revision, worker identity, repository binding or publication clock.
type Manifest struct {
	Slug         string                   `json:"slug"`
	Presentation presentationmodel.Config `json:"presentation"`
	Description  string                   `json:"description"`
	Links        []Link                   `json:"links"`
	ProjectMap   []Card                   `json:"project_map"`
	Progress     []Card                   `json:"progress"`
	WorkerLanes  []WorkerLane             `json:"worker_lanes"`
	Reviews      []EnrolledReview         `json:"reviews"`
	JobRefs      []JobRef                 `json:"job_refs,omitempty"`
	MapRefs      []MapRef                 `json:"map_refs,omitempty"`
	WorkerRefs   []WorkerRef              `json:"worker_refs,omitempty"`
}

// RoundFacts is a narrow input boundary for retained review summaries. Key is
// an opaque publication-specific reference supplied by the source adapter, not
// a source run/attempt/snapshot ID. ObservedAt means that round's actual retained
// completion/observation time, never workspace freshness; empty means unknown.
// Completed and Finalized gate eligibility and are not exposed as private state.
type RoundFacts struct {
	Key                  string
	Completed, Finalized bool
	Complete             bool
	Found, Accepted      int64
	Routed               int64
	ObservedAt           string
	DurationMilliseconds *int64
}

type ReviewFacts struct {
	Number                    int64
	Lifecycle                 string
	CLI                       []RoundFacts
	Hosted                    []RoundFacts
	CLIHasMore, HostedHasMore bool
}

type PublicRound struct {
	Key                  string `json:"key"`
	Found                int64  `json:"found"`
	Accepted             int64  `json:"accepted"`
	Routed               int64  `json:"routed"`
	Complete             bool   `json:"complete"`
	ObservedAt           string `json:"observed_at"`
	DurationMilliseconds *int64 `json:"duration_milliseconds"`
}

type PublicReview struct {
	Number        int64         `json:"number"`
	Title         string        `json:"title"`
	URL           string        `json:"url"`
	Lifecycle     string        `json:"lifecycle"`
	CLI           []PublicRound `json:"cli"`
	Hosted        []PublicRound `json:"hosted"`
	CLIHasMore    bool          `json:"cli_has_more,omitempty"`
	HostedHasMore bool          `json:"hosted_has_more,omitempty"`
}

// Content contains only approved editorial values and eligible summary facts.
// Revision/freshness are deliberately owned by the publishing service, which
// can compare this deterministic content before emitting public activity.
type Content struct {
	Slug         string                   `json:"slug"`
	Presentation presentationmodel.Config `json:"presentation"`
	Description  string                   `json:"description"`
	Links        []Link                   `json:"links"`
	ProjectMap   []Card                   `json:"project_map"`
	Progress     []Card                   `json:"progress"`
	WorkerLanes  []WorkerLane             `json:"worker_lanes"`
	Reviews      []PublicReview           `json:"reviews"`
}

func invalidModel() error {
	return errors.New("use bounded explicitly approved public project content")
}

// Text is plain literal content, not HTML, CSS or an instruction to render
// markup. Match the map owner's 8KiB long text and 240-byte headline bounds.
func publicText(s string, limit int, multiline bool, required bool) bool {
	return len(s) <= limit && utf8.ValidString(s) && (!required || strings.TrimSpace(s) != "") && strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsControl(r) && (!multiline || r != '\n' && r != '\t')
	}) < 0
}

func approvedURL(raw string) bool {
	if raw == "" || len(raw) > 2048 || !utf8.ValidString(raw) || strings.Contains(raw, `\`) || strings.IndexFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.Opaque == ""
}

func approvedReviewURL(raw string, number int64) bool {
	if !approvedURL(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	return u.Scheme == "https" && u.Host == "github.com" && u.RawPath == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && len(parts) == 4 && repositoryPart.MatchString(parts[0]) && repositoryPart.MatchString(parts[1]) && parts[0] != "." && parts[0] != ".." && parts[1] != "." && parts[1] != ".." && parts[2] == "pull" && parts[3] == strconv.FormatInt(number, 10)
}

func normalizeLinks(in []Link) ([]Link, error) {
	if len(in) > MaxLinks {
		return nil, invalidModel()
	}
	out := make([]Link, len(in))
	keys := make(map[string]bool, len(in))
	for i, link := range in {
		if !keyPattern.MatchString(link.Key) || keys[link.Key] || !publicText(link.Label, 120, false, true) || !approvedURL(link.URL) {
			return nil, invalidModel()
		}
		keys[link.Key] = true
		out[i] = link
	}
	return out, nil
}

func NormalizeManifest(in Manifest) (Manifest, error) {
	// Queue headings are private operational metadata, not public appearance.
	if len(in.Presentation.ReviewGroups) != 0 || len(in.Presentation.WorkerOrder) != 0 {
		return Manifest{}, invalidModel()
	}
	if !slugPattern.MatchString(in.Slug) || !publicText(in.Description, 8<<10, true, false) || len(in.ProjectMap)+len(in.MapRefs) > MaxCards || len(in.Progress)+len(in.JobRefs) > MaxCards || len(in.WorkerLanes)+len(in.WorkerRefs) > MaxWorkerLanes || len(in.Reviews) > MaxReviews {
		return Manifest{}, invalidModel()
	}
	presentation, err := presentationmodel.Normalize(in.Presentation)
	if err != nil {
		return Manifest{}, invalidModel()
	}
	out := Manifest{Slug: in.Slug, Presentation: presentation, Description: in.Description, ProjectMap: make([]Card, len(in.ProjectMap)), Progress: make([]Card, len(in.Progress)), WorkerLanes: make([]WorkerLane, len(in.WorkerLanes)), Reviews: make([]EnrolledReview, len(in.Reviews)), JobRefs: make([]JobRef, len(in.JobRefs)), MapRefs: make([]MapRef, len(in.MapRefs)), WorkerRefs: make([]WorkerRef, len(in.WorkerRefs))}
	if out.Links, err = normalizeLinks(in.Links); err != nil {
		return Manifest{}, err
	}
	keys := make(map[string]bool)
	for _, group := range []struct{ in, out []Card }{{in.ProjectMap, out.ProjectMap}, {in.Progress, out.Progress}} {
		for i, card := range group.in {
			if !keyPattern.MatchString(card.Key) || keys[card.Key] || !publicText(card.Title, 240, false, true) || !publicText(card.Description, 8<<10, true, false) || !publicText(card.Phase, 160, false, false) || !publicText(card.Status, 240, false, false) {
				return Manifest{}, invalidModel()
			}
			keys[card.Key] = true
			if card.Links, err = normalizeLinks(card.Links); err != nil {
				return Manifest{}, err
			}
			group.out[i] = card
		}
	}
	for i, lane := range in.WorkerLanes {
		if !keyPattern.MatchString(lane.Key) || keys[lane.Key] || !publicText(lane.Label, 120, false, true) || !publicText(lane.Headline, 240, false, false) || !publicText(lane.Status, 240, false, false) || len(lane.Milestones) > MaxMilestones {
			return Manifest{}, invalidModel()
		}
		keys[lane.Key] = true
		milestones := make([]string, len(lane.Milestones))
		for j, text := range lane.Milestones {
			if !publicText(text, 240, false, true) {
				return Manifest{}, invalidModel()
			}
			milestones[j] = text
		}
		lane.Milestones = milestones
		out.WorkerLanes[i] = lane
	}
	for i, ref := range in.JobRefs {
		if !keyPattern.MatchString(ref.Key) || keys[ref.Key] || !jobIDPattern.MatchString(ref.JobID) || ref.SummaryRevision < 1 {
			return Manifest{}, invalidModel()
		}
		keys[ref.Key] = true
		out.JobRefs[i] = ref
	}
	for i, ref := range in.MapRefs {
		if !keyPattern.MatchString(ref.Key) || keys[ref.Key] || !mapEntryIDPattern.MatchString(ref.EntryID) || ref.Revision < 1 {
			return Manifest{}, invalidModel()
		}
		keys[ref.Key] = true
		out.MapRefs[i] = ref
	}
	for i, ref := range in.WorkerRefs {
		if !keyPattern.MatchString(ref.Key) || keys[ref.Key] || !workerIDPattern.MatchString(ref.WorkerID) || !publicText(ref.Label, 120, false, true) {
			return Manifest{}, invalidModel()
		}
		keys[ref.Key] = true
		out.WorkerRefs[i] = ref
	}
	if len(out.JobRefs) == 0 {
		out.JobRefs = nil
	}
	if len(out.MapRefs) == 0 {
		out.MapRefs = nil
	}
	if len(out.WorkerRefs) == 0 {
		out.WorkerRefs = nil
	}
	numbers := make(map[int64]bool, len(in.Reviews))
	for i, review := range in.Reviews {
		if review.Number < 1 || review.Number > 2147483647 || numbers[review.Number] || !publicText(review.Title, 240, false, true) || !approvedReviewURL(review.URL, review.Number) {
			return Manifest{}, invalidModel()
		}
		numbers[review.Number] = true
		out.Reviews[i] = review
	}
	if raw, err := json.Marshal(out); err != nil || len(raw) > MaxManifestBytes {
		return Manifest{}, invalidModel()
	}
	return out, nil
}

// DecodeManifest rejects unknown/duplicate/case-aliased fields, scalar null and
// malformed Unicode before normalization. Omitted optional collections become
// empty arrays; omitted presentation defaults use the shared selected theme.
func DecodeManifest(raw []byte) (Manifest, error) {
	var in Manifest
	if len(raw) > MaxManifestBytes || strictjson.Decode(raw, &in) != nil {
		return Manifest{}, invalidModel()
	}
	var values any
	if json.Unmarshal(raw, &values) != nil || !nonNull(values) {
		return Manifest{}, invalidModel()
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return Manifest{}, invalidModel()
	}
	presentation, err := presentationmodel.Decode(fields["presentation"])
	if err != nil {
		return Manifest{}, invalidModel()
	}
	in.Presentation = presentation
	return NormalizeManifest(in)
}

func nonNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case map[string]any:
		for _, field := range value {
			if !nonNull(field) {
				return false
			}
		}
	case []any:
		for _, item := range value {
			if !nonNull(item) {
				return false
			}
		}
	}
	return true
}

func publicRounds(in []RoundFacts, keys map[string]bool, limit int) ([]PublicRound, error) {
	out := make([]PublicRound, 0)
	for _, round := range in {
		if !round.Completed || !round.Finalized {
			continue
		}
		if len(out) >= limit || !roundKeyPattern.MatchString(round.Key) || keys[round.Key] || round.Found < 0 || round.Found > maxJSONInteger || round.Accepted < 0 || round.Routed < 0 || round.Accepted > round.Found || round.Routed > round.Found-round.Accepted {
			return nil, invalidModel()
		}
		keys[round.Key] = true
		observed := ""
		if round.ObservedAt != "" {
			at, err := time.Parse(time.RFC3339Nano, round.ObservedAt)
			at = at.UTC()
			if err != nil || at.IsZero() || at.Year() < 1 || at.Year() > 9999 {
				return nil, invalidModel()
			}
			observed = at.Format(time.RFC3339Nano)
		}
		var duration *int64
		if round.DurationMilliseconds != nil {
			if *round.DurationMilliseconds < 0 || *round.DurationMilliseconds > maxJSONInteger {
				return nil, invalidModel()
			}
			copy := *round.DurationMilliseconds
			duration = &copy
		}
		out = append(out, PublicRound{Key: round.Key, Found: round.Found, Accepted: round.Accepted, Routed: round.Routed, Complete: round.Complete, ObservedAt: observed, DurationMilliseconds: duration})
	}
	// Known round times are chronological. Unknown times remain visibly unknown;
	// opaque public keys give them deterministic order without inventing times.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ObservedAt != out[j].ObservedAt {
			if out[i].ObservedAt == "" {
				return false
			}
			if out[j].ObservedAt == "" {
				return true
			}
			a, _ := time.Parse(time.RFC3339Nano, out[i].ObservedAt)
			b, _ := time.Parse(time.RFC3339Nano, out[j].ObservedAt)
			return a.Before(b)
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

func normalizeContent(manifest Manifest, facts []ReviewFacts, roundLimit int) (Content, error) {
	approved, err := NormalizeManifest(manifest)
	if err != nil {
		return Content{}, err
	}
	out := Content{Slug: approved.Slug, Presentation: approved.Presentation, Description: approved.Description, Links: approved.Links, ProjectMap: approved.ProjectMap, Progress: approved.Progress, WorkerLanes: approved.WorkerLanes, Reviews: make([]PublicReview, len(approved.Reviews))}
	indexes := make(map[int64]int, len(approved.Reviews))
	for i, review := range approved.Reviews {
		indexes[review.Number] = i
		out.Reviews[i] = PublicReview{Number: review.Number, Title: review.Title, URL: review.URL, Lifecycle: "unknown", CLI: []PublicRound{}, Hosted: []PublicRound{}}
	}
	seen := make(map[int64]bool)
	keys := make(map[string]bool)
	for _, fact := range facts {
		i, enrolled := indexes[fact.Number]
		if !enrolled {
			continue
		}
		if seen[fact.Number] || fact.Lifecycle != "" && fact.Lifecycle != "unknown" && fact.Lifecycle != "open" && fact.Lifecycle != "closed" && fact.Lifecycle != "merged" {
			return Content{}, invalidModel()
		}
		seen[fact.Number] = true
		if fact.Lifecycle != "" {
			out.Reviews[i].Lifecycle = fact.Lifecycle
		}
		if out.Reviews[i].CLI, err = publicRounds(fact.CLI, keys, roundLimit); err != nil {
			return Content{}, err
		}
		if out.Reviews[i].Hosted, err = publicRounds(fact.Hosted, keys, roundLimit); err != nil {
			return Content{}, err
		}
		out.Reviews[i].CLIHasMore, out.Reviews[i].HostedHasMore = fact.CLIHasMore, fact.HostedHasMore
	}
	return out, nil
}

// BuildContent remains the strict retained-content boundary. Historical reads
// never trim a selected round or reinterpret earlier canonical content.
func BuildContent(manifest Manifest, facts []ReviewFacts) (Content, error) {
	out, err := normalizeContent(manifest, facts, MaxChannelRounds)
	if err != nil {
		return Content{}, err
	}
	if raw, err := json.Marshal(out); err != nil || len(raw) > MaxContentBytes {
		return Content{}, invalidModel()
	}
	return out, nil
}

// BuildWindowContent admits the bounded union of prior approved and current
// eligible summaries. It preserves every editorial selection while exposing an
// honest finite review window, without mutating either input.
func BuildWindowContent(manifest Manifest, facts []ReviewFacts) (Content, error) {
	enrolled := make(map[int64]bool, len(manifest.Reviews))
	for _, review := range manifest.Reviews {
		enrolled[review.Number] = true
	}
	for _, fact := range facts {
		if enrolled[fact.Number] && (len(fact.CLI) > 2*MaxChannelRounds || len(fact.Hosted) > 2*MaxChannelRounds) {
			return Content{}, invalidModel()
		}
	}
	out, err := normalizeContent(manifest, facts, 2*MaxChannelRounds)
	if err != nil {
		return Content{}, err
	}
	for i := range out.Reviews {
		review := &out.Reviews[i]
		if len(review.CLI) > MaxChannelRounds {
			review.CLI = review.CLI[len(review.CLI)-MaxChannelRounds:]
			review.CLIHasMore = true
		}
		if len(review.Hosted) > MaxChannelRounds {
			review.Hosted = review.Hosted[len(review.Hosted)-MaxChannelRounds:]
			review.HostedHasMore = true
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return Content{}, invalidModel()
	}
	size := len(raw)
	for size > MaxContentBytes {
		// Manifest order, then CLI before hosted, breaks equal-length ties.
		var largest *[]PublicRound
		var hasMore *bool
		flagBytes := 0
		for i := range out.Reviews {
			review := &out.Reviews[i]
			for _, channel := range []struct {
				rounds *[]PublicRound
				more   *bool
				added  int
			}{
				{&review.CLI, &review.CLIHasMore, len(`,"cli_has_more":true`)},
				{&review.Hosted, &review.HostedHasMore, len(`,"hosted_has_more":true`)},
			} {
				if len(*channel.rounds) > 0 && (largest == nil || len(*channel.rounds) > len(*largest)) {
					largest, hasMore, flagBytes = channel.rounds, channel.more, channel.added
				}
			}
		}
		if largest == nil {
			return Content{}, invalidModel()
		}
		removed, err := json.Marshal((*largest)[0])
		if err != nil {
			return Content{}, invalidModel()
		}
		size -= len(removed)
		*largest = (*largest)[1:]
		if len(*largest) > 0 {
			size--
		} // one array separator also disappears
		if !*hasMore {
			*hasMore = true
			size += flagBytes
		}
	}
	raw, err = json.Marshal(out)
	if err != nil || len(raw) != size || len(raw) > MaxContentBytes {
		return Content{}, invalidModel()
	}
	return out, nil
}

// DecodeContent validates canonical retained public content against the same
// allowlist as publication construction, independently of any private source.
func DecodeContent(raw []byte) (Content, error) {
	var content Content
	if len(raw) > MaxContentBytes || strictjson.Decode(raw, &content) != nil {
		return Content{}, invalidModel()
	}
	manifest := EditorialManifest(content)
	facts := make([]ReviewFacts, len(content.Reviews))
	for i, review := range content.Reviews {
		manifest.Reviews[i] = EnrolledReview{Number: review.Number, Title: review.Title, URL: review.URL}
		copyRounds := func(in []PublicRound) []RoundFacts {
			out := make([]RoundFacts, len(in))
			for j, round := range in {
				out[j] = RoundFacts{Key: round.Key, Completed: true, Finalized: true, Complete: round.Complete, Found: round.Found, Accepted: round.Accepted, Routed: round.Routed, ObservedAt: round.ObservedAt, DurationMilliseconds: round.DurationMilliseconds}
			}
			return out
		}
		facts[i] = ReviewFacts{Number: review.Number, Lifecycle: review.Lifecycle, CLI: copyRounds(review.CLI), Hosted: copyRounds(review.Hosted), CLIHasMore: review.CLIHasMore, HostedHasMore: review.HostedHasMore}
	}
	validated, err := BuildContent(manifest, facts)
	if err != nil {
		return Content{}, err
	}
	canonical, err := json.Marshal(validated)
	if err != nil || string(canonical) != string(raw) {
		return Content{}, invalidModel()
	}
	return validated, nil
}
