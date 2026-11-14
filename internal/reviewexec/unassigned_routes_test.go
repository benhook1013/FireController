package reviewexec

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

func TestUnassignedNativeRoutePreservesOpenWorkButAllowsSourceProgress(t *testing.T) {
	f, a, id := routeFixture(t, 1)
	created := routeCreate(t, f, id, "backlog", routeRequest(0, 0, 0))
	d := routeRead(t, f, created.ID)
	if d.Route.TargetPullNumber != 0 || d.Route.Disposition != "unadjudicated" || d.Route.FixSHA != "" {
		t.Fatal(d)
	}
	adjudicationComplete(t, f, a)
	raw, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "final", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	adjudicationReceipt(t, raw, err, id, "finalized", 2)
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		evidence, state, err := loadReviewAdjudication(ctx, tx, attemptProject, id)
		if err != nil {
			return err
		}
		ready, err := coverageProof(ctx, tx, evidence, state, "cli", false)
		if err != nil {
			return err
		}
		if !ready {
			t.Fatal("unassigned route blocked source")
		}
		progress, err := channelProgress(ctx, tx, attemptProject, evidence.Anchor.RepositoryID, evidence.Anchor.PullNumber, "cli")
		if err != nil {
			return err
		}
		if progress.Pending {
			t.Fatal("source admission remains blocked", progress)
		}

		count, err := pendingUnassignedSourceRoutes(ctx, tx, attemptProject, evidence.Anchor.PullNumber)
		if err != nil {
			return err
		}
		if count != 1 {
			t.Fatal("unresolved work hidden", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request := routeRequest(2, 0, 4)
	request.ExpectedRouteRevision = 1
	assigned := routeCreate(t, f, id, "assign", request)
	if got, err := routePending(t, f, 4); err != nil || got != 1 {
		t.Fatal(got, err)
	}
	// Explicit source revocation restores accepted/unfixed work and keeps history.
	raw, err = f.e.DecideReviewFinding(t.Context(), f.scope(t, attemptProject, f.writer, false), "revoke", id, FindingDecisionRequest{ExpectedRevision: adjudicationRevision(3), Finding: 0, Disposition: "accepted", Reason: "Return responsibility to source", ExpectedRouteRevision: assigned.Revision})
	adjudicationReceipt(t, raw, err, id, "adjudicated", 4)
	state := adjudicationRead(t, f, id)
	if state.Findings[0].Disposition != "accepted" || state.Findings[0].FixSHA != "" || state.FrozenCounts.Routed != 1 {
		t.Fatal(state)
	}
	if got, err := routePending(t, f, 4); err != nil || got != 0 {
		t.Fatal(got, err)
	}
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		evidence, _, err := loadReviewAdjudication(ctx, tx, attemptProject, id)
		if err != nil {
			return err
		}
		progress, err := channelProgress(ctx, tx, attemptProject, evidence.Anchor.RepositoryID, evidence.Anchor.PullNumber, "cli")
		if err == nil && !progress.Pending {
			t.Fatal("revocation did not restore source hold")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUnassignedRouteV1MigrationPreservesHistoryAndAllowsBacklog(t *testing.T) {
	f, _, id := routeFixture(t, 2)
	original := routeCreate(t, f, id, "old-route", routeRequest(0, 0, 4))
	before := routeRead(t, f, original.ID)
	routePolicyPreDomain(t, f)
	if err := f.store.EnsureSchemaWithBackfill(t.Context(), "review_route_policy", [][]string{RoutePolicyV1SchemaSQL()}, backfillRoutePolicyFacts); err != nil {
		t.Fatal(err)
	}
	if err := ensureRoutePolicySchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if after := routeRead(t, f, original.ID); !reflect.DeepEqual(before, after) {
		t.Fatal("migration rewrote route", after)
	}
	routeCreate(t, f, id, "new-backlog", routeRequest(1, 1, 0))
	if err := ValidateRoutePolicyFacts(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if count, err := routePending(t, f, 4); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}
func TestUnassignedNativeRouteDoesNotHideUndecidedSibling(t *testing.T) {
	f, a, id := routeFixture(t, 2)
	routeCreate(t, f, id, "one-backlog", routeRequest(0, 0, 0))
	adjudicationComplete(t, f, a)
	_, err := f.e.FinalizeReviewEvidence(t.Context(), f.scope(t, attemptProject, f.writer, false), "must-not-finalize", id, FinalizeReviewRequest{ExpectedRevision: adjudicationRevision(1)})
	if err == nil {
		t.Fatal("unassigned route silently finalized undecided sibling")
	}
}
