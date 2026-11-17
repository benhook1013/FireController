package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestExternalRoundCreditBackupRetainsAuthorityAndRefusesTampering(t *testing.T) {
	f := newRestoreFixture(t)
	scope := routePolicyInventoryScope(t, f)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err = f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	pull := reviews.PullInput{Number: 4, Title: "External result target", State: "open", HeadSHA: head, BaseRef: "main", BaseSHA: base, CIState: "pending", Mergeability: "unknown", Complete: true}
	f.request(t, "POST", "/api/v1/projects/"+f.project+"/reviews/observations", reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: 991, Source: "external-credit-fixture", ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Queue: []reviews.PullInput{
		{Number: 1, Title: "Retained merged history", State: "merged", BaseRef: "main", CIState: "success", Mergeability: "mergeable", Complete: true},
		{Number: 2, Title: "Current source", State: "open", HeadSHA: head, BaseRef: "main", BaseSHA: base, CIState: "pending", Mergeability: "unknown", Complete: true},
		pull,
	}})
	zero, maximum := int64(0), int64(3)
	if _, err = e.ConfigureRoundControl(t.Context(), scope, "external-credit-grant", 4, "hosted", reviewexec.RoundControlRequest{ExpectedRevision: &zero, Action: "grant", Minimum: &zero, Maximum: &maximum}); err != nil {
		t.Fatal(err)
	}
	const sourceID = "publication:11111111-1111-1111-1111-111111111111"
	err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := reviews.InitializeExternalHostedFrontier(ctx, tx, f.project, 991, 4); err != nil {
			return err
		}
		run := reviews.RunInput{SourceRunID: sourceID, Channel: "hosted", HeadSHA: head, Reviewer: "Synthetic provider", Scope: "Original provider comparison", Outcome: "completed", Attributable: true, Complete: true, StartedAt: "2026-10-20T00:01:00Z", FinishedAt: "2026-10-20T00:01:00Z"}
		_, _, err := reviews.PublishExternalHosted(ctx, tx, scope, 991, 4, []reviews.RunInput{run})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	count := true
	runID := reviews.ExternalHostedRunID(f.project, sourceID)
	request := reviewexec.ExternalHostedSettlement{CountRound: &count, Runs: []reviewexec.ExternalHostedSelectedRun{{RunID: runID, Findings: []reviewexec.ExternalHostedDecision{}}}}
	if _, err = e.SettleExternalHosted(t.Context(), scope, "external-credit-settle", 4, request); err != nil {
		t.Fatal(err)
	}
	original := backupRows(t, f.store.DB())
	bundle := restoreBundle(t, f)
	destination := filepath.Join(t.TempDir(), "inactive")
	if got, err := Restore(t.Context(), bundle.Path, destination); err != nil || got.State != "inactive" {
		t.Fatal("external credit was not restorable", got, err)
	}
	db, err := openSnapshot(t.Context(), filepath.Join(destination, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = reviewexec.ValidateRoundSnapshot(t.Context(), db); err != nil {
		t.Fatal("restored external authority unavailable", err)
	}
	restored := backupRows(t, db)
	for _, table := range []string{"review_round_external_credits", "review_round_external_members", "review_run_versions", "review_round_controls"} {
		if restored[table] != original[table] {
			t.Fatal("inactive restore changed retained round authority", table)
		}
	}
	for _, tc := range []struct{ name, query string }{
		{"missing-member", `DELETE FROM review_round_external_members`},
		{"wrong-member", `UPDATE review_round_external_members SET run_id='review_run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'`},
		{"wrong-accepted", `UPDATE review_round_external_credits SET accepted=1`},
		{"wrong-ordinal", `UPDATE review_round_external_credits SET ordinal=2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := restoreBundle(t, f)
			mutateBundleSQL(t, copy.Path, tc.query)
			assertCurrentSummaryInventoryRefused(t, copy, true)
		})
	}
	if !reflect.DeepEqual(original, backupRows(t, f.store.DB())) {
		t.Fatal("backup/restore changed the original source")
	}
}
