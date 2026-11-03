package reviews

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestHistoryRunDetailsVersionTogetherWithoutCopyingUnrelatedRecords(t *testing.T) {
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	run := RunInput{SourceRunID: "external-run", Channel: "cli", HeadSHA: "head", Scope: "full", Outcome: "completed", Complete: true, Attributable: true, Found: 1, Findings: []Finding{{SourceFindingID: "finding-1", Title: "Original title", Detail: "Original body", Disposition: "unadjudicated"}}}
	first := historyWriterSnapshot(t, tx, f.project, 1)
	one := newHistorySnapshotWrite(f.project, 1)
	if err := one.pull(t.Context(), tx, 7, 0, []any{"Owning PR", "open", "head", "main", "base", "unknown", "unknown", false, 0, 0, "[]", "[]"}); err != nil {
		t.Fatal(err)
	}
	if err := one.run(t.Context(), tx, "imported", 7, 0, run); err != nil {
		t.Fatal(err)
	}
	if err := one.applyCurrent(t.Context(), tx, first, 0); err != nil {
		t.Fatal(err)
	}
	second := historyWriterSnapshot(t, tx, f.project, 2)
	two := newHistorySnapshotWrite(f.project, 2)
	if err := two.run(t.Context(), tx, "imported", 7, 1, run); err != nil {
		t.Fatal(err)
	}
	if err := two.attempt(t.Context(), tx, "native-executor", 7, 0, Attempt{SourceAttemptID: "attempt-1", Channel: "cli", HeadSHA: "head", State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := two.applyCurrent(t.Context(), tx, second, 1); err != nil {
		t.Fatal(err)
	}
	third := historyWriterSnapshot(t, tx, f.project, 3)
	three := newHistorySnapshotWrite(f.project, 3)
	// Changing decision text without changing summary counters must still
	// append an exact run/detail version, leaving the first snapshot intact.
	run.Findings[0].DecisionReason = "New adjudication note"
	if err := three.run(t.Context(), tx, "imported", 7, 1, run); err != nil {
		t.Fatal(err)
	}
	if err := three.applyCurrent(t.Context(), tx, third, 2); err != nil {
		t.Fatal(err)
	}
	for table, want := range map[string]int{"review_run_versions": 2, "review_finding_versions": 2, "review_attempt_versions": 1} {
		var count int
		if err := tx.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s: %d want%d: %v", table, count, want, err)
		}
	}
	var original, corrected string
	if err := tx.QueryRowContext(t.Context(), "SELECT decision_reason FROM review_finding_versions WHERE run_version_revision=1").Scan(&original); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRowContext(t.Context(), "SELECT decision_reason FROM review_finding_versions WHERE run_version_revision=3").Scan(&corrected); err != nil {
		t.Fatal(err)
	}
	if original != "" || corrected != run.Findings[0].DecisionReason {
		t.Fatal("new decision overwrote retained detail")
	}
	if err := ValidateHistoryReferences(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryRecordIdentitiesSurviveOmissionAndCannotBeRebound(t *testing.T) {
	f := newHistoryWriterFixture(t)
	tx := historyWriterTx(t, f)
	run := RunInput{SourceRunID: "same-id", Channel: "hosted", HeadSHA: "old-head", Scope: "full", Findings: []Finding{}}
	attempt := Attempt{SourceAttemptID: "same-id", Channel: "cli", HeadSHA: "old-head", State: "completed", TriggerID: "trigger-1"}
	first := historyWriterSnapshot(t, tx, f.project, 1)
	one := newHistorySnapshotWrite(f.project, 1)
	if err := one.pull(t.Context(), tx, 7, 0, []any{"Owning PR", "open", "head", "main", "base", "unknown", "unknown", false, 0, 0, "[]", "[]"}); err != nil {
		t.Fatal(err)
	}
	if err := one.run(t.Context(), tx, "first-producer", 7, 0, run); err != nil {
		t.Fatal(err)
	}
	if err := one.attempt(t.Context(), tx, "first-producer", 7, 0, attempt); err != nil {
		t.Fatal(err)
	}
	// Another original producer legitimately has the same external identities.
	if err := one.run(t.Context(), tx, "second-producer", 7, 1, run); err != nil {
		t.Fatal(err)
	}
	if err := one.attempt(t.Context(), tx, "second-producer", 7, 1, attempt); err != nil {
		t.Fatal(err)
	}
	if err := one.applyCurrent(t.Context(), tx, first, 0); err != nil {
		t.Fatal(err)
	}
	second := historyWriterSnapshot(t, tx, f.project, 2)
	two := newHistorySnapshotWrite(f.project, 2)
	id := runOpaqueID(f.project, "first-producer", run.SourceRunID)
	if err := two.remove(t.Context(), tx, historyRunRecord, []any{id}); err != nil {
		t.Fatal(err)
	}
	if err := two.remove(t.Context(), tx, historyAttemptRecord, []any{"first-producer", attempt.SourceAttemptID}); err != nil {
		t.Fatal(err)
	}
	if err := two.applyCurrent(t.Context(), tx, second, 1); err != nil {
		t.Fatal(err)
	}
	_ = historyWriterSnapshot(t, tx, f.project, 3)
	three := newHistorySnapshotWrite(f.project, 3)
	nativeErrorCode(t, three.run(t.Context(), tx, "first-producer", 8, 0, run), "observation_conflict")
	moved := run
	moved.HeadSHA = "new-head"
	nativeErrorCode(t, three.run(t.Context(), tx, "first-producer", 7, 0, moved), "observation_conflict")
	nativeErrorCode(t, three.attempt(t.Context(), tx, "first-producer", 8, 0, attempt), "observation_conflict")
	reopened := attempt
	reopened.State = "running"
	nativeErrorCode(t, three.attempt(t.Context(), tx, "first-producer", 7, 0, reopened), "observation_conflict")
	erased := attempt
	erased.TriggerID = ""
	nativeErrorCode(t, three.attempt(t.Context(), tx, "first-producer", 7, 0, erased), "observation_conflict")
	if err := three.run(t.Context(), tx, "first-producer", 7, 0, run); err != nil {
		t.Fatal("returning same source should reuse old body", err)
	}
}

func TestHistoryAttemptUnknownIdentityCanFillOnceThroughObservationAPI(t *testing.T) {
	for _, firstField := range []string{"head", "start"} {
		t.Run(firstField+"-first", func(t *testing.T) {
			f := historyObservationFixture(t)
			base := "/api/v1/projects/" + f.project + "/reviews"
			request := func(revision int64, attempt *Attempt) ObservationRequest {
				pull := samplePull(8, "Observed lifecycle")
				pull.State, pull.Complete = "open", false
				if attempt != nil {
					pull.Attempts = []Attempt{*attempt}
				}
				req := reviewRequest(revision, []PullInput{pull, samplePull(9, "Retained merged PR")})
				req.ObservedAt = "2026-01-01T00:00:00Z"
				return req
			}
			attempt := Attempt{SourceAttemptID: "observed-attempt", Channel: "manual", State: "prepared", TriggerID: "known-trigger", ProviderReviewID: "known-provider-review"}
			initial := request(0, &attempt)
			first := f.call(http.MethodPost, base+"/observations", f.admin, "unknown-identity-initial", initial)
			var receipt Receipt
			if first.Code != http.StatusCreated || json.Unmarshal(first.Body.Bytes(), &receipt) != nil {
				t.Fatal("valid unknown initial attempt was refused", first.Code, first.Body.String())
			}
			var page QueueResponse
			nativeRead(t, f, "/queue?limit=1", &page)
			if page.Cursor == "" {
				t.Fatal("fixture lacks a pinned historical continuation")
			}
			cursorPath := base + "/queue?limit=1&cursor=" + url.QueryEscape(page.Cursor)
			cursorResponse := f.call(http.MethodGet, cursorPath, f.readToken, "", nil)
			if cursorResponse.Code != http.StatusOK {
				t.Fatal("initial historical continuation failed", cursorResponse.Body.String())
			}
			oldCursor := cursorResponse.Body.String()
			archivePath := base + "/pulls/8/attempts?snapshot=" + receipt.ID
			archiveResponse := f.call(http.MethodGet, archivePath, f.readToken, "", nil)
			var initialArchive AttemptsResponse
			if archiveResponse.Code != http.StatusOK || json.Unmarshal(archiveResponse.Body.Bytes(), &initialArchive) != nil || len(initialArchive.Items) != 1 || initialArchive.Items[0].HeadSHA != "" || initialArchive.Items[0].StartedAt != "" {
				t.Fatal("initial snapshot did not retain unknown facts", archiveResponse.Body.String())
			}
			oldArchive := archiveResponse.Body.String()
			assertCurrent := func(want Attempt) {
				t.Helper()
				var archive AttemptsResponse
				nativeRead(t, f, "/pulls/8/attempts", &archive)
				if len(archive.Items) != 1 || archive.Items[0].SourceAttemptID != want.SourceAttemptID || archive.Items[0].HeadSHA != want.HeadSHA || archive.Items[0].StartedAt != want.StartedAt || archive.Items[0].State != want.State {
					t.Fatal("enrichment changed identity or duplicated an attempt", archive.Items)
				}
				var pull PullResponse
				nativeRead(t, f, "/pulls/8", &pull)
				if pull.Pull.AttemptCount != 1 || pull.Pull.RunCount != 0 || pull.Pull.Complete || len(pull.Pull.LatestAttempts) != 1 || pull.Pull.LatestAttempts[0].HeadSHA != want.HeadSHA || pull.Pull.LatestAttempts[0].StartedAt != want.StartedAt {
					t.Fatal("enrichment lost summary fields or fabricated coverage", pull.Pull)
				}
			}
			assertCurrent(attempt)
			attempt.State = "preparing"
			if firstField == "head" {
				attempt.HeadSHA = strings.Repeat("a", 40)
			} else {
				attempt.StartedAt = "2026-01-01T00:00:00Z"
			}
			receipt = historyObservationPost(t, f, request(receipt.Revision, &attempt), "fill-first-unknown-field")
			assertCurrent(attempt)
			attempt.HeadSHA, attempt.StartedAt, attempt.State = strings.Repeat("a", 40), "2026-01-01T00:00:00Z", "active"
			receipt = historyObservationPost(t, f, request(receipt.Revision, &attempt), "fill-second-unknown-field")
			assertCurrent(attempt)
			rejectKnownChanges := func(revision int64) {
				t.Helper()
				for name, change := range map[string]func(*Attempt){
					"clear-head":       func(a *Attempt) { a.HeadSHA = "" },
					"change-head":      func(a *Attempt) { a.HeadSHA = strings.Repeat("b", 40) },
					"clear-start":      func(a *Attempt) { a.StartedAt = "" },
					"change-start":     func(a *Attempt) { a.StartedAt = "2026-01-01T00:00:01Z" },
					"change-channel":   func(a *Attempt) { a.Channel = "cli" },
					"clear-trigger":    func(a *Attempt) { a.TriggerID = "" },
					"replace-provider": func(a *Attempt) { a.ProviderReviewID = "other-provider-review" },
				} {
					candidate := attempt
					change(&candidate)
					before := historyViewsDigest(t, f)
					response := f.call(http.MethodPost, base+"/observations", f.admin, fmt.Sprintf("reject-%d-%s", revision, name), request(revision, &candidate))
					if response.Code != http.StatusConflict || before != historyViewsDigest(t, f) {
						t.Fatal("known identity mutation changed state", name, response.Code, response.Body.String())
					}
				}
			}
			rejectKnownChanges(receipt.Revision)
			// Omission cannot erase a previously learned identity.
			receipt = historyObservationPost(t, f, request(receipt.Revision, nil), "omit-observed-attempt")
			rejectKnownChanges(receipt.Revision)
			receipt = historyObservationPost(t, f, request(receipt.Revision, &attempt), "restore-same-observed-attempt")
			assertCurrent(attempt)
			seconds := int64(60)
			attempt.State, attempt.FinishedAt, attempt.DurationSeconds = "completed", "2026-01-01T00:01:00Z", &seconds
			receipt = historyObservationPost(t, f, request(receipt.Revision, &attempt), "complete-observed-attempt")
			assertCurrent(attempt)
			before := historyViewsDigest(t, f)
			reopened := attempt
			reopened.State = "active"
			if response := f.call(http.MethodPost, base+"/observations", f.admin, "reject-settled-reopen", request(receipt.Revision, &reopened)); response.Code != http.StatusConflict || historyViewsDigest(t, f) != before {
				t.Fatal("once-fill reopened a settled attempt", response.Code, response.Body.String())
			}
			replay := f.call(http.MethodPost, base+"/observations", f.admin, "unknown-identity-initial", initial)
			if replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() || historyViewsDigest(t, f) != before {
				t.Fatal("initial replay rewrote learned identity", replay.Code, replay.Body.String())
			}
			if got := f.call(http.MethodGet, cursorPath, f.readToken, "", nil); got.Code != http.StatusOK || got.Body.String() != oldCursor {
				t.Fatal("historical continuation changed")
			}
			if got := f.call(http.MethodGet, archivePath, f.readToken, "", nil); got.Code != http.StatusOK || got.Body.String() != oldArchive {
				t.Fatal("original unknown snapshot changed")
			}
			historyObservationCounts(t, f, map[string]int{"review_attempt_versions": 4, "review_run_versions": 0, "review_finding_versions": 0})
			for _, validate := range []func(context.Context, HistoryQuery) error{ValidateHistoryReferences, ValidateCurrentSummaries, ValidateCurrentRunOrder} {
				if err := validate(t.Context(), f.store.DB()); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
