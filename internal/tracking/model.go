package tracking

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/strictjson"
	"github.com/benhook1013/FireController/internal/trackingmodel"
)

// The wire types live in trackingmodel so clients avoid storage dependencies.
type (
	Milestone         = trackingmodel.Milestone
	MilestoneFields   = trackingmodel.MilestoneFields
	Link              = trackingmodel.Link
	LinkTarget        = trackingmodel.LinkTarget
	Item              = trackingmodel.Item
	ItemFields        = trackingmodel.ItemFields
	Counts            = trackingmodel.Counts
	ItemSummary       = trackingmodel.ItemSummary
	ItemView          = trackingmodel.ItemView
	MilestoneSummary  = trackingmodel.MilestoneSummary
	MilestoneView     = trackingmodel.MilestoneView
	HistorySummary    = trackingmodel.HistorySummary
	ItemRevision      = trackingmodel.ItemRevision
	MilestoneRevision = trackingmodel.MilestoneRevision
	Receipt           = trackingmodel.Receipt
	BatchReceipt      = trackingmodel.BatchReceipt
)

// Page is one keyset page.
type Page[T any] = trackingmodel.Page[T]

var (
	Statuses     = trackingmodel.Statuses
	OpenStatuses = trackingmodel.OpenStatuses
	LinkTypes    = trackingmodel.LinkTypes
	LinkRoles    = trackingmodel.LinkRoles
)

const (
	maxPosition          = 1000000
	maxTitleBytes        = 240
	maxItemDescription   = 2 << 10
	maxMilestoneDescript = 8 << 10
	maxRevisitBytes      = 1 << 10
	maxLabels            = 16
	maxLinks             = 64
	maxLinkLabelBytes    = 120
	maxRecordBytes       = 32 << 10
	maxBatchItems        = 100
	maxBatchMilestones   = 20
	maxParentDepth       = 32
)

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
var labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+-]{0,63}$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
var refIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

func invalid(message string) error {
	if message == "" {
		message = "Use a complete bounded tracking value with valid fields."
	}
	return core.Fail("validation_failed", message, 400)
}
func unavailable() error {
	return core.Fail("storage_unavailable", "Stored tracking state is unavailable.", 503)
}
func notFound() error {
	return core.Fail("not_found", "The selected record is unavailable in this project.", 404)
}
func conflict(rev int64) error {
	return &core.Error{Code: "revision_conflict", Message: "The record changed; reload it or omit expected_revision for a last-write-wins change.", Status: 409, Details: map[string]int64{"current_revision": rev}}
}

func validKey(v string) bool    { return keyPattern.MatchString(v) }
func validStatus(v string) bool { return slices.Contains(Statuses, v) }
func validProjectID(v string) bool {
	return v != "" && len(v) <= 128 && utf8.ValidString(v) && strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) || r == '/' }) < 0
}
func text(v string, n int) bool {
	return len(v) <= n && utf8.ValidString(v) && strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) < 0
}
func short(v string, n int) bool { return text(v, n) && strings.IndexFunc(v, unicode.IsControl) < 0 }
func named(v string, n int) bool { return short(v, n) && v != "" && strings.TrimSpace(v) == v }
func stamp() string              { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }
func validStamp(v string) bool {
	t, err := time.Parse(time.RFC3339Nano, v)
	return err == nil && !t.IsZero() && strings.HasSuffix(v, "Z")
}
func validDate(v string) bool {
	if v == "" {
		return true
	}
	t, err := time.Parse("2006-01-02", v)
	return err == nil && t.Format("2006-01-02") == v
}

func normalizeMilestone(m MilestoneFields) (MilestoneFields, error) {
	m.Title = strings.TrimSpace(m.Title)
	if m.State == "" {
		m.State = "open"
	}
	if !named(m.Title, maxTitleBytes) || !text(m.Description, maxMilestoneDescript) || (m.State != "open" && m.State != "closed") || !validDate(m.TargetDate) || m.Position < 0 || m.Position > maxPosition {
		return MilestoneFields{}, invalid("Use a nonempty title (240 bytes), description up to 8 KiB, state open or closed, target_date YYYY-MM-DD and position 0..1000000.")
	}
	return m, nil
}

// normalizeItem trims and canonicalizes fields that need no store lookups.
// Link targets, parent and milestone existence are checked in the transaction.
func normalizeItem(f ItemFields) (ItemFields, error) {
	f.Title = strings.TrimSpace(f.Title)
	if f.Status == "" {
		f.Status = "planned"
	}
	if !named(f.Title, maxTitleBytes) || !text(f.Description, maxItemDescription) || !validStatus(f.Status) || !text(f.RevisitWhen, maxRevisitBytes) || f.Position < 0 || f.Position > maxPosition {
		return ItemFields{}, invalid("Use a nonempty title (240 bytes), description up to 2 KiB, status planned|active|blocked|deferred|done|dropped, revisit_when up to 1 KiB and position 0..1000000.")
	}
	if f.Milestone != "" && !validKey(f.Milestone) || f.Parent != "" && !validKey(f.Parent) {
		return ItemFields{}, invalid("Milestone and parent must be existing keys in this project, or empty.")
	}
	labels, err := normalizeLabels(f.Labels)
	if err != nil {
		return ItemFields{}, err
	}
	f.Labels = labels
	if f.Links == nil {
		f.Links = []Link{}
	}
	if len(f.Links) > maxLinks {
		return ItemFields{}, invalid("An item holds at most 64 links.")
	}
	return f, nil
}

func normalizeLabels(labels []string) ([]string, error) {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if !labelPattern.MatchString(l) {
			return nil, invalid("Labels are 1-64 characters of letters, digits and ._:/+- starting with a letter or digit.")
		}
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	if len(out) > maxLabels {
		return nil, invalid("An item holds at most 16 labels.")
	}
	slices.Sort(out)
	return out, nil
}

// normalizeLinkShape checks the type, role and ref syntax. repository is the
// project's default "owner/name" used to expand a bare PR number.
func normalizeLinkShape(l Link, repository string) (Link, error) {
	l.Label = strings.TrimSpace(l.Label)
	if !slices.Contains(LinkTypes, l.Type) {
		return Link{}, invalid("Link type is one of job, pr, route, note, item or url.")
	}
	if !slices.Contains(LinkRoles, l.Role) {
		return Link{}, invalid("Link role is one of delivers, proof, blocks or related.")
	}
	if !short(l.Label, maxLinkLabelBytes) {
		return Link{}, invalid("Link labels are at most 120 bytes of single-line text.")
	}
	ref, err := normalizeRef(l.Type, l.Ref, repository)
	if err != nil {
		return Link{}, err
	}
	l.Ref = ref
	return l, nil
}

// normalizeRef returns the canonical stored reference for one link target.
func normalizeRef(kind, ref, repository string) (string, error) {
	switch kind {
	case "pr":
		repo, number := repository, ref
		if i := strings.LastIndex(ref, "#"); i >= 0 {
			if i > 0 {
				repo = ref[:i]
			}
			number = ref[i+1:]
		}
		n, err := strconv.ParseInt(number, 10, 64)
		if err != nil || n < 1 || n > 1<<31 || strconv.FormatInt(n, 10) != number || !repoPattern.MatchString(repo) {
			return "", invalid("A pr link ref is NUMBER, #NUMBER or owner/name#NUMBER.")
		}
		return repo + "#" + number, nil
	case "item":
		if !validKey(ref) {
			return "", invalid("An item link ref is an item key.")
		}
		return ref, nil
	case "url":
		u, err := url.Parse(ref)
		if len(ref) > 2048 || !utf8.ValidString(ref) || strings.IndexFunc(ref, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 || err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
			return "", invalid("A url link ref is an absolute http or https URL with a host and no credentials.")
		}
		return ref, nil
	default:
		if !refIDPattern.MatchString(ref) {
			return "", invalid("A " + kind + " link ref is that record's ID.")
		}
		return ref, nil
	}
}

func sortLinks(links []Link) {
	slices.SortFunc(links, func(a, b Link) int {
		if c := strings.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		if c := strings.Compare(a.Ref, b.Ref); c != 0 {
			return c
		}
		return strings.Compare(a.Role, b.Role)
	})
}

func itemFields(i Item) ItemFields {
	return ItemFields{Title: i.Title, Description: i.Description, Status: i.Status, Milestone: i.Milestone, Parent: i.Parent, Labels: slices.Clone(i.Labels), RevisitWhen: i.RevisitWhen, Position: i.Position, Links: slices.Clone(i.Links)}
}
func milestoneFields(m Milestone) MilestoneFields {
	return MilestoneFields{Title: m.Title, Description: m.Description, State: m.State, TargetDate: m.TargetDate, Position: m.Position}
}
func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// validItem checks a stored item record without store lookups.
func validItem(i Item) bool {
	if !validKey(i.Key) || !validProjectID(i.ProjectID) || i.Revision < 1 || !validStamp(i.CreatedAt) || !validStamp(i.UpdatedAt) || i.Labels == nil || i.Links == nil {
		return false
	}
	f, err := normalizeItem(itemFields(i))
	if err != nil || !sameJSON(f, itemFields(i)) {
		return false
	}
	seen := map[[3]string]bool{}
	for _, l := range i.Links {
		n, err := normalizeLinkShape(l, "")
		if err != nil || n != l || seen[[3]string{l.Type, l.Ref, l.Role}] || l.Type == "item" && l.Ref == i.Key {
			return false
		}
		seen[[3]string{l.Type, l.Ref, l.Role}] = true
	}
	sorted := slices.Clone(i.Links)
	sortLinks(sorted)
	return slices.Equal(sorted, i.Links) && i.Parent != i.Key
}
func validMilestone(m Milestone) bool {
	if !validKey(m.Key) || !validProjectID(m.ProjectID) || m.Revision < 1 || !validStamp(m.CreatedAt) || !validStamp(m.UpdatedAt) {
		return false
	}
	f, err := normalizeMilestone(milestoneFields(m))
	return err == nil && f == milestoneFields(m)
}

func decodeItem(raw string) (Item, error) {
	var i Item
	if len(raw) > maxRecordBytes || strictjson.Decode([]byte(raw), &i) != nil || !validItem(i) {
		return Item{}, unavailable()
	}
	if canonical, err := json.Marshal(i); err != nil || string(canonical) != raw {
		return Item{}, unavailable()
	}
	return i, nil
}
func decodeMilestone(raw string) (Milestone, error) {
	var m Milestone
	if len(raw) > maxRecordBytes || strictjson.Decode([]byte(raw), &m) != nil || !validMilestone(m) {
		return Milestone{}, unavailable()
	}
	if canonical, err := json.Marshal(m); err != nil || string(canonical) != raw {
		return Milestone{}, unavailable()
	}
	return m, nil
}

// rejectNulls refuses explicit JSON null anywhere in a request so omission
// stays the only way to leave a field unchanged.
func rejectNulls(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return invalid("Use one JSON object.")
		}
		if token == nil {
			return invalid("Omit a field instead of sending null.")
		}
	}
}
