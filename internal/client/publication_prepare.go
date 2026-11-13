package client

import (
	"context"
	"encoding/json"
	"math"
	"regexp"

	"github.com/benhook1013/FireController/internal/publicsitemodel"
	"github.com/benhook1013/FireController/internal/strictjson"
)

// A preparation plan is intentionally much smaller than a complete editorial
// document. It selects explicit public revisions, never latest private prose.
const MaxPublicationChangesBytes = 8 << 10

type PublicationSelectionChanges[T any, K comparable] struct {
	Upsert []T `json:"upsert,omitempty"`
	Remove []K `json:"remove,omitempty"`
}

type PublicationChanges struct {
	ExpectedRevision int64                                                               `json:"expected_revision"`
	Description      *string                                                             `json:"description,omitempty"`
	JobRefs          *PublicationSelectionChanges[publicsitemodel.JobRef, string]        `json:"job_refs,omitempty"`
	MapRefs          *PublicationSelectionChanges[publicsitemodel.MapRef, string]        `json:"map_refs,omitempty"`
	WorkerRefs       *PublicationSelectionChanges[publicsitemodel.WorkerRef, string]     `json:"worker_refs,omitempty"`
	Reviews          *PublicationSelectionChanges[publicsitemodel.EnrolledReview, int64] `json:"reviews,omitempty"`
}

var publicationSelectionKey = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func publicationChangesInvalid() error {
	return fail("invalid_publication_changes", "Select bounded explicit publication edits and a current configuration revision.", "Use pinned job-summary/map revisions, explicit public labels and review titles/URLs; omit unchanged content.", 400)
}

func DecodePublicationChanges(raw []byte) (PublicationChanges, error) {
	var in PublicationChanges
	var fields map[string]json.RawMessage
	var values any
	if len(raw) > MaxPublicationChangesBytes || strictjson.Decode(raw, &in) != nil || json.Unmarshal(raw, &fields) != nil || fields["expected_revision"] == nil || json.Unmarshal(raw, &values) != nil || !publicationChangesNonNull(values) || validatePublicationChanges(in) != nil {
		return PublicationChanges{}, publicationChangesInvalid()
	}
	return in, nil
}

func publicationChangesNonNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case map[string]any:
		for _, field := range value {
			if !publicationChangesNonNull(field) {
				return false
			}
		}
	case []any:
		for _, field := range value {
			if !publicationChangesNonNull(field) {
				return false
			}
		}
	}
	return true
}

func validatePublicationChanges(in PublicationChanges) error {
	if in.ExpectedRevision < 1 || in.ExpectedRevision == math.MaxInt64 {
		return publicationChangesInvalid()
	}
	raw, err := json.Marshal(in)
	if err != nil || len(raw) > MaxPublicationChangesBytes {
		return publicationChangesInvalid()
	}
	selected := PublicationManifest{Slug: "preparation"}
	selected.Presentation.Name = "Preparation validation"
	changed := in.Description != nil
	if in.Description != nil {
		selected.Description = *in.Description
	}
	validKey := func(key string) bool { return publicationSelectionKey.MatchString(key) }
	if in.JobRefs != nil {
		if !validPublicationSelectionChanges(*in.JobRefs, publicsitemodel.MaxCards, func(ref publicsitemodel.JobRef) string { return ref.Key }, validKey) {
			return publicationChangesInvalid()
		}
		selected.JobRefs = in.JobRefs.Upsert
		changed = true
	}
	if in.MapRefs != nil {
		if !validPublicationSelectionChanges(*in.MapRefs, publicsitemodel.MaxCards, func(ref publicsitemodel.MapRef) string { return ref.Key }, validKey) {
			return publicationChangesInvalid()
		}
		selected.MapRefs = in.MapRefs.Upsert
		changed = true
	}
	if in.WorkerRefs != nil {
		if !validPublicationSelectionChanges(*in.WorkerRefs, publicsitemodel.MaxWorkerLanes, func(ref publicsitemodel.WorkerRef) string { return ref.Key }, validKey) {
			return publicationChangesInvalid()
		}
		selected.WorkerRefs = in.WorkerRefs.Upsert
		changed = true
	}
	if in.Reviews != nil {
		if !validPublicationSelectionChanges(*in.Reviews, publicsitemodel.MaxReviews, func(ref publicsitemodel.EnrolledReview) int64 { return ref.Number }, func(n int64) bool { return n >= 1 && n <= 2147483647 }) {
			return publicationChangesInvalid()
		}
		selected.Reviews = in.Reviews.Upsert
		changed = true
	}
	if !changed {
		return publicationChangesInvalid()
	}
	if _, err = publicsitemodel.NormalizeManifest(selected); err != nil {
		return publicationChangesInvalid()
	}
	return nil
}

func validPublicationSelectionChanges[T any, K comparable](edits PublicationSelectionChanges[T, K], limit int, key func(T) K, valid func(K) bool) bool {
	if len(edits.Upsert)+len(edits.Remove) == 0 || len(edits.Upsert) > limit || len(edits.Remove) > limit {
		return false
	}
	seen := make(map[K]bool)
	for _, value := range edits.Upsert {
		id := key(value)
		if !valid(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	for _, id := range edits.Remove {
		if !valid(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func editPublicationSelection[T any, K comparable](existing []T, edits *PublicationSelectionChanges[T, K], key func(T) K) ([]T, error) {
	if edits == nil {
		return existing, nil
	}
	present := make(map[K]bool, len(existing))
	for _, value := range existing {
		present[key(value)] = true
	}
	remove := make(map[K]bool, len(edits.Remove))
	for _, id := range edits.Remove {
		if !present[id] {
			return nil, publicationChangesInvalid()
		}
		remove[id] = true
	}
	upsert := make(map[K]T, len(edits.Upsert))
	for _, value := range edits.Upsert {
		upsert[key(value)] = value
	}
	out := make([]T, 0, len(existing)+len(edits.Upsert))
	for _, value := range existing {
		id := key(value)
		if remove[id] {
			continue
		}
		if replacement, ok := upsert[id]; ok {
			value = replacement
		}
		out = append(out, value)
	}
	for _, value := range edits.Upsert {
		if !present[key(value)] {
			out = append(out, value)
		}
	}
	return out, nil
}

// PreparePublication reads configuration once, merges only the selected fields,
// and uses the normal guarded preview. It never applies publication or fetches
// private source records. The returned exact input is the apply/recovery file.
func (c *Client) PreparePublication(ctx context.Context, edits PublicationChanges) (PublicationInput, string, error) {
	if err := validatePublicationChanges(edits); err != nil {
		return PublicationInput{}, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	configuration, err := c.Publication(ctx)
	if err != nil {
		return PublicationInput{}, "", err
	}
	if configuration.Manifest == nil {
		return PublicationInput{}, "", fail("publication_not_configured", "Configure publication before preparing selected edits.", "Use the complete manifest preview/apply flow for initial setup.", 409)
	}
	if configuration.Revision != edits.ExpectedRevision {
		return PublicationInput{}, "", &Error{Code: "publication_revision_conflict", Message: "The publication configuration changed.", Action: "Read its current revision and prepare the intended edits again; retain any already-applied request for exact recovery.", Status: 409, Details: map[string]any{"current_revision": configuration.Revision}}
	}
	manifest := *configuration.Manifest
	if edits.Description != nil {
		manifest.Description = *edits.Description
	}
	manifest.JobRefs, err = editPublicationSelection(manifest.JobRefs, edits.JobRefs, func(ref publicsitemodel.JobRef) string { return ref.Key })
	if err == nil {
		manifest.MapRefs, err = editPublicationSelection(manifest.MapRefs, edits.MapRefs, func(ref publicsitemodel.MapRef) string { return ref.Key })
	}
	if err == nil {
		manifest.WorkerRefs, err = editPublicationSelection(manifest.WorkerRefs, edits.WorkerRefs, func(ref publicsitemodel.WorkerRef) string { return ref.Key })
	}
	if err == nil {
		manifest.Reviews, err = editPublicationSelection(manifest.Reviews, edits.Reviews, func(ref publicsitemodel.EnrolledReview) int64 { return ref.Number })
	}
	if err != nil {
		return PublicationInput{}, "", err
	}
	input, err := normalizePublicationInput(PublicationInput{ExpectedRevision: configuration.Revision, Enabled: configuration.Enabled, Manifest: manifest})
	if err != nil {
		return PublicationInput{}, "", publicationChangesInvalid()
	}
	preview, err := c.PreviewPublication(ctx, input)
	if err != nil {
		return PublicationInput{}, "", err
	}
	return input, preview.SHA256, nil
}
