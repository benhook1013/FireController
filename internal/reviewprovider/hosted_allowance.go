package reviewprovider

import (
	"regexp"
	"strconv"
	"time"
)

// HostedAllowanceSnapshot is one explicitly complete scoped comment history.
// Repository/PR selection and observation freshness belong to its trusted caller.
type HostedAllowanceSnapshot struct {
	IssueComments []IssueComment
	Complete      bool
	ObservedAt    time.Time
}

// HostedAllowanceObservation retains only an attributed provider observation.
// Neither observed state grants execution, numeric capacity or billing proof.
// RetryNotBefore includes rounding uncertainty; it is not an exact quota reset.
type HostedAllowanceObservation struct {
	State             string
	Reason            string
	QueryCommentID    int64
	ResponseCommentID int64
	QueryCreatedAt    time.Time
	ResponseCreatedAt time.Time
	RetryNotBefore    time.Time
}

// HostedAllowanceQueryBody generates exactly one non-review command, bound to
// the same immutable operation/attempt/head identity as the final review intent.
func HostedAllowanceQueryBody(intent HostedTriggerIntent) (string, error) {
	if _, err := HostedTriggerBody(intent); err != nil {
		return "", err
	}
	return "@coderabbitai rate limit\n\n<!-- firecontroller-allowance-query v1 operation=" + intent.OperationID + " attempt=" + intent.AttemptID + " head=" + intent.HeadSHA + " -->", nil
}

var hostedAllowanceCooldown = regexp.MustCompile(`^<!-- This is an auto-generated reply by CodeRabbit -->\nYour \[plan\]\(https://docs\.coderabbit\.ai/management/plans(?:#fair-usage-limits-policy)?\) includes PR reviews subject to \[rate limits\]\(https://docs\.coderabbit\.ai/management/plans#rate-limits\)\. More reviews will be available in ([1-9][0-9]{0,3}) (minute|minutes)\.$`)
var hostedAllowanceCommand = regexp.MustCompile(`(?i)@coderabbitai(?:[ \t]|$)`)

const hostedAllowanceAvailable = "<!-- This is an auto-generated reply by CodeRabbit -->\nYour [plan](https://docs.coderabbit.ai/management/plans#fair-usage-limits-policy) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). Reviews are available now."
const hostedAllowanceAvailableBasePlan = "<!-- This is an auto-generated reply by CodeRabbit -->\nYour [plan](https://docs.coderabbit.ai/management/plans) includes PR reviews subject to [rate limits](https://docs.coderabbit.ai/management/plans#rate-limits). Reviews are available now."

// ResolveHostedAllowance performs no I/O. It recognizes only the independently
// observed availability/cooldown grammar, never infers numeric allowance and never
// adopts an edited query/reply or incomplete history as current provider proof.
func ResolveHostedAllowance(snapshot HostedAllowanceSnapshot, intent HostedTriggerIntent, postingAuthor, providerAuthor Author, earliestPosting time.Time) (HostedAllowanceObservation, error) {
	result := HostedAllowanceObservation{State: "unproven", Reason: "allowance_not_proven"}
	body, err := HostedAllowanceQueryBody(intent)
	if err != nil || !publicationValidActor(providerAuthor) || publicationActor(postingAuthor, providerAuthor) {
		return result, invalidSelector()
	}
	query, err := recoverHostedCommand(snapshot.IssueComments, snapshot.Complete, intent, postingAuthor, earliestPosting, snapshot.ObservedAt, body, "firecontroller-allowance-query")
	if err != nil {
		return result, err
	}
	if query.State != "adopted" {
		if query.State == "ambiguous" {
			result.State, result.Reason = "ambiguous", "conflicting_query_evidence"
		} else if !snapshot.Complete {
			result.Reason = "incomplete_collection"
		}
		return result, nil
	}
	result.QueryCommentID, result.QueryCreatedAt = query.CommentID, query.CreatedAt
	// A competing command closes causal attribution to this query. An edited
	// older command has unknown original bytes, so close at the query itself.
	var closing time.Time
	for _, comment := range snapshot.IssueComments {
		if comment.ID == query.CommentID || publicationActor(comment.Author, providerAuthor) {
			continue
		}
		// Current prose cannot prove an edited comment never contained a
		// competing command. Close before its possible original reply, even
		// when that command was removed or hidden by the edit.
		if !comment.UpdatedAt.Equal(comment.CreatedAt) && !comment.UpdatedAt.Before(query.CreatedAt) {
			when := comment.CreatedAt
			if when.Before(query.CreatedAt) {
				when = query.CreatedAt
			}
			if closing.IsZero() || when.Before(closing) {
				closing = when
			}
			continue
		}
		command := false
		for _, line := range publicationVisibleLines(comment.Body) {
			if hostedAllowanceCommand.MatchString(line) {
				command = true
				break
			}
		}
		if !command {
			continue
		}
		when := comment.CreatedAt
		if when.Before(query.CreatedAt) {
			if comment.UpdatedAt.Before(query.CreatedAt) {
				continue
			}
			when = query.CreatedAt
		}
		if closing.IsZero() || when.Before(closing) {
			closing = when
		}
	}
	var response *IssueComment
	var responseState string
	var responseMinutes int64
	unsupported := false
	for index := range snapshot.IssueComments {
		comment := &snapshot.IssueComments[index]
		if !publicationActor(comment.Author, providerAuthor) || !comment.CreatedAt.After(query.CreatedAt) || !closing.IsZero() && !comment.CreatedAt.Before(closing) {
			continue
		}
		// Full comment validation and <=ObservedAt were established by recovery.
		// Unknown or edited provider replies cannot be discarded in favour of an
		// apparent supported answer from the same otherwise uncorrelated window.
		if !comment.UpdatedAt.Equal(comment.CreatedAt) {
			unsupported = true
			continue
		}
		state, minutes := "available_observed", int64(0)
		if comment.Body != hostedAllowanceAvailable && comment.Body != hostedAllowanceAvailableBasePlan {
			match := hostedAllowanceCooldown.FindStringSubmatch(comment.Body)
			if match == nil {
				unsupported = true
				continue
			}
			minutes, err = strconv.ParseInt(match[1], 10, 64)
			if err != nil || minutes < 1 || minutes >= 1440 || (minutes == 1) != (match[2] == "minute") {
				unsupported = true
				continue
			}
			state = "cooldown_observed"
		}
		if response != nil {
			result.State, result.Reason = "ambiguous", "conflicting_allowance_replies"
			return result, nil
		}
		response = comment
		responseState, responseMinutes = state, minutes
	}
	if unsupported {
		result.Reason = "allowance_reply_unsupported"
		return result, nil
	}
	if response == nil {
		if !closing.IsZero() {
			result.Reason = "query_window_closed"
		}
		return result, nil
	}
	var retry time.Time
	if responseState == "cooldown_observed" {
		retry = response.CreatedAt.Add(time.Duration(responseMinutes+1) * time.Minute)
		if !hostedTriggerTime(retry) {
			result.Reason = "allowance_reply_unsupported"
			return result, nil
		}
	}
	result.State, result.Reason = responseState, "provider_"+responseState
	result.ResponseCommentID, result.ResponseCreatedAt, result.RetryNotBefore = response.ID, response.CreatedAt, retry
	return result, nil
}
