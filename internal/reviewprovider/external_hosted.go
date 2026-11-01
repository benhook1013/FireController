package reviewprovider

import (
	"net/http"
	"sort"
	"time"
)

// ExternalHostedRun is a provider-owned archive source, not a controller
// execution, command completion, allowance observation or coverage grant.
// Native marker metadata is only a hint; consumers must verify ledger ownership.
type ExternalHostedRun struct {
	SourceID          string
	HeadSHA           string
	FinishedAt        time.Time
	ReviewIDs         []int64
	ProviderRunID     string
	Inventory         HostedFindingInventory
	TriggerCommentID  int64
	NativeOperationID string
}

type externalHostedGroup struct {
	run      ExternalHostedRun
	batches  map[int]HostedReview
	total    int
	count    int
	selected map[string]bool
	reason   string
	standing bool
	first    time.Time
}

// DiscoverExternalHosted recognizes actual provider identities independently of
// native trigger ownership. Complete means supported source inventory only.
// There are no provider calls, synthetic attempts, acknowledgements or retries.
func DiscoverExternalHosted(snapshot HostedPublicationSnapshot, provider Author) ([]ExternalHostedRun, error) {
	empty := []ExternalHostedRun{}
	if !publicationValidActor(provider) || provider.Kind != "Bot" {
		return empty, invalidSelector()
	}
	if !snapshot.Complete || snapshot.IssueComments == nil || snapshot.Reviews == nil || snapshot.ReviewComments == nil || !hostedTriggerTime(snapshot.ObservedAt) || !validPublicationSnapshot(snapshot) {
		return empty, invalidResponse(false, http.StatusOK)
	}
	groups := map[string]*externalHostedGroup{}
	sourceHeads := map[string]string{}
	group := func(source, head string) *externalHostedGroup {
		if old, ok := sourceHeads[source]; ok && old != head {
			return nil
		}
		sourceHeads[source] = head
		key := source + ":" + head
		if g := groups[key]; g != nil {
			return g
		}
		g := &externalHostedGroup{run: ExternalHostedRun{SourceID: source, HeadSHA: head, ReviewIDs: []int64{}, Inventory: HostedFindingInventory{Findings: []HostedFinding{}, Publication: HostedPublication{State: "attributed", Batches: []HostedReview{}, Roots: []ReviewComment{}, EligibleReviews: []HostedReview{}}}}, batches: map[int]HostedReview{}}
		groups[key] = g
		return g
	}
	for _, review := range snapshot.Reviews {
		if !publicationActor(review.Author, provider) || review.SubmittedAt.IsZero() || review.State != "commented" && review.State != "approved" && review.State != "changes_requested" {
			continue
		}
		marker, found, bad := publicationBatchMarker(review.Body)
		if bad {
			continue
		}
		var g *externalHostedGroup
		if found {
			g = group("publication:"+marker.publication, review.HeadSHA)
			if g == nil {
				return empty, invalidResponse(false, http.StatusOK)
			}
			pub := &g.run.Inventory.Publication
			if g.total != 0 && (g.total != marker.total || pub.ProviderAttemptID != marker.attempt) {
				g.reason = "conflicting_publication_evidence"
			}
			if _, duplicate := g.batches[marker.ordinal]; duplicate {
				g.reason = "conflicting_publication_evidence"
			}
			g.total, pub.PublicationID, pub.ProviderAttemptID = marker.total, marker.publication, marker.attempt
			g.batches[marker.ordinal] = review
		} else {
			run, count, selected, valid := parseSubmittedReview(review.Body, review.HeadSHA)
			if run == "" {
				continue
			}
			g = group("run:"+run, review.HeadSHA)
			if g == nil {
				return empty, invalidResponse(false, http.StatusOK)
			}
			if len(g.run.Inventory.Publication.EligibleReviews) > 0 || g.standing {
				g.reason = "conflicting_submitted_review_evidence"
			}
			g.run.ProviderRunID, g.count, g.selected = run, count, selected
			if !valid {
				g.reason = "submitted_review_inventory_unproven"
			}
		}
		g.run.Inventory.Publication.EligibleReviews = append(g.run.Inventory.Publication.EligibleReviews, review)
		if g.first.IsZero() || review.SubmittedAt.Before(g.first) {
			g.first = review.SubmittedAt
		}
		if review.SubmittedAt.After(g.run.FinishedAt) {
			g.run.FinishedAt = review.SubmittedAt
		}
	}
	for _, comment := range snapshot.IssueComments {
		if !publicationActor(comment.Author, provider) {
			continue
		}
		zero, valid := parseStandingZero(comment.Body)
		if !valid {
			continue
		}
		g := group("run:"+zero.ProviderRunID, zero.HeadSHA)
		if g == nil {
			return empty, invalidResponse(false, http.StatusOK)
		}
		if g.standing || len(g.run.Inventory.Publication.EligibleReviews) > 0 {
			g.reason = "conflicting_standing_zero_evidence"
		}
		g.standing, g.run.ProviderRunID = true, zero.ProviderRunID
		g.first, g.run.FinishedAt = comment.UpdatedAt, comment.UpdatedAt
	}
	roots := map[int64][]ReviewComment{}
	for _, root := range snapshot.ReviewComments {
		if root.InReplyToID != 0 {
			continue
		}
		copy := root
		copy.Line, copy.StartLine = copyPublicationLine(root.Line), copyPublicationLine(root.StartLine)
		copy.OriginalLine, copy.OriginalStartLine = copyPublicationLine(root.OriginalLine), copyPublicationLine(root.OriginalStartLine)
		roots[root.ReviewID] = append(roots[root.ReviewID], copy)
	}
	windows := externalHostedNativeWindows(snapshot.IssueComments, provider)
	result := make([]ExternalHostedRun, 0, len(groups))
	for _, g := range groups {
		pub := &g.run.Inventory.Publication
		pub.FinishedAt = g.run.FinishedAt
		if g.standing {
			// A supported mutable source observation is not an attributable native
			// finished command. No StandingZero/ACK proof or native coverage is forged.
			pub.Reason = "external_standing_zero_observed"
			g.run.Inventory.Complete, g.run.Inventory.Reason = g.reason == "", pub.Reason
		} else {
			if g.total > 0 {
				for ordinal := 1; ordinal <= g.total; ordinal++ {
					if batch, ok := g.batches[ordinal]; ok {
						pub.Batches = append(pub.Batches, batch)
					}
				}
				if len(g.batches) != g.total && g.reason == "" {
					g.reason = "missing_publication_batches"
				}
			} else {
				pub.Batches = append(pub.Batches, pub.EligibleReviews...)
			}
			sort.Slice(pub.EligibleReviews, func(i, j int) bool { return pub.EligibleReviews[i].ID < pub.EligibleReviews[j].ID })
			for _, review := range pub.EligibleReviews {
				g.run.ReviewIDs = append(g.run.ReviewIDs, review.ID)
				for _, root := range roots[review.ID] {
					pub.Roots = append(pub.Roots, root)
					if !publicationActor(root.Author, provider) || root.OriginalHeadSHA != g.run.HeadSHA || root.CreatedAt.After(g.run.FinishedAt) || g.total == 0 && (root.UpdatedAt.After(g.run.FinishedAt) || !g.selected[root.Path]) {
						g.reason = "root_binding_mismatch"
					}
				}
			}
			sort.Slice(pub.Roots, func(i, j int) bool { return pub.Roots[i].ID < pub.Roots[j].ID })
			if g.total > 0 && len(pub.Batches) > 0 && g.reason == "" {
				count, valid := publicationRootCount(g.batches[1].Body)
				if !valid {
					if len(pub.Roots) == 0 && publicationOutsideOnly(*pub) {
						count, valid = 0, true
					}
				}
				if !valid {
					g.reason = "invalid_actionable_count"
				} else {
					g.count = count
				}
			}
			if len(pub.Roots) != g.count && g.reason == "" {
				g.reason = "root_count_mismatch"
			}
			if g.reason == "" {
				g.run.Inventory = decodeHostedFindings(*pub)
			}
		}
		if g.reason != "" {
			g.run.Inventory.Complete = false
			g.run.Inventory.Reason = g.reason
			g.run.Inventory.Publication.Reason = g.reason
		}
		g.run.TriggerCommentID, g.run.NativeOperationID = windows.match(g.run.HeadSHA, g.first, g.run.FinishedAt)
		result = append(result, g.run)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].FinishedAt.Equal(result[j].FinishedAt) {
			return result[i].SourceID < result[j].SourceID
		}
		return result[i].FinishedAt.Before(result[j].FinishedAt)
	})
	return result, nil
}
