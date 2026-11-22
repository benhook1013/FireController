package reviews

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

func applyNativeAdjudicationCorrection(f *apiFixture, input NativeAdjudication, permit NativeFixCorrection) (Receipt, bool, error) {
	var receipt Receipt
	var changed bool
	err := f.store.BackgroundWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		receipt, changed, err = UpdateNativeAdjudicationCorrection(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "native-adjudicator"}, input, permit)
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
	for _, state := range []string{"unconfigured", "imported-only", "other-project-only", "historical-native-only", "different-current-native-run"} {
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
			case "different-current-native-run":
				run := nativeUndecidedRun("different-native-run", 0)
				mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
			}
			counts := nativeProjectionCounts(t, f)
			receipt, changed, err := applyNativeAdjudication(f, input)
			if err != nil || changed || receipt != (Receipt{}) || !reflect.DeepEqual(nativeProjectionCounts(t, f), counts) {
				t.Fatal("missing current native projection produced effects", receipt, changed, err)
			}
			correction := input
			correction.Finalized, correction.FrozenCounts = true, &NativeCounts{}
			receipt, changed, err = applyNativeAdjudicationCorrection(f, correction, NativeFixCorrection{SourceFindingID: "finding-0", PreviousSHA: strings.Repeat("f", 40), PreviousNote: "Previous proof", PreviousResolvedAt: "2026-01-01T00:05:00Z"})
			if err != nil || changed || receipt != (Receipt{}) || !reflect.DeepEqual(nativeProjectionCounts(t, f), counts) {
				t.Fatal("missing current native projection produced correction effects", receipt, changed, err)
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

func rejectNativeFindingWithProof(finding *NativeFindingUpdate) {
	decideNativeFinding(finding, "rejected")
	finding.DecisionReason = "The prior fix was based on a mistaken attribution"
	finding.FixSHA, finding.ProofNote, finding.ResolvedAt = "", "", ""
	finding.Route, finding.OperationalProof = nil, nil
}

func TestNativeAdjudicationCorrectionRetiresOnlyPinnedProofAndKeepsHistory(t *testing.T) {
	f := newAPIFixture(t)
	run := nativeUndecidedRun("native-proof-correction", 4)
	mustNativePublish(t, f, NativePublication{Pull: nativePull(1), Run: &run})
	input := nativeAdjudicationInput(run)
	for i := range input.Findings[:3] {
		decideNativeFinding(&input.Findings[i], "accepted")
	}
	for i := range input.Findings[:2] {
		finding := &input.Findings[i]
		finding.FixSHA, finding.ProofNote, finding.ResolvedAt = strings.Repeat("f", 40), "Previous proof\nexact bytes", "2026-01-01T00:05:00Z"
	}
	note := "Operational settings verification"
	digest := sha256.Sum256([]byte(note))
	input.Findings[2].OperationalProof = &OperationalProof{Digest: hex.EncodeToString(digest[:]), Note: note, ActorID: "operator", RecordedAt: "2026-01-01T00:05:00Z"}
	decideNativeFinding(&input.Findings[3], "routed")
	route := nativeRouteFixture(3)
	route.Disposition, route.Reason, route.Actor, route.DecidedAt = "accepted", "Receiving decision", "receiver", "2026-01-01T00:05:00Z"
	route.FixSHA, route.ProofNote, route.FixedAt = strings.Repeat("c", 40), "Receiving proof", "2026-01-01T00:06:00Z"
	input.Findings[3].Route = route
	unfinalized, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed {
		t.Fatal("setup unfinalized proof", unfinalized, changed, err)
	}
	input.Finalized, input.FrozenCounts = true, &NativeCounts{Found: 4, Accepted: 3, Routed: 1}
	next := input
	next.Findings = append([]NativeFindingUpdate(nil), input.Findings...)
	before := input.Findings[0]
	permit := NativeFixCorrection{SourceFindingID: before.SourceFindingID, PreviousSHA: before.FixSHA, PreviousNote: before.ProofNote, PreviousResolvedAt: before.ResolvedAt}
	rejectNativeFindingWithProof(&next.Findings[0])

	checkConflict := func(t *testing.T, candidate NativeAdjudication, permit NativeFixCorrection) {
		t.Helper()
		state := importDatabaseState(t, f)
		receipt, changed, err := applyNativeAdjudicationCorrection(f, candidate, permit)
		nativeErrorCode(t, err, "observation_conflict")
		if changed || receipt != (Receipt{}) || !reflect.DeepEqual(state, importDatabaseState(t, f)) {
			t.Fatal("refused correction changed projection state", receipt, changed)
		}
	}
	// The permit is not authority to finalize and retire proof in one step.
	checkConflict(t, next, permit)
	previous, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed {
		t.Fatal("setup finalization", previous, changed, err)
	}
	runPath := "/runs/" + runOpaqueID(f.project, nativeSource, run.SourceRunID)
	var oldDetail RunDetailResponse
	oldBytes := nativeRead(t, f, runPath+"?snapshot="+previous.ID, &oldDetail)
	oldSnapshot := nativeSnapshotBytes(t, f, previous.ID)
	for _, tc := range []struct {
		name   string
		mutate func(*NativeAdjudication, *NativeFixCorrection)
	}{
		{"wrong-finding", func(_ *NativeAdjudication, p *NativeFixCorrection) { p.SourceFindingID = "missing-finding" }},
		{"wrong-sha", func(_ *NativeAdjudication, p *NativeFixCorrection) { p.PreviousSHA = strings.Repeat("e", 40) }},
		{"wrong-note", func(_ *NativeAdjudication, p *NativeFixCorrection) { p.PreviousNote += " " }},
		{"wrong-resolved-at", func(_ *NativeAdjudication, p *NativeFixCorrection) { p.PreviousResolvedAt = "2026-01-01T00:05:00.0Z" }},
		{"unused-permit", func(in *NativeAdjudication, _ *NativeFixCorrection) { in.Findings[0] = before }},
		{"unfinalize", func(in *NativeAdjudication, _ *NativeFixCorrection) { in.Finalized, in.FrozenCounts = false, nil }},
		{"change-frozen-counts", func(in *NativeAdjudication, _ *NativeFixCorrection) {
			in.FrozenCounts = &NativeCounts{Found: 4, Accepted: 2, Routed: 1}
		}},
		{"erase-without-rejection", func(in *NativeAdjudication, _ *NativeFixCorrection) { in.Findings[0].Disposition = "accepted" }},
		{"retire-second-commit-proof", func(in *NativeAdjudication, _ *NativeFixCorrection) { rejectNativeFindingWithProof(&in.Findings[1]) }},
		{"change-unselected-finding", func(in *NativeAdjudication, _ *NativeFixCorrection) { in.Findings[1].DecisionReason += " (changed)" }},
		{"operational-proof-is-not-commit-proof", func(in *NativeAdjudication, p *NativeFixCorrection) {
			in.Findings[0] = before
			*p = NativeFixCorrection{SourceFindingID: in.Findings[2].SourceFindingID}
			rejectNativeFindingWithProof(&in.Findings[2])
		}},
		{"receiving-proof-is-not-source-proof", func(in *NativeAdjudication, p *NativeFixCorrection) {
			in.Findings[0] = before
			*p = NativeFixCorrection{SourceFindingID: in.Findings[3].SourceFindingID, PreviousSHA: route.FixSHA, PreviousNote: route.ProofNote, PreviousResolvedAt: route.FixedAt}
			rejectNativeFindingWithProof(&in.Findings[3])
		}},
		{"erase-receiving-proof-on-unselected-finding", func(in *NativeAdjudication, _ *NativeFixCorrection) { rejectNativeFindingWithProof(&in.Findings[3]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, candidatePermit := next, permit
			candidate.Findings = append([]NativeFindingUpdate(nil), next.Findings...)
			tc.mutate(&candidate, &candidatePermit)
			checkConflict(t, candidate, candidatePermit)
		})
	}
	if _, changed, err = applyNativeAdjudication(f, next); changed {
		t.Fatal("ordinary path retired proof")
	}
	nativeErrorCode(t, err, "observation_conflict")
	t.Run("caller-rollback", func(t *testing.T) {
		state := importDatabaseState(t, f)
		scope := core.Scope{ProjectID: f.project, PrincipalID: "native-adjudicator"}
		err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
			if _, changed, err := UpdateNativeAdjudicationCorrection(ctx, tx, scope, next, permit); err != nil || !changed {
				return fmt.Errorf("correction did not reach caller rollback: changed=%t err=%v", changed, err)
			}
			return core.Fail("test_rollback", "Synthetic caller rollback.", 409)
		})
		nativeErrorCode(t, err, "test_rollback")
		if !reflect.DeepEqual(state, importDatabaseState(t, f)) {
			t.Fatal("caller rollback left projection effects")
		}
	})
	counts := nativeProjectionCounts(t, f)
	corrected, changed, err := applyNativeAdjudicationCorrection(f, next, permit)
	if err != nil || !changed || corrected.Revision != previous.Revision+1 {
		t.Fatal("matching permit failed to project correction", corrected, changed, err)
	}
	var detail RunDetailResponse
	nativeRead(t, f, runPath, &detail)
	expectedRun := oldDetail.Run
	expectedRun.Fixed--
	if !reflect.DeepEqual(detail.Run, expectedRun) || !reflect.DeepEqual(detail.Findings[1:], oldDetail.Findings[1:]) {
		t.Fatal("run facts, original counts, or other findings changed")
	}
	expectedFinding := oldDetail.Findings[0]
	expectedFinding.Disposition, expectedFinding.DecisionReason = "rejected", next.Findings[0].DecisionReason
	expectedFinding.FixSHA, expectedFinding.ProofNote, expectedFinding.ResolvedAt = "", "", ""
	if !reflect.DeepEqual(detail.Findings[0], expectedFinding) {
		t.Fatal("target finding did not change only its decision and effective proof", detail.Findings[0])
	}
	if !bytes.Equal(nativeRead(t, f, runPath+"?snapshot="+previous.ID, &detail), oldBytes) || nativeSnapshotBytes(t, f, previous.ID) != oldSnapshot {
		t.Fatal("correction rewrote the previous snapshot")
	}
	after := nativeProjectionCounts(t, f)
	if after["review_snapshots"] != counts["review_snapshots"]+1 || after["events"] != counts["events"]+1 || after["receipts"] != counts["receipts"] {
		t.Fatal("correction did not append exactly one projection snapshot and event")
	}
	var storedDigest string
	if err := f.store.DB().QueryRow(`SELECT payload_sha256 FROM review_snapshots WHERE project_id=? AND snapshot_id=?`, f.project, corrected.ID).Scan(&storedDigest); err != nil {
		t.Fatal(err)
	}
	effectiveDigest := sha256.Sum256([]byte(previous.ID + "\x00" + mustJSON(next)))
	if storedDigest != hex.EncodeToString(effectiveDigest[:]) {
		t.Fatal("snapshot digest did not use just the effective adjudication input")
	}
	checkConflict(t, next, permit) // A spent permit is not a projection-level replay key.
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

func TestNativeAdjudicationGrowthCallerFailureAndExactMutationReplay(t *testing.T) {
	for _, mode := range []string{"caller_failure", "event_failure", "exact_replay"} {
		t.Run(mode, func(t *testing.T) {
			f, input, original := nativeAdjudicationFixture(t)
			oldBytes := nativeSnapshotBytes(t, f, original.ID)
			var previous Receipt
			for batch := 0; batch < 3; batch++ {
				previous = nativeGrowthTransferBatch(t, f, nativePull(2), "retained-before-adjudication", batch*75, 75, 0)
			}
			if _, err := f.store.DB().Exec(`CREATE TABLE adjudication_test_ledger(note TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if mode == "event_failure" {
				if _, err := f.store.DB().Exec(`CREATE TRIGGER adjudication_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'synthetic adjudication event failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			decideNativeFinding(&input.Findings[0], "accepted")
			before, counts := importDatabaseState(t, f), nativeProjectionCounts(t, f)
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
				after := importDatabaseState(t, f)
				replay, err := f.store.Mutate(context.Background(), scope, mutation, callback)
				if err != nil || !bytes.Equal(first, replay) || calls != 1 || countRows(t, f.store, "adjudication_test_ledger") != 1 || countRows(t, f.store, "events") != counts["events"]+1 || countRows(t, f.store, "review_snapshots") != counts["review_snapshots"]+1 || !reflect.DeepEqual(after, importDatabaseState(t, f)) {
					t.Fatal("same-key retry republished native adjudication above retained totals", err)
				}
				var receipt Receipt
				if json.Unmarshal(first, &receipt) != nil || receipt.Revision != previous.Revision+1 {
					t.Fatal("compact transaction receipt changed")
				}
				var current RunDetailResponse
				nativeRead(t, f, "/runs/"+runOpaqueID(f.project, nativeSource, input.SourceRunID), &current)
				if current.Run.Found != 2 || current.Run.Accepted != 1 || current.Run.Routed != 0 || current.Run.Source != nativeSource || current.Findings[0].Detail != "IMMUTABLE_NATIVE_SOURCE_0" {
					t.Fatal("source facts changed during growth adjudication")
				}
			} else {
				code := "test_failure"
				if mode == "event_failure" {
					code = "storage_unavailable"
				}
				nativeErrorCode(t, err, code)
				if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
					t.Fatal("failed adjudication changed caller ledger, indexes or retained source history")
				}
			}
			if nativeSnapshotBytes(t, f, original.ID) != oldBytes {
				t.Fatal("growth adjudication rewrote old snapshot bytes")
			}
			if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
				t.Fatal(err)
			}
			if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeOperationalProofProjectionAndRetention(t *testing.T) {
	f, input, _ := nativeAdjudicationFixture(t)
	decideNativeFinding(&input.Findings[0], "accepted")
	decideNativeFinding(&input.Findings[1], "rejected")
	note := "Verified external settings change without a code commit."
	hash := sha256.Sum256([]byte(note))
	p := &OperationalProof{Digest: hex.EncodeToString(hash[:]), Note: note, ActorID: "authenticated-operator", RecordedAt: "2026-01-01T00:05:00Z"}
	input.Findings[0].OperationalProof = p
	first, changed, err := applyNativeAdjudication(f, input)
	if err != nil || !changed {
		t.Fatal("operational proof was not recorded", err)
	}
	var runs RunsResponse
	nativeRead(t, f, "/pulls/1/runs", &runs)
	if len(runs.Items) != 2 || runs.Items[0].Source != nativeSource || runs.Items[0].Fixed != 1 {
		t.Fatal("operational proof was not counted as fixed", runs)
	}
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+runs.Items[0].ID+"?snapshot="+first.ID, &detail)
	if !reflect.DeepEqual(detail.Findings[0].OperationalProof, p) || detail.Findings[0].FixSHA != "" {
		t.Fatal("native projection lost proof or invented SHA", detail.Findings[0])
	}
	input.Findings[0].OperationalProof = nil
	if _, changed, err := applyNativeAdjudication(f, input); err == nil || changed {
		t.Fatal("existing operational proof was removed")
	}
	input.Findings[0].OperationalProof = p
	input.Findings[0].FixSHA = strings.Repeat("f", 40)
	if _, changed, err := applyNativeAdjudication(f, input); err == nil || changed {
		t.Fatal("combined code and operational proof accepted")
	}
	input.Findings[0].FixSHA = ""
	p.Digest = strings.Repeat("0", 64)
	if _, changed, err := applyNativeAdjudication(f, input); err == nil || changed {
		t.Fatal("invalid operational proof digest accepted")
	}
}
