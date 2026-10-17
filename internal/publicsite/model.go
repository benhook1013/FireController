// Package publicsite serves explicitly approved anonymous project projections.
package publicsite

import "github.com/benhook1013/FireController/internal/publicsitemodel"

const (
	MaxManifestBytes = publicsitemodel.MaxManifestBytes
	MaxContentBytes  = publicsitemodel.MaxContentBytes
	MaxReviews       = publicsitemodel.MaxReviews
	MaxCards         = publicsitemodel.MaxCards
	MaxWorkerLanes   = publicsitemodel.MaxWorkerLanes
	MaxLinks         = publicsitemodel.MaxLinks
	MaxMilestones    = publicsitemodel.MaxMilestones
	MaxChannelRounds = publicsitemodel.MaxChannelRounds
)

type Link = publicsitemodel.Link
type Card = publicsitemodel.Card
type WorkerLane = publicsitemodel.WorkerLane
type EnrolledReview = publicsitemodel.EnrolledReview
type Manifest = publicsitemodel.Manifest
type RoundFacts = publicsitemodel.RoundFacts
type ReviewFacts = publicsitemodel.ReviewFacts
type PublicRound = publicsitemodel.PublicRound
type PublicReview = publicsitemodel.PublicReview
type Content = publicsitemodel.Content

func NormalizeManifest(in Manifest) (Manifest, error) { return publicsitemodel.NormalizeManifest(in) }
func DecodeManifest(raw []byte) (Manifest, error)     { return publicsitemodel.DecodeManifest(raw) }
func BuildContent(manifest Manifest, facts []ReviewFacts) (Content, error) {
	return publicsitemodel.BuildContent(manifest, facts)
}

// BuildWindowContent constructs a bounded current projection. Historical
// decoders deliberately keep using strict BuildContent without rewindowing.
func BuildWindowContent(manifest Manifest, facts []ReviewFacts) (Content, error) {
	return publicsitemodel.BuildWindowContent(manifest, facts)
}
func ValidSlug(slug string) bool { return publicsitemodel.ValidSlug(slug) }
