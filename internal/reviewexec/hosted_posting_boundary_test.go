package reviewexec

import (
	"context"
	"strings"
	"testing"
)

func TestReadHostedPostingBoundaryIndependentOfCapabilityAndCurrentEligibility(t *testing.T) {
	f, a, admitted := hostedRecoveryFixture(t)
	f.configure(t, attemptProject, "other-account", 1, true)
	admissionPolicy(t, f, 1)
	admissionSQL(t, f, `UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`)
	reader, err := New(f.store, nil)
	if err != nil {
		t.Fatal(err)
	}
	before := hostedObservationState(t, f, a)
	exact, err := reader.ReadHostedPostingBoundary(t.Context(), attemptProject, a.ID)
	if err != nil || !exact.Equal(admitted) || hostedObservationState(t, f, a) != before {
		t.Fatal("trusted local boundary depended on current execution permission", exact, err)
	}
}

func TestReadHostedPostingBoundaryRejectsWrongScopeChannelAndState(t *testing.T) {
	for _, mode := range []string{"channel", "state", "operation", "repository", "preparing"} {
		t.Run(mode, func(t *testing.T) {
			f, a, _ := hostedRecoveryFixture(t)
			switch mode {
			case "channel":
				admissionSQL(t, f, `UPDATE review_operations SET channel='cli' WHERE id=?`, a.OperationID)
			case "state":
				admissionSQL(t, f, `UPDATE review_operations SET state='completed' WHERE id=?`, a.OperationID)
			case "operation":
				admissionSQL(t, f, `UPDATE review_operations SET attempt_id='' WHERE id=?`, a.OperationID)
			case "repository":
				admissionSQL(t, f, `UPDATE projects SET github_repository_id=999 WHERE id=?`, attemptProject)
			case "preparing":
				admissionSQL(t, f, `UPDATE review_operations SET state='preparing' WHERE id=?`, a.OperationID)
				admissionSQL(t, f, `UPDATE review_execution_attempts SET state='preparing' WHERE id=?`, a.ID)
			}
			before := hostedObservationState(t, f, a)
			exact, err := f.e.ReadHostedPostingBoundary(t.Context(), attemptProject, a.ID)
			attemptCode(t, err, "review_conflict")
			if !exact.IsZero() || hostedObservationState(t, f, a) != before {
				t.Fatal("wrong binding returned a local boundary", exact, err)
			}
		})
	}
	f, a, _ := hostedRecoveryFixture(t)
	_, err := f.e.ReadHostedPostingBoundary(t.Context(), otherAttemptProject, a.ID)
	attemptCode(t, err, "not_found")
	for _, input := range []struct {
		ctx              context.Context
		project, attempt string
	}{{nil, attemptProject, a.ID}, {t.Context(), "../project", a.ID}, {t.Context(), attemptProject, "review_attempt_" + strings.Repeat("x", 32)}} {
		exact, err := f.e.ReadHostedPostingBoundary(input.ctx, input.project, input.attempt)
		attemptCode(t, err, "validation_failed")
		if !exact.IsZero() {
			t.Fatal("invalid selector returned local time")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	exact, err := f.e.ReadHostedPostingBoundary(ctx, attemptProject, a.ID)
	if err == nil || !exact.IsZero() {
		t.Fatal("cancelled local read returned a boundary", exact, err)
	}
}
