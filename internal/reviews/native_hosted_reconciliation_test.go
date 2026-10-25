package reviews

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func hostedReconciliationRun(id string) RunInput {
	run := nativeRun(id)
	run.Channel = "hosted"
	return run
}

func hostedReconcile(f *apiFixture, number int64, head, nativeID, externalID string) (Receipt, bool, error) {
	var receipt Receipt
	var changed bool
	err := f.store.BackgroundWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		receipt, changed, err = ReconcileNativeHostedSource(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "synthetic-native-reconciler"}, number, head, nativeID, externalID)
		return err
	})
	return receipt, changed, err
}

func hostedReconciliationExternal(t *testing.T, f *apiFixture, number int64) (RunInput, Receipt) {
	t.Helper()
	p := nativePull(number)
	historyObservationPost(t, f, reviewRequest(0, []PullInput{p}), "reconciliation-original")
	if number != 1 {
		t.Fatal("selected test helper supports PR1")
	}
	externalInit(t, f)
	run := externalRun(1)
	run.HeadSHA = strings.Repeat("a", 40)
	return run, externalMustPublish(t, f, run)
}

func TestNativeHostedReconciliationExternalFirstRetainsOriginalHistoryAndNativeDecisions(t *testing.T) {
	f := newAPIFixture(t)
	external, before := hostedReconciliationExternal(t, f, 1)
	originalSnapshot := nativeSnapshotBytes(t, f, before.ID)
	var oldDetail RunDetailResponse
	externalID := runOpaqueID(f.project, externalHostedSource, external.SourceRunID)
	originalDetail := nativeRead(t, f, "/runs/"+externalID+"?snapshot="+before.ID, &oldDetail)
	run := hostedReconciliationRun("native-replacement")
	published := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run, ExternalHostedSourceID: external.SourceRunID})
	current := channelCountsPull(t, f, "")
	if current.Pull.RunCounts.Hosted != 1 || current.Pull.RunCount != 1 || len(current.Pull.RecentRuns) != 1 || current.Pull.RecentRuns[0].SourceRunID != run.SourceRunID || !current.Pull.RecentRuns[0].Finalized || current.Pull.RecentRuns[0].Accepted != 1 {
		t.Fatal(current)
	}
	if nativeSnapshotBytes(t, f, before.ID) != originalSnapshot {
		t.Fatal("external source snapshot changed")
	}
	var retained RunDetailResponse
	if after := nativeRead(t, f, "/runs/"+externalID+"?snapshot="+before.ID, &retained); string(after) != string(originalDetail) {
		t.Fatal("historical external finding changed")
	}
	var native RunDetailResponse
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, run.SourceRunID), &native)
	if !reflect.DeepEqual(plainFindingsForTest(native.Findings), run.Findings) || native.Run.Accepted != 1 || native.Run.Fixed != 1 || !native.Run.Finalized {
		t.Fatal("native adjudication was replaced by observer state", native)
	}
	var currentExternal, versions, findings int
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_current_runs WHERE project_id=? AND id=?`, f.project, externalID).Scan(&currentExternal); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_run_versions WHERE project_id=? AND id=?`, f.project, externalID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_finding_versions WHERE project_id=? AND run_id=?`, f.project, externalID).Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if currentExternal != 0 || versions != 1 || findings != 1 {
		t.Fatal(currentExternal, versions, findings)
	}
	assertCurrentSummaryReduction(t, f, 1)
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	beforeReplay := historyMigrationDatabaseDigest(t, f)
	if receipt, changed, err := hostedReconcile(f, 1, run.HeadSHA, run.SourceRunID, external.SourceRunID); err != nil || changed || receipt != published {
		t.Fatal(receipt, changed, err)
	}
	if historyMigrationDatabaseDigest(t, f) != beforeReplay {
		t.Fatal("idempotent reconciliation changed retained data")
	}
}

func TestNativeHostedReconciliationRepairsExistingDuplicateInEitherArrivalOrder(t *testing.T) {
	for _, nativeFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "external-first", true: "native-first"}[nativeFirst], func(t *testing.T) {
			f := newAPIFixture(t)
			historyObservationPost(t, f, reviewRequest(0, []PullInput{nativePull(1)}), "repair-original")
			externalInit(t, f)
			run := hostedReconciliationRun("native-installed")
			external := externalRun(1)
			external.HeadSHA = run.HeadSHA
			if nativeFirst {
				mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
				externalMustPublish(t, f, external)
			} else {
				externalMustPublish(t, f, external)
				mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
			}
			duplicate := channelCountsPull(t, f, "")
			snapshot := duplicate.SnapshotID
			if duplicate.Pull.RunCounts.Hosted != 2 {
				t.Fatal("fixture must contain both projections", duplicate)
			}
			original := nativeSnapshotBytes(t, f, snapshot)
			var nativeBefore RunDetailResponse
			nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, run.SourceRunID), &nativeBefore)
			receipt, changed, err := hostedReconcile(f, 1, run.HeadSHA, run.SourceRunID, external.SourceRunID)
			if err != nil || !changed || receipt.Revision != duplicate.Revision+1 {
				t.Fatal(receipt, changed, err)
			}
			if got := channelCountsPull(t, f, ""); got.Pull.RunCounts.Hosted != 1 || got.Pull.RunCount != 1 {
				t.Fatal(got)
			}
			if nativeSnapshotBytes(t, f, snapshot) != original || channelCountsPull(t, f, snapshot).Pull.RunCounts.Hosted != 2 {
				t.Fatal("repair rewrote old duplicate snapshot")
			}
			var nativeAfter RunDetailResponse
			nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, run.SourceRunID), &nativeAfter)
			if !reflect.DeepEqual(nativeBefore.Run, nativeAfter.Run) || !reflect.DeepEqual(nativeBefore.Findings, nativeAfter.Findings) {
				t.Fatal("repair touched native evidence")
			}
			assertCurrentSummaryReduction(t, f, 1)
		})
	}
}

func TestNativeHostedReconciliationRequiresExactCurrentReplacement(t *testing.T) {
	for _, kind := range []string{"missing-native", "omitted-native", "wrong-native-pr", "wrong-native-head", "wrong-external-head", "other-producer", "other-project", "wrong-repository", "incomplete-native"} {
		t.Run(kind, func(t *testing.T) {
			f := newAPIFixture(t)
			external, _ := hostedReconciliationExternal(t, f, 1)
			run := hostedReconciliationRun("guard-native")
			if kind == "wrong-native-head" {
				run.HeadSHA = strings.Repeat("b", 40)
			}
			if kind == "incomplete-native" {
				run.Complete, run.Attributable, run.Finalized = false, false, false
				run.Outcome = "failed"
			}
			pull := nativePull(1)
			if kind == "wrong-native-pr" {
				pull = nativePull(2)
			}
			if kind != "missing-native" && kind != "other-project" && kind != "other-producer" {
				mustNativePublish(t, f, NativePublication{Pull: pull, Run: &run})
			}
			if kind == "omitted-native" {
				p := nativePull(1)
				p.Runs = []RunInput{external}
				request := reviewRequest(3, []PullInput{p})
				request.Source = externalHostedSource
				historyObservationPost(t, f, request, "omit-replacement-only")
				current := channelCountsPull(t, f, "")
				if current.Pull.RunCounts.Hosted != 1 || len(current.Pull.RecentRuns) != 1 || current.Pull.RecentRuns[0].Source != externalHostedSource {
					t.Fatal("fixture must retain observer while omitting native", current)
				}
			}
			if kind == "wrong-repository" {
				if _, err := f.store.DB().Exec(`UPDATE projects SET github_repository_id=1701 WHERE id=?`, f.project); err != nil {
					t.Fatal(err)
				}
			}
			before := historyMigrationDatabaseDigest(t, f)
			nativeID := run.SourceRunID
			head := strings.Repeat("a", 40)
			externalID := external.SourceRunID
			if kind == "wrong-external-head" {
				head = strings.Repeat("b", 40)
				nativeID = "missing-bound-replacement"
				other := hostedReconciliationRun(nativeID)
				other.HeadSHA = head
				mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &other})
				before = historyMigrationDatabaseDigest(t, f)
			}
			if kind == "other-producer" {
				externalID = "publication:00000000-0000-0000-0000-000000000099"
				p := nativePull(1)
				observer := externalRun(99)
				observer.SourceRunID = externalID
				p.Runs = []RunInput{observer}
				var revision int64
				if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&revision); err != nil {
					t.Fatal(err)
				}
				historyObservationPost(t, f, reviewRequest(revision, []PullInput{p}), "other-import-producer")
				mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
				before = historyMigrationDatabaseDigest(t, f)
			}
			var receipt Receipt
			var changed bool
			var err error
			if kind == "other-project" {
				err = f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
					receipt, changed, err = ReconcileNativeHostedSource(ctx, tx, core.Scope{ProjectID: "unrelated-project", PrincipalID: "synthetic"}, 1, head, nativeID, externalID)
					return err
				})
			} else {
				receipt, changed, err = hostedReconcile(f, 1, head, nativeID, externalID)
			}
			if kind == "wrong-native-pr" || kind == "wrong-native-head" || kind == "wrong-external-head" || kind == "wrong-repository" || kind == "incomplete-native" {
				nativeErrorCode(t, err, "observation_conflict")
			} else if err != nil {
				t.Fatal(receipt, changed, err)
			}
			if changed || historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("unproven identity changed projection", kind, receipt, changed)
			}
		})
	}
}

func TestNativeHostedReconciliationPublicationValidationAndAtomicRollback(t *testing.T) {
	for _, kind := range []string{"cli-alias", "incomplete-alias", "missing-run", "bad-alias", "event-failure", "wrong-external-head"} {
		t.Run(kind, func(t *testing.T) {
			f := newAPIFixture(t)
			external, _ := hostedReconciliationExternal(t, f, 1)
			run := hostedReconciliationRun("atomic-native")
			p := NativePublication{Pull: nativePull(1), Run: &run, ExternalHostedSourceID: external.SourceRunID}
			switch kind {
			case "cli-alias":
				run.Channel = "cli"
			case "incomplete-alias":
				run.Complete = false
			case "missing-run":
				p.Run = nil
				p.Attempt = &Attempt{SourceAttemptID: "one", Channel: "hosted", State: "active"}
			case "bad-alias":
				p.ExternalHostedSourceID = "not-a-provider-identity"
			case "wrong-external-head":
				run.HeadSHA = strings.Repeat("b", 40)
			case "event-failure":
				if _, err := f.store.DB().Exec(`CREATE TRIGGER reconcile_event_failure BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'synthetic reconciliation failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			before := historyMigrationDatabaseDigest(t, f)
			_, err := nativePublish(f, p)
			if err == nil {
				t.Fatal("unsafe publication accepted")
			}
			if kind != "event-failure" && kind != "wrong-external-head" {
				nativeErrorCode(t, err, "validation_failed")
			}
			if historyMigrationDatabaseDigest(t, f) != before {
				t.Fatal("failed publication changed current or historical bytes")
			}
		})
	}
	// Reconciliation after an already-installed duplicate is atomic as well.
	f := newAPIFixture(t)
	external, _ := hostedReconciliationExternal(t, f, 1)
	run := hostedReconciliationRun("already-native")
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	before := historyMigrationDatabaseDigest(t, f)
	_, _, err := func() (Receipt, bool, error) {
		var r Receipt
		var changed bool
		err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			var err error
			r, changed, err = ReconcileNativeHostedSource(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "synthetic"}, 1, run.HeadSHA, run.SourceRunID, external.SourceRunID)
			if err != nil {
				return err
			}
			return errors.New("synthetic caller rollback")
		})
		return r, changed, err
	}()
	if err == nil || historyMigrationDatabaseDigest(t, f) != before {
		t.Fatal("outer rollback lost retained evidence", err)
	}
}

func TestNativeHostedReconciliationOldCapturedHeadPreservesCurrentPRFacts(t *testing.T) {
	f := newAPIFixture(t)
	external, _ := hostedReconciliationExternal(t, f, 1)
	current := nativePull(1)
	current.HeadSHA = strings.Repeat("c", 40)
	current.Title = "Newer retained PR metadata"
	// A native attempt updates current facts without inventing a captured run.
	attempt := nativeAttempt("newer-metadata", "active")
	attempt.Channel = "hosted"
	attempt.HeadSHA = current.HeadSHA
	mustNativePublish(t, f, NativePublication{Pull: current, Attempt: &attempt})
	run := hostedReconciliationRun("older-comparison-capture")
	mustNativePublish(t, f, NativePublication{Pull: current, Run: &run, ExternalHostedSourceID: external.SourceRunID})
	got := channelCountsPull(t, f, "")
	if got.Pull.HeadSHA != current.HeadSHA || got.Pull.Title != current.Title || got.Pull.RunCounts.Hosted != 1 || len(got.Pull.RecentRuns) != 1 || got.Pull.RecentRuns[0].HeadSHA != run.HeadSHA {
		t.Fatal("old source replaced current PR metadata", got)
	}
}

func TestNativeHostedReconciliationIdentityConflictDoesNotDirtyContinuingTransaction(t *testing.T) {
	f := newAPIFixture(t)
	external, _ := hostedReconciliationExternal(t, f, 1)
	run := hostedReconciliationRun("moved-comparison")
	run.HeadSHA = strings.Repeat("b", 40)
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	before := historyMigrationDatabaseDigest(t, f)
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		_, changed, err := ReconcileNativeHostedSource(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "synthetic"}, 1, run.HeadSHA, run.SourceRunID, external.SourceRunID)
		nativeErrorCode(t, err, "observation_conflict")
		if changed {
			t.Fatal("identity conflict reported change")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if historyMigrationDatabaseDigest(t, f) != before {
		t.Fatal("handled identity conflict left staged mutations")
	}
}
