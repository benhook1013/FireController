package reviewprovider

import (
	"strings"
	"time"
)

// GitHub timestamps have second precision and may trail the admitting server's
// wall clock. Only the recovery lower bound permits this fixed five-second skew;
// original provider timestamps and all immutable ownership checks stay intact.
const hostedProviderClockSkew = 5 * time.Second

// HostedTriggerIntent binds the one fixed hosted command to a retained local
// operation, attempt and candidate. It accepts no caller-supplied command prose.
type HostedTriggerIntent struct {
	OperationID string
	AttemptID   string
	HeadSHA     string
}

// HostedTriggerRecovery identifies a posted trigger only. Even an adopted
// trigger says nothing about execution, completion, coverage or allowance.
// Neither uncertain nor ambiguous establishes that reposting would be safe.
type HostedTriggerRecovery struct {
	State     string
	Reason    string
	CommentID int64
	CreatedAt time.Time
}

// HostedTriggerBody generates the entire command body, including its versioned
// application marker. The exact bytes are used for response-loss recovery.
func HostedTriggerBody(intent HostedTriggerIntent) (string, error) {
	if !hostedTriggerID(intent.OperationID, "review_operation_") || !hostedTriggerID(intent.AttemptID, "review_attempt_") || !validSHA(intent.HeadSHA) || strings.ToLower(intent.HeadSHA) != intent.HeadSHA {
		return "", invalidSelector()
	}
	return "@coderabbitai full review\n\n" + hostedTriggerMarker(intent), nil
}

func hostedTriggerMarker(intent HostedTriggerIntent) string {
	return "<!-- firecontroller-review-trigger v1 operation=" + intent.OperationID + " attempt=" + intent.AttemptID + " head=" + intent.HeadSHA + " -->"
}

func hostedTriggerID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+32 {
		return false
	}
	for _, c := range value[len(prefix):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func hostedTriggerTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && value.Year() >= 1 && value.Year() <= 9999
}

// RecoverHostedTrigger consumes an explicitly complete collection already scoped
// to the selected repository and PR by its caller. It performs no external work.
// Stable numeric ID and actor kind establish ownership; login is display data.
// An edited comment cannot establish the original command body after response
// loss, even when its current bytes match the intended command.
func RecoverHostedTrigger(comments []IssueComment, complete bool, intent HostedTriggerIntent, postingAuthor Author, earliestPosting, observedAt time.Time) (HostedTriggerRecovery, error) {
	body, err := HostedTriggerBody(intent)
	if err != nil {
		return HostedTriggerRecovery{}, err
	}
	return recoverHostedCommand(comments, complete, intent, postingAuthor, earliestPosting, observedAt, body, "firecontroller-review-trigger")
}

// recoverHostedCommand shares immutable command identity, source bounds and
// lost-response recovery between the two fixed application-owned commands.
func recoverHostedCommand(comments []IssueComment, complete bool, intent HostedTriggerIntent, postingAuthor Author, earliestPosting, observedAt time.Time, body, markerFamily string) (HostedTriggerRecovery, error) {
	_, actorErr := normalizeActor(githubActor{ID: postingAuthor.ID, Login: postingAuthor.Login, Type: postingAuthor.Kind})
	if actorErr != nil || !hostedTriggerTime(earliestPosting) || !hostedTriggerTime(observedAt) || observedAt.Before(earliestPosting) {
		return HostedTriggerRecovery{}, invalidSelector()
	}
	uncertain := HostedTriggerRecovery{State: "uncertain", Reason: "trigger_not_proven"}
	ambiguous := HostedTriggerRecovery{State: "ambiguous", Reason: "conflicting_trigger_evidence"}
	if len(comments) > githubMaxItems {
		return ambiguous, nil
	}
	if !complete {
		uncertain.Reason = "incomplete_collection"
		return uncertain, nil
	}
	seen := make(map[int64]struct{}, len(comments))
	lowerBound := earliestPosting.Add(-hostedProviderClockSkew)
	total := 0
	var match *IssueComment
	for i := range comments {
		comment := &comments[i]
		total += len(comment.Body)
		_, actorErr := normalizeActor(githubActor{ID: comment.Author.ID, Login: comment.Author.Login, Type: comment.Author.Kind})
		if total > githubMaxAggregate || !validBody(comment.Body) || comment.ID <= 0 || actorErr != nil || !hostedTriggerTime(comment.CreatedAt) || !hostedTriggerTime(comment.UpdatedAt) || comment.UpdatedAt.Before(comment.CreatedAt) || comment.UpdatedAt.After(observedAt) {
			return ambiguous, nil
		}
		if _, duplicate := seen[comment.ID]; duplicate {
			return ambiguous, nil
		}
		seen[comment.ID] = struct{}{}
		// A quoted, fenced, modified or malformed copy of this owned marker is
		// conflicting evidence, never a second format accepted for adoption.
		ownedMarker := strings.Contains(comment.Body, markerFamily) && (strings.Contains(comment.Body, "operation="+intent.OperationID) || strings.Contains(comment.Body, "attempt="+intent.AttemptID))
		if comment.Body != body && !ownedMarker {
			continue
		}
		if comment.Body != body || comment.Author.ID != postingAuthor.ID || comment.Author.Kind != postingAuthor.Kind || comment.CreatedAt.Before(lowerBound) || comment.CreatedAt.After(observedAt) || !comment.UpdatedAt.Equal(comment.CreatedAt) || match != nil {
			return ambiguous, nil
		}
		match = comment
	}
	if match == nil {
		return uncertain, nil
	}
	return HostedTriggerRecovery{State: "adopted", Reason: "exact_trigger_found", CommentID: match.ID, CreatedAt: match.CreatedAt}, nil
}
