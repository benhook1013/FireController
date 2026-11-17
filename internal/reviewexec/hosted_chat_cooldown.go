package reviewexec

import (
	"context"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// checkHostedChatCooldown derives a shared negative gate from the existing
// canonical, capture-bound ledger. It neither creates quota nor refreshes old
// provider timestamps. Account aliases do not cross this explicit account scope.
func checkHostedChatCooldown(ctx context.Context, q EvidenceQuery, provider, account string, posting, bot reviewprovider.Author, now time.Time) error {
	// Bound active candidates, not retained history. Invalid JSON/deadlines remain
	// candidates and fail closed in canonical validation rather than disappearing.
	// SQLite date conversion rounds subsecond values; include a one-second
	// overlap and compare the exact canonical Go timestamp below.
	rows, err := q.QueryContext(ctx, `SELECT a.project_id,a.id FROM review_execution_attempts a JOIN review_hosted_preflight p ON p.attempt_id=a.id WHERE a.provider=? AND a.account=? AND CASE WHEN json_valid(p.body_json) THEN json_extract(p.body_json,'$.input.observation.Reason')=? AND (julianday(json_extract(p.body_json,'$.input.observation.RetryNotBefore')) IS NULL OR julianday(json_extract(p.body_json,'$.input.observation.RetryNotBefore'))>julianday(?,'-1 second')) ELSE 1 END LIMIT 101`, provider, account, reviewprovider.HostedChatCooldownReason, now.Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	var owners [][2]string
	for rows.Next() {
		var owner [2]string
		if err := rows.Scan(&owner[0], &owner[1]); err != nil {
			rows.Close()
			return err
		}
		owners = append(owners, owner)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(owners) > 100 {
		return hostedAllowanceUnavailable()
	}
	blocked := false
	for _, owner := range owners {
		a, err := readAttempt(ctx, q, owner[0], owner[1])
		if err != nil {
			return err
		}
		o, err := readOperationBase(ctx, q, owner[0], a.OperationID)
		if err != nil {
			return err
		}
		record, err := readHostedAllowanceRecord(ctx, q, o, a)
		if err != nil {
			return err
		}
		query, err := readHostedQuery(ctx, q, a, o)
		if err != nil {
			return err
		}
		if a.Provider != provider || a.Account != account {
			return hostedAllowanceUnavailable()
		}
		observation := record.Input.Observation
		if observation.Reason == reviewprovider.HostedChatCooldownReason && observation.RetryNotBefore.After(now) && query.PostingActor.ID == posting.ID && query.PostingActor.Kind == posting.Kind && query.ProviderActor.ID == bot.ID && query.ProviderActor.Kind == bot.Kind {
			blocked = true
		}
	}
	if blocked {
		return sourceHold("hosted_chat_cooldown")
	}
	return nil
}
