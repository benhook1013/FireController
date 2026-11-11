package reviews

import (
	"context"
	"database/sql"
	"github.com/benhook1013/FireController/internal/core"
)

type retainedHostedRecoveryKey struct{}
type retainedHostedRecovery struct{ project, attempt, evidence string }

// PublishRecoveredHostedAttempt is only for the trusted journal's atomic,
// audited retained-source repair. The journal must already have validated the
// incomplete release and published the original complete native evidence.
// Import and ordinary publication never receive this narrowly scoped token.
func PublishRecoveredHostedAttempt(ctx context.Context, tx *sql.Tx, scope core.Scope, p NativePublication) (Receipt, error) {
	if p.Attempt == nil || p.Run != nil || p.ExternalHostedSourceID != "" || p.Attempt.Channel != "hosted" || p.Attempt.State != "completed" || p.Attempt.SourceRunID == "" || p.Attempt.Reason != "" {
		return Receipt{}, validationError("Publish only the recovered original Hosted attempt with its native evidence.")
	}
	token := retainedHostedRecovery{scope.ProjectID, p.Attempt.SourceAttemptID, p.Attempt.SourceRunID}
	return PublishNative(context.WithValue(ctx, retainedHostedRecoveryKey{}, token), tx, scope, p)
}
func allowsRetainedHostedRecovery(ctx context.Context, project, source, oldState, oldReason, oldRun string, attempt Attempt) bool {
	token, ok := ctx.Value(retainedHostedRecoveryKey{}).(retainedHostedRecovery)
	return ok && source == nativeSource && token.project == project && token.attempt == attempt.SourceAttemptID && token.evidence != "" && token.evidence == attempt.SourceRunID && oldState == "failed" && oldReason == "hosted_finished_incomplete" && oldRun == "" && attempt.Channel == "hosted" && attempt.State == "completed" && attempt.Reason == ""
}
