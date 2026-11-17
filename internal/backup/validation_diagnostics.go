package backup

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Fixed, bounded phases only: no row, path, identifier, content or error text.
// Timings partition one validation call, not individual retained records.
type validationPhase string

const (
	validationOpenSnapshot        validationPhase = "open_snapshot"
	validationSchema              validationPhase = "schema"
	validationAdministration      validationPhase = "administration"
	validationIntegrity           validationPhase = "integrity"
	validationForeignKeys         validationPhase = "foreign_keys"
	validationReviewHistory       validationPhase = "review_history"
	validationProjectsJobs        validationPhase = "projects_jobs"
	validationArtifactReferences  validationPhase = "artifact_references"
	validationExecutionControls   validationPhase = "execution_controls"
	validationComparisonProofs    validationPhase = "comparison_proofs"
	validationApplicability       validationPhase = "applicability"
	validationRoutePolicy         validationPhase = "route_policy"
	validationAdoptionObligations validationPhase = "adoption_obligations"
	validationRounds              validationPhase = "rounds"
	validationIntentRebindings    validationPhase = "intent_rebindings"
	validationDecisionHistory     validationPhase = "decision_history"
	validationFronts              validationPhase = "fronts"
	validationSelection           validationPhase = "selection"
	validationScheduling          validationPhase = "scheduling"
	validationReviewOrder         validationPhase = "review_order"
	validationActivityEvidence    validationPhase = "activity_evidence"
)

type validationDiagnostics struct {
	ctx     context.Context
	phase   validationPhase
	started time.Time
}

func beginValidationDiagnostics(ctx context.Context) *validationDiagnostics {
	if ctx == nil {
		ctx = context.Background()
	}
	d := &validationDiagnostics{ctx: ctx}
	d.start(validationOpenSnapshot)
	return d
}
func (d *validationDiagnostics) start(phase validationPhase) {
	d.phase, d.started = phase, time.Now()
	slog.InfoContext(context.WithoutCancel(d.ctx), "Backup validation phase started", "phase", string(phase))
}
func (d *validationDiagnostics) next(phase validationPhase) { d.finish(nil); d.start(phase) }
func (d *validationDiagnostics) finish(err error) {
	slog.InfoContext(context.WithoutCancel(d.ctx), "Backup validation phase outcome", "phase", string(d.phase), "duration_ms", time.Since(d.started).Milliseconds(), "failed", err != nil, "deadline_exceeded", errors.Is(d.ctx.Err(), context.DeadlineExceeded), "cancelled", errors.Is(d.ctx.Err(), context.Canceled))
}
