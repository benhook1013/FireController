package reviewprovider

import (
	"strings"
	"time"
)

// ResolveHostedAllowanceWithBaseline keeps strict attribution first. Only an
// independently retained, pre-edit body can prove that an older unrelated Bot
// report's refresh did not introduce or remove a provider command. The baseline
// may have been collected after the query: its server-owned UpdatedAt must still
// prove those exact old bytes predated the query. The baseline must itself
// prove the same query was open and pending. Neither input is changed.
func ResolveHostedAllowanceWithBaseline(snapshot, baseline HostedAllowanceSnapshot, intent HostedTriggerIntent, postingAuthor, providerAuthor Author, earliestPosting time.Time) (HostedAllowanceObservation, error) {
	strict, err := ResolveHostedAllowance(snapshot, intent, postingAuthor, providerAuthor, earliestPosting)
	if err != nil || strict.Reason != "query_window_closed" {
		return strict, err
	}
	old, err := ResolveHostedAllowance(baseline, intent, postingAuthor, providerAuthor, earliestPosting)
	if err != nil || old.QueryCommentID != strict.QueryCommentID || old.QueryCreatedAt != strict.QueryCreatedAt || old.State != "unproven" || old.Reason != "allowance_not_proven" || !baseline.Complete || !baseline.ObservedAt.Before(snapshot.ObservedAt) {
		return strict, nil
	}
	previous := make(map[int64]IssueComment, len(baseline.IssueComments))
	for _, comment := range baseline.IssueComments {
		previous[comment.ID] = comment
	}
	projected := snapshot
	projected.IssueComments = append([]IssueComment(nil), snapshot.IssueComments...)
	changed := false
	for index, current := range projected.IssueComments {
		old, found := previous[current.ID]
		if !found || current.Author.Kind != "Bot" || current.Author != old.Author || publicationActor(current.Author, postingAuthor) || publicationActor(current.Author, providerAuthor) ||
			!current.CreatedAt.Equal(old.CreatedAt) || !current.CreatedAt.Before(strict.QueryCreatedAt) || !old.UpdatedAt.Before(strict.QueryCreatedAt) || old.UpdatedAt.After(baseline.ObservedAt) ||
			!baseline.ObservedAt.Before(current.UpdatedAt) || !current.UpdatedAt.After(strict.QueryCreatedAt) || !current.UpdatedAt.After(old.UpdatedAt) ||
			strings.Contains(strings.ToLower(old.Body), "coderabbit") || strings.Contains(strings.ToLower(current.Body), "coderabbit") {
			continue
		}
		// Preserve both original bodies, identities and creation instants. Only this
		// proven harmless edit timestamp is excluded from causal-window closure.
		projected.IssueComments[index].UpdatedAt = old.UpdatedAt
		changed = true
	}
	if !changed {
		return strict, nil
	}
	return ResolveHostedAllowance(projected, intent, postingAuthor, providerAuthor, earliestPosting)
}
