package reviews

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func applyNativeAdjudication(f *apiFixture, input NativeAdjudication) (Receipt, bool, error) {
	var receipt Receipt
	var changed bool
	err := f.store.BackgroundWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		receipt, changed, err = UpdateNativeAdjudication(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "native-adjudicator"}, input)
		return err
	})
	return receipt, changed, err
}

func nativeAdjudicationInput(run RunInput) NativeAdjudication {
	input := NativeAdjudication{SourceRunID: run.SourceRunID, Finalized: run.Finalized, Findings: make([]NativeFindingUpdate, len(run.Findings))}
	if run.Finalized {
		input.FrozenCounts = &NativeCounts{Found: run.Found, Accepted: run.Accepted, Routed: run.Routed}
	}
	for i, finding := range run.Findings {
		input.Findings[i] = NativeFindingUpdate{SourceFindingID: finding.SourceFindingID, Disposition: finding.Disposition, DecisionReason: finding.DecisionReason, Actor: finding.Actor, DecidedAt: finding.DecidedAt, FixSHA: finding.FixSHA, ProofNote: finding.ProofNote, ResolvedAt: finding.ResolvedAt}
	}
	return input
}

func nativeUndecidedRun(id string, count int) RunInput {
	run := nativeRun(id)
	run.Found, run.Accepted, run.Routed, run.Finalized = int64(count), 0, 0, false
	run.Findings = make([]Finding, count)
	for i := range run.Findings {
		run.Findings[i] = Finding{SourceFindingID: fmt.Sprintf("finding-%d", i), Title: fmt.Sprintf("Immutable title %d", i), Detail: fmt.Sprintf("IMMUTABLE_NATIVE_SOURCE_%d", i), Severity: "high", Disposition: "unadjudicated"}
	}
	return run
}

func nativeAdjudicationFixture(t *testing.T) (*apiFixture, NativeAdjudication, Receipt) {
	t.Helper()
	f := newAPIFixture(t)
	imported := nativePull(1)
	imported.Runs = []RunInput{nativeRun("same-source-run")}
	f.observe(t, reviewRequest(0, []PullInput{imported, nativePull(2)}), "import-before-adjudication")
	run := nativeUndecidedRun("same-source-run", 2)
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	// Metadata has moved on since capture. A disposition update has no authority
	// to replace these current facts with the capture's older admitted head.
	metadata := nativePull(1)
	metadata.Title, metadata.State = "Current PR metadata", "open"
	metadata.HeadSHA, metadata.BaseSHA = strings.Repeat("9", 40), strings.Repeat("8", 40)
	metadata.CIState, metadata.Mergeability = "pending", "conflicting"
	attempt := nativeAttempt("newer-source-attempt", "active")
	attempt.HeadSHA = metadata.HeadSHA
	current := mustNativePublish(t, f, NativePublication{Pull: metadata, Attempt: &attempt})
	return f, nativeAdjudicationInput(run), current
}

func decideNativeFinding(finding *NativeFindingUpdate, disposition string) {
	finding.Disposition, finding.DecisionReason, finding.Actor, finding.DecidedAt = disposition, "Exact retained rationale\nwith source evidence", "native-adjudicator", "2026-01-01T00:04:00Z"
}

func TestNativeAdjudicationMissingCurrentProjectionIsNoOp(t *testing.T) {
	f := newAPIFixture(t)
	input := nativeAdjudicationInput(nativeUndecidedRun("same-source-run", 0))
	for _, state := range []string{"unconfigured", "imported-only", "other-project-only", "historical-native-only"} {
		t.Run(state, func(t *testing.T) {
			switch state {
			case "imported-only":
				pull := nativePull(1)
				pull.Runs = []RunInput{nativeRun(input.SourceRunID)}
				f.observe(t, reviewRequest(0, []PullInput{pull}), "import-matching-external-id")
			case "other-project-only":
				if _, err := f.store.DB().Exec(`INSERT INTO projects VALUES('other-project','Other',702,'owner/other')`); err != nil {
					t.Fatal(err)
				}
				other := *f
				other.project = "other-project"
				run := nativeUndecidedRun(input.SourceRunID, 0)
				mustNativePublish(t, &other, NativePublication{Pull: nativePull(1), Run: &run})
			case "historical-native-only":
				run := nativeUndecidedRun(input.SourceRunID, 0)
				mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
				f.observe(t, reviewRequest(2, []PullInput{nativePull(1)}), "new-current-without-native")
			}
			counts := nativeProjectionCounts(t, f)
			receipt, changed, err := applyNativeAdjudication(f, input)
			if err != nil || changed || receipt != (Receipt{}) || !reflect.DeepEqual(nativeProjectionCounts(t, f), counts) {
				t.Fatal("missing current native projection produced effects", receipt, changed, err)
			}
		})
	}
}

func nativeProjectionCounts(t *testing.T, f *apiFixture) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"review_snapshots", "review_pulls", "review_runs", "review_findings", "review_attempts", "review_retired_pulls", "review_workspace", "events", "receipts"} {
		counts[table] = countRows(t, f.store, table)
	}
	return counts
}

func TestNativeAdjudicationProjectsCorrectionsWithoutRewritingSourceOrPRFacts(t *testing.T) {
	f, input, previous := nativeAdjudicationFixture(t)
	oldDigest := nativeSnapshotBytes(t, f, previous.ID)
	var initialQueue QueueResponse
	nativeRead(t, f, "/queue", &initialQueue)
	var oldNative, oldImported RunDetailResponse
	runID := runOpaqueID(f.project, nativeSource, input.SourceRunID)
	importedID := runOpaqueID(f.project, "test-import", input.SourceRunID)
	nativeRead(t, f, "/runs/"+runID, &oldNative)
	oldImportedBytes := nativeRead(t, f, "/runs/"+importedID, &oldImported)
	var oldPull PullResponse
	nativeRead(t, f, "/pulls/1", &oldPull)
	oldOtherPull := initialQueue.Items[1]
	if _, err := f.store.DB().Exec(`INSERT INTO projects VALUES('other-native-project','Other',703,'owner/other-native')`); err != nil {
		t.Fatal(err)
	}
	other := *f
	other.project = "other-native-project"
	otherRun := nativeUndecidedRun(input.SourceRunID, 2)
	otherReceipt := mustNativePublish(t, &other, NativePublication{Pull: nativePull(1), Run: &otherRun})
	otherDigest := nativeSnapshotBytes(t, &other, otherReceipt.ID)
	counts := nativeProjectionCounts(t, f)
	receipt, changed, err := applyNativeAdjudication(f, input)
	if err != nil || changed || receipt != previous || !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) {
		t.Fatal("unchanged initial state cloned a snapshot", err)
	}

	decideNativeFinding(&input.Findings[0], "accepted")
	decideNativeFinding(&input.Findings[1], "rejected")
	decided, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed || decided.Revision != previous.Revision+1 {
		t.Fatal("decisions were not projected", err)
	}
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+runID, &detail)
	if detail.Run.Accepted != 1 || detail.Run.Finalized || detail.Run.Fixed != 0 || detail.Findings[1].Disposition != "rejected" {
		t.Fatal("unfinalized decision counts wrong")
	}

	input.Findings[0].FixSHA, input.Findings[0].ProofNote, input.Findings[0].ResolvedAt = strings.Repeat("f", 40), "Focused immutable proof", "2026-01-01T00:05:00Z"
	fixed, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed || fixed.Revision != decided.Revision+1 {
		t.Fatal("fix projection failed", err)
	}
	input.Finalized, input.FrozenCounts = true, &NativeCounts{Found: 2, Accepted: 1}
	finalized, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed || finalized.Revision != fixed.Revision+1 {
		t.Fatal("source finalization projection failed", err)
	}
	finalDigest := nativeSnapshotBytes(t, f, finalized.ID)
	var frozenDetail RunDetailResponse
	nativeRead(t, f, "/runs/"+runID+"?snapshot="+finalized.ID, &frozenDetail)
	decideNativeFinding(&input.Findings[1], "accepted")
	input.Findings[1].DecisionReason = "Correction preserves original source counts"
	corrected, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed || corrected.Revision != finalized.Revision+1 {
		t.Fatal("post-finalization correction failed", err)
	}
	nativeRead(t, f, "/runs/"+runID, &detail)
	if detail.Run.Accepted != 1 || detail.Run.Found != 2 || detail.Run.Routed != 0 || !detail.Run.Finalized || detail.Run.Fixed != 1 || detail.Findings[0].Disposition != "accepted" || detail.Findings[1].Disposition != "accepted" || detail.Findings[0].ProofNote != input.Findings[0].ProofNote {
		t.Fatal("frozen counts or effective decisions/proof lost")
	}
	// The same immutable source rows remain recognizable through every change.
	oldSummary, correctedSummary := oldNative.Run, detail.Run
	correctedSummary.Accepted, correctedSummary.Finalized, correctedSummary.Fixed = oldSummary.Accepted, oldSummary.Finalized, oldSummary.Fixed
	if !reflect.DeepEqual(correctedSummary, oldSummary) {
		t.Fatal("adjudication changed source capture identity, times, anchor or coverage")
	}
	for i, finding := range detail.Findings {
		old := oldNative.Findings[i]
		if finding.SourceFindingID != old.SourceFindingID || finding.Title != old.Title || finding.Detail != old.Detail || finding.Severity != old.Severity {
			t.Fatal("finding source bytes replaced")
		}
	}
	var queue QueueResponse
	nativeRead(t, f, "/queue", &queue)
	if queue.ObservedAt != initialQueue.ObservedAt || queue.Items[0].RecentRuns[0].Accepted != 1 || queue.Items[0].RecentRuns[0].Fixed != 1 || !reflect.DeepEqual(queue.Items[1], oldOtherPull) || queue.Items[1].State != "merged" {
		t.Fatal("queue summaries, merged history or freshness changed")
	}
	var pull PullResponse
	nativeRead(t, f, "/pulls/1", &pull)
	if pull.Pull.Title != oldPull.Pull.Title || pull.Pull.HeadSHA != strings.Repeat("9", 40) || pull.Pull.BaseSHA != strings.Repeat("8", 40) || pull.Pull.CIState != "pending" || pull.Pull.Mergeability != "conflicting" || pull.Pull.State != "open" || !reflect.DeepEqual(pull.Pull.LatestAttempts, oldPull.Pull.LatestAttempts) {
		t.Fatal("projection regressed current PR facts to captured source")
	}
	var imported RunDetailResponse
	nativeRead(t, f, "/runs/"+importedID, &imported)
	if !reflect.DeepEqual(imported.Run, oldImported.Run) || !reflect.DeepEqual(imported.Findings, oldImported.Findings) {
		t.Fatal("imported source with same external ID changed")
	}
	if nativeSnapshotBytes(t, f, previous.ID) != oldDigest || nativeSnapshotBytes(t, f, finalized.ID) != finalDigest || nativeSnapshotBytes(t, &other, otherReceipt.ID) != otherDigest {
		t.Fatal("old snapshot or other project changed")
	}
	var retained RunDetailResponse
	if !bytes.Equal(nativeRead(t, f, "/runs/"+importedID+"?snapshot="+previous.ID, &retained), oldImportedBytes) {
		t.Fatal("historical imported response bytes changed")
	}
	nativeRead(t, f, "/runs/"+runID+"?snapshot="+finalized.ID, &retained)
	if !reflect.DeepEqual(retained, frozenDetail) {
		t.Fatal("historical decisions were rewritten")
	}
	counts = nativeProjectionCounts(t, f)
	// Input order is not source identity and must not manufacture a new revision.
	input.Findings[0], input.Findings[1] = input.Findings[1], input.Findings[0]
	receipt, changed, err = applyNativeAdjudication(f, input)
	if err != nil || changed || receipt != corrected || !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) {
		t.Fatal("exact effective state cloned or emitted event", err)
	}
	if counts["events"] != 8 {
		t.Fatal("each real change must append exactly one observation event")
	}
}

func TestNativeAdjudicationRejectsInvalidIdentityCountsAndProofAtomically(t *testing.T) {
	f, valid, previous := nativeAdjudicationFixture(t)
	decideNativeFinding(&valid.Findings[0], "accepted")
	decideNativeFinding(&valid.Findings[1], "rejected")
	cases := map[string]func(*NativeAdjudication){
		"empty_source":          func(in *NativeAdjudication) { in.SourceRunID = "" },
		"source_control":        func(in *NativeAdjudication) { in.SourceRunID += "\x00" },
		"duplicate_id":          func(in *NativeAdjudication) { in.Findings[1].SourceFindingID = in.Findings[0].SourceFindingID },
		"unknown_id":            func(in *NativeAdjudication) { in.Findings[1].SourceFindingID = "foreign-finding" },
		"missing_id":            func(in *NativeAdjudication) { in.Findings = in.Findings[:1] },
		"finding_capacity":      func(in *NativeAdjudication) { in.Findings = make([]NativeFindingUpdate, 101) },
		"routed":                func(in *NativeAdjudication) { in.Findings[0].Disposition = "routed" },
		"reason_blank":          func(in *NativeAdjudication) { in.Findings[0].DecisionReason = " \n" },
		"reason_bytes":          func(in *NativeAdjudication) { in.Findings[0].DecisionReason = strings.Repeat("é", 1025) },
		"reason_control":        func(in *NativeAdjudication) { in.Findings[0].DecisionReason = "reason\u0085" },
		"actor_blank":           func(in *NativeAdjudication) { in.Findings[0].Actor = "" },
		"actor_control":         func(in *NativeAdjudication) { in.Findings[0].Actor = "actor\x1b" },
		"decision_time":         func(in *NativeAdjudication) { in.Findings[0].DecidedAt = "not-a-time" },
		"zero_time":             func(in *NativeAdjudication) { in.Findings[0].DecidedAt = "0001-01-01T00:00:00Z" },
		"sha_case":              func(in *NativeAdjudication) { in.Findings[0].FixSHA = strings.Repeat("A", 40) },
		"sha_short":             func(in *NativeAdjudication) { in.Findings[0].FixSHA = strings.Repeat("a", 39) },
		"proof_missing":         func(in *NativeAdjudication) { in.Findings[0].FixSHA = strings.Repeat("a", 40) },
		"proof_without_sha":     func(in *NativeAdjudication) { in.Findings[0].ProofNote = "proof" },
		"timestamp_without_sha": func(in *NativeAdjudication) { in.Findings[0].ResolvedAt = "2026-01-01T00:00:00Z" },
		"unexpected_counts":     func(in *NativeAdjudication) { in.FrozenCounts = &NativeCounts{Found: 2, Accepted: 1} },
		"missing_counts":        func(in *NativeAdjudication) { in.Finalized = true },
		"negative_counts": func(in *NativeAdjudication) {
			in.Finalized, in.FrozenCounts = true, &NativeCounts{Found: 2, Accepted: -1}
		},
		"found_count": func(in *NativeAdjudication) {
			in.Finalized, in.FrozenCounts = true, &NativeCounts{Found: 1, Accepted: 1}
		},
		"first_freeze_mismatch": func(in *NativeAdjudication) {
			in.Finalized, in.FrozenCounts = true, &NativeCounts{Found: 2, Accepted: 0}
		},
		"routed_counts": func(in *NativeAdjudication) {
			in.Finalized, in.FrozenCounts = true, &NativeCounts{Found: 2, Accepted: 1, Routed: 1}
		},
		"undecided_with_reason": func(in *NativeAdjudication) { in.Findings[0].Disposition = "unadjudicated" },
	}
	before, counts := nativeSnapshotBytes(t, f, previous.ID), nativeProjectionCounts(t, f)
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			input := valid
			input.Findings = append([]NativeFindingUpdate(nil), valid.Findings...)
			mutate(&input)
			if _, _, err := applyNativeAdjudication(f, input); err == nil {
				t.Fatal("invalid update accepted")
			}
			if nativeSnapshotBytes(t, f, previous.ID) != before || !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) {
				t.Fatal("invalid update partially published")
			}
		})
	}
}

func TestNativeAdjudicationFinalizationAndFixProofCannotBeErased(t *testing.T) {
	f, input, _ := nativeAdjudicationFixture(t)
	decideNativeFinding(&input.Findings[0], "accepted")
	decideNativeFinding(&input.Findings[1], "rejected")
	input.Findings[0].FixSHA, input.Findings[0].ProofNote, input.Findings[0].ResolvedAt = strings.Repeat("f", 40), "Fix proof", "2026-01-01T00:05:00Z"
	input.Finalized, input.FrozenCounts = true, &NativeCounts{Found: 2, Accepted: 1}
	previous, _, err := applyNativeAdjudication(f, input)
	if err != nil {
		t.Fatal(err)
	}
	counts, digest := nativeProjectionCounts(t, f), nativeSnapshotBytes(t, f, previous.ID)
	for _, mode := range []string{"unfinalize", "replace_counts", "reject_with_proof", "erase_then_reject", "erase_proof"} {
		t.Run(mode, func(t *testing.T) {
			next := input
			next.Findings = append([]NativeFindingUpdate(nil), input.Findings...)
			switch mode {
			case "unfinalize":
				next.Finalized, next.FrozenCounts = false, nil
			case "replace_counts":
				next.FrozenCounts = &NativeCounts{Found: 2, Accepted: 2}
			case "reject_with_proof":
				next.Findings[0].Disposition = "rejected"
			case "erase_then_reject":
				next.Findings[0].Disposition = "rejected"
				fallthrough
			case "erase_proof":
				next.Findings[0].FixSHA, next.Findings[0].ProofNote, next.Findings[0].ResolvedAt = "", "", ""
			}
			if _, _, err := applyNativeAdjudication(f, next); err == nil {
				t.Fatal("source counts or proof were erased")
			}
			if nativeSnapshotBytes(t, f, previous.ID) != digest || !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) {
				t.Fatal("conflict partially published")
			}
		})
	}
	// A guarded ledger correction can replace a proof value while keeping all
	// prior immutable snapshots and original finalized counts inspectable.
	input.Findings[0].FixSHA, input.Findings[0].ProofNote = strings.Repeat("e", 40), "Corrected proof"
	if _, changed, err := applyNativeAdjudication(f, input); err != nil || !changed {
		t.Fatal("valid proof correction rejected", err)
	}
}

func TestNativeAdjudicationZeroFindingsAndIncompleteFinalization(t *testing.T) {
	for _, mode := range []string{"zero", "partial", "reported_count_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			f := newAPIFixture(t)
			run := nativeUndecidedRun("empty-run", 0)
			if mode == "partial" {
				run.Outcome, run.Complete = "failed", false
			}
			if mode == "reported_count_mismatch" {
				run.Found, run.Complete = 1, false
			}
			previous := mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
			input := nativeAdjudicationInput(run)
			input.Finalized, input.FrozenCounts = true, &NativeCounts{}
			receipt, changed, err := applyNativeAdjudication(f, input)
			if mode == "zero" {
				if err != nil || !changed || receipt.Revision != 2 {
					t.Fatal("legitimate zero source finalization failed", err)
				}
				var detail RunDetailResponse
				nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, run.SourceRunID), &detail)
				if !detail.Run.Finalized || detail.Run.Found != 0 || detail.Run.Accepted != 0 || len(detail.Findings) != 0 {
					t.Fatal("zero finalization fabricated findings")
				}
			} else if err == nil || countRows(t, f.store, "review_snapshots") != 1 || receipt.ID != "" || previous.Revision != 1 {
				t.Fatal("incomplete source acquired finalized credit")
			}
		})
	}
}

func TestNativeAdjudicationCapacityCallerFailureAndExactMutationReplay(t *testing.T) {
	for _, mode := range []string{"capacity", "caller_failure", "exact_replay"} {
		t.Run(mode, func(t *testing.T) {
			f, input, previous := nativeAdjudicationFixture(t)
			if _, err := f.store.DB().Exec(`CREATE TABLE adjudication_test_ledger(note TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if mode == "capacity" {
				// Model a retained snapshot that exceeds the current capacity after
				// an operator/import migration. The correction must refuse atomically.
				historyFixtureOverflowAttempts(t, f, previous, 2, 101)
			}
			decideNativeFinding(&input.Findings[0], "accepted")
			before, counts := nativeSnapshotBytes(t, f, previous.ID), nativeProjectionCounts(t, f)
			r := httptest.NewRequest("POST", "/", nil)
			r.SetPathValue("project_id", f.project)
			r.Header.Set("Authorization", "Bearer "+f.admin)
			scope, err := f.store.AdminScope(r)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			callback := func(ctx context.Context, tx *sql.Tx) (any, error) {
				calls++
				if _, err := tx.ExecContext(ctx, `INSERT INTO adjudication_test_ledger VALUES('new decision')`); err != nil {
					return nil, err
				}
				receipt, changed, err := UpdateNativeAdjudication(ctx, tx, scope, input)
				if err != nil {
					return nil, err
				}
				if !changed {
					return nil, fmt.Errorf("fixture update did not change projection")
				}
				if mode == "caller_failure" {
					return nil, core.Fail("test_failure", "Synthetic caller rollback.", 409)
				}
				return receipt, nil
			}
			mutation := core.Mutation{Operation: "test.native-adjudication", Target: input.SourceRunID, Key: "exact-transaction-key", Request: input}
			first, err := f.store.Mutate(context.Background(), scope, mutation, callback)
			if mode == "exact_replay" {
				if err != nil {
					t.Fatal(err)
				}
				replay, err := f.store.Mutate(context.Background(), scope, mutation, callback)
				if err != nil || !bytes.Equal(first, replay) || calls != 1 || countRows(t, f.store, "adjudication_test_ledger") != 1 || countRows(t, f.store, "events") != counts["events"]+1 || countRows(t, f.store, "review_snapshots") != counts["review_snapshots"]+1 {
					t.Fatal("same-key caller retry republished projection", err)
				}
				var receipt Receipt
				if json.Unmarshal(first, &receipt) != nil || receipt.Revision != previous.Revision+1 {
					t.Fatal("compact transaction receipt changed")
				}
			} else {
				code := "workspace_capacity"
				if mode == "caller_failure" {
					code = "test_failure"
				}
				nativeErrorCode(t, err, code)
				if nativeSnapshotBytes(t, f, previous.ID) != before || !reflect.DeepEqual(counts, nativeProjectionCounts(t, f)) || countRows(t, f.store, "adjudication_test_ledger") != 0 {
					t.Fatal("projection failure did not roll back caller ledger and snapshot")
				}
			}
		})
	}
}

// Model already-retained data above today's request capacity without bypassing
// exact version/membership/current references in the installed physical schema.
func historyFixtureOverflowAttempts(t *testing.T, f *apiFixture, snapshot Receipt, number int64, count int) {
	t.Helper()
	tx := historyWriterTx(t, f)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("capacity-%d", i)
		if _, err := tx.Exec(`INSERT INTO review_attempt_versions VALUES(?,'test-import',?,?,?,'cli','','unknown','','',NULL,'','','')`, f.project, id, snapshot.Revision, number); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO review_attempt_memberships VALUES(?,'test-import',?,?,NULL,?,?,?)`, f.project, id, snapshot.Revision, snapshot.Revision, number, i); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO review_current_attempts VALUES(?,'test-import',?,?,?,?)`, f.project, id, snapshot.Revision, number, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}
