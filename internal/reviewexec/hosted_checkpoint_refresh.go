package reviewexec

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

var checkpointRefreshID = regexp.MustCompile(`<!-- firecontroller-checkpoint:v1 publication=(review_publication_[a-f0-9]{32}) semantic=[a-f0-9]{64} -->`)

// ProjectHostedCheckpointFileEdits qualifies one narrow editorial change to an
// older published controller checkpoint. It changes attribution timestamps only:
// callers must already own the immutable same-attempt pre-POST baseline, and
// raw captures, bodies, checkpoint intents and provider facts remain untouched.
func (e *Engine) ProjectHostedCheckpointFileEdits(ctx context.Context, project, attemptID string, baseline, current reviewprovider.HostedPublicationSnapshot) (reviewprovider.HostedPublicationSnapshot, error) {
	projected := current
	copied := false
	if !baseline.Complete || !current.Complete {
		return current, nil
	}
	markerPresent := false
	for _, comment := range current.IssueComments {
		markerPresent = markerPresent || strings.Contains(comment.Body, "<!-- firecontroller-checkpoint:v1 ")
	}
	if !markerPresent {
		return current, nil
	}
	err := e.readAdmission(ctx, func(ctx context.Context, tx *sql.Tx) error {
		a, o, err := readHostedPostingAttempt(ctx, tx, project, attemptID)
		if err != nil {
			return err
		}
		at, err := readHostedAdmissionTime(ctx, tx, a, o)
		if err != nil {
			return err
		}
		if !baseline.ObservedAt.Before(at) || !baseline.ObservedAt.Before(current.ObservedAt) {
			return nil
		}
		query, err := readHostedQuery(ctx, tx, a, o)
		if err != nil {
			return err
		}
		intent := reviewprovider.HostedTriggerIntent{OperationID: o.ID, AttemptID: a.ID, HeadSHA: a.Anchor.CandidateSHA}
		trigger, err := reviewprovider.RecoverHostedTrigger(current.IssueComments, current.Complete, intent, query.PostingActor, at.Truncate(time.Second), current.ObservedAt)
		if err != nil || trigger.State != "adopted" || a.ExternalID != strconv.FormatInt(trigger.CommentID, 10) {
			return err
		}
		old := make(map[int64]reviewprovider.IssueComment, len(baseline.IssueComments))
		for _, c := range baseline.IssueComments {
			old[c.ID] = c
		}
		candidates := 0
		for index, c := range current.IssueComments {
			prior, exists := old[c.ID]
			if !exists || c.ID == trigger.CommentID || !reflect.DeepEqual(c.Author, prior.Author) || c.Author.ID != query.PostingActor.ID || c.Author.Kind != query.PostingActor.Kind || !c.CreatedAt.Equal(prior.CreatedAt) || !c.CreatedAt.Before(trigger.CreatedAt) || prior.UpdatedAt.After(baseline.ObservedAt) || !c.UpdatedAt.After(trigger.CreatedAt) || !c.UpdatedAt.After(prior.UpdatedAt) {
				continue
			}
			ids := checkpointRefreshID.FindAllStringSubmatch(c.Body, -1)
			if len(ids) != 1 {
				continue
			}
			candidates++
			if candidates > 32 {
				return core.Fail("overloaded", "Checkpoint annotation attribution is unavailable.", 503)
			}
			r, err := readCheckpoint(ctx, tx, project, ids[0][1])
			var failure *core.Error
			if errors.As(err, &failure) && failure.Code == "not_found" {
				continue
			}
			if err != nil {
				return err
			}
			if r.View.State != "published" || r.View.CommentID != c.ID || r.Intent == nil || r.Payload.Channel != "hosted" || r.Payload.Files != nil || r.Payload.RepositoryID != a.Anchor.RepositoryID || r.Payload.PullNumber != a.Anchor.PullNumber || r.Intent.Actor.ID != c.Author.ID || r.Intent.Actor.Kind != c.Author.Kind {
				continue
			}
			publishedAt, valid := checkpointTime(r.View.PublishedAt)
			if !valid || !publishedAt.Before(trigger.CreatedAt) {
				continue
			}
			match, err := reviewprovider.MatchCheckpoint(prior.Body, r.Payload)
			if err != nil || match != reviewprovider.CheckpointMatch {
				continue
			}
			evidence, _, err := sourceRouteState(ctx, tx, project, r.View.EvidenceID)
			if err != nil {
				return err
			}
			allowance, _, _, err := readHostedAllowance(ctx, tx, project, evidence.AttemptID)
			if err != nil {
				return err
			}
			if !validHostedAllowanceInput(allowance.Input) || !hostedAllowanceSameSource(allowance.Input.Anchor, r.View.Anchor) {
				continue
			}
			lines := strings.SplitN(r.Intent.Body, "\n", 3)
			if len(lines) != 3 {
				continue
			}
			expected := lines[0] + "\n" + strconv.FormatInt(allowance.Input.ChangedFiles, 10) + " files · " + lines[1] + "\n" + lines[2]
			if strings.Join(strings.Fields(c.Body), " ") != strings.Join(strings.Fields(expected), " ") {
				continue
			}
			if !copied {
				projected.IssueComments = append([]reviewprovider.IssueComment(nil), current.IssueComments...)
				copied = true
			}
			projected.IssueComments[index].UpdatedAt = prior.UpdatedAt
		}
		return nil
	})
	if err != nil {
		return current, err
	}
	return projected, nil
}
