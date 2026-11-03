package reviews

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
)

func sourcePins(t *testing.T, f *apiFixture, numbers ...int64) map[int64]SourceMetadataPin {
	t.Helper()
	var pins map[int64]SourceMetadataPin
	if err := f.store.BackgroundWrite(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		pins, err = PinSourceMetadata(ctx, tx, f.project, numbers)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return pins
}
func sourcePublish(f *apiFixture, pins map[int64]SourceMetadataPin, pulls ...PullInput) (Receipt, error) {
	var receipt Receipt
	err := f.store.BackgroundWrite(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		var err error
		receipt, err = PublishSourceMetadata(ctx, tx, core.Scope{ProjectID: f.project, PrincipalID: "source-fixture"}, 701, pins, pulls)
		return err
	})
	return receipt, err
}
func sourcePull(number int64, title string) PullInput {
	p := nativePull(number)
	p.Title = title
	p.State = "open"
	p.Complete = false
	p.CIState = ""
	p.Mergeability = ""
	return p
}

func TestSourceMetadataPreservesConcurrentEvidenceAndPinnedHistory(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	p.State = "open"
	p.Runs = []RunInput{nativeRun("imported-source-run")}
	old := f.observe(t, reviewRequest(0, []PullInput{p, nativePull(2)}), "source-start")["id"].(string)
	oldBytes := nativeSnapshotBytes(t, f, old)
	pins := sourcePins(t, f, 1, 2, 999)
	a := nativeAttempt("concurrent-attempt", "prepared")
	currentMetadata := p
	currentMetadata.Runs = nil
	mustNativePublish(t, f, NativePublication{Pull: currentMetadata, Attempt: &a})
	concurrent := metadataRead(t, f, 1, "").Pull
	updated := sourcePull(1, "Fresh provider title")
	updated.Diff = &PullDiff{7, 30, 2}
	draft := false
	updated.IsDraft = &draft
	receipt, err := sourcePublish(f, pins, updated)
	if err != nil || receipt.ID == "" {
		t.Fatal(receipt, err)
	}
	got := metadataRead(t, f, 1, "").Pull
	if got.Title != updated.Title || !reflect.DeepEqual(got.Diff, updated.Diff) || got.IsDraft == nil || *got.IsDraft || got.RunCount != concurrent.RunCount || got.AttemptCount != concurrent.AttemptCount || !reflect.DeepEqual(got.RecentRuns, concurrent.RecentRuns) || !reflect.DeepEqual(got.LatestAttempts, concurrent.LatestAttempts) {
		t.Fatalf("fresh facts or concurrent evidence lost: %+v", got)
	}
	if nativeSnapshotBytes(t, f, old) != oldBytes {
		t.Fatal("historical snapshot changed")
	}
	if _, ok := pins[999]; ok {
		t.Fatal("missing row pinned")
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestSourceMetadataNoopAndConcurrentEditWins(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	p.State = "open"
	f.observe(t, reviewRequest(0, []PullInput{p}), "source-start")
	pins := sourcePins(t, f, 1)
	same := sourcePull(1, p.Title)
	before := historyTableCounts(t, f)
	if r, err := sourcePublish(f, pins, same); err != nil || r.ID != "" {
		t.Fatal(r, err)
	}
	if !reflect.DeepEqual(before, historyTableCounts(t, f)) {
		t.Fatal("no-op grew history")
	}
	edited := p
	edited.Title = "Newer deliberate edit"
	f.observe(t, reviewRequest(1, []PullInput{edited}), "source-edit")
	stale := sourcePull(1, "Stale in-flight provider response")
	if r, err := sourcePublish(f, pins, stale); err != nil || r.ID != "" {
		t.Fatal(r, err)
	}
	if got := metadataRead(t, f, 1, ""); got.Pull.Title != edited.Title {
		t.Fatal("concurrent display edit lost")
	}
}

func TestSourceMetadataMergedDateHeadChangesAndRetirement(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	p.State = "open"
	p.Complete = true
	p.Diff = &PullDiff{3, 5, 7}
	f.observe(t, reviewRequest(0, []PullInput{p, nativePull(2)}), "source-start")
	newHead := sourcePull(1, "Rebased provider head")
	newHead.HeadSHA = strings.Repeat("d", 40)
	if _, err := sourcePublish(f, sourcePins(t, f, 1), newHead); err != nil {
		t.Fatal(err)
	}
	got := metadataRead(t, f, 1, "").Pull
	if got.Diff != nil || got.Complete || got.CIState != "unknown" || got.Mergeability != "unknown" {
		t.Fatal("new head inherited old facts", got)
	}
	merged := newHead
	merged.State = "merged"
	merged.MergedAt = "2026-01-01T01:02:03Z"
	merged.Diff = &PullDiff{}
	if _, err := sourcePublish(f, sourcePins(t, f, 1), merged); err != nil {
		t.Fatal(err)
	}
	got = metadataRead(t, f, 1, "").Pull
	if got.State != "merged" || got.MergedAt != merged.MergedAt || got.Diff == nil || *got.Diff != (PullDiff{}) {
		t.Fatal("actual merge or known zero lost", got)
	}
	pins := sourcePins(t, f, 2)
	if w := queueEditHTTP(f, queueEditRequest(3, nil, []RetiredPull{{Number: 2, Reason: "superseded"}}), "source-retire"); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if r, err := sourcePublish(f, pins, sourcePull(2, "Old selected provider response")); err != nil || r.ID != "" {
		t.Fatal(r, err)
	}
	if len(sourcePins(t, f, 2)) != 0 {
		t.Fatal("retired row pinned")
	}
	var queue QueueResponse
	nativeRead(t, f, "/queue", &queue)
	if len(queue.Items) != 1 || queue.Items[0].Number != 1 {
		t.Fatal("retirement lost", queue)
	}
}

func historyTableCounts(t *testing.T, f *apiFixture) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, name := range []string{"review_snapshots", "review_pull_versions", "review_pull_memberships", "events"} {
		var n int64
		if err := f.store.DB().QueryRow("SELECT count(*) FROM " + name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[name] = n
	}
	return out
}

func TestSourceMetadataRetargetAndEventFailureAreSafe(t *testing.T) {
	f := newAPIFixture(t)
	p := nativePull(1)
	p.State = "open"
	p.Diff = &PullDiff{1, 2, 3}
	old := f.observe(t, reviewRequest(0, []PullInput{p}), "source-start")["id"].(string)
	pins := sourcePins(t, f, 1)
	newBase := sourcePull(1, p.Title)
	newBase.BaseSHA = strings.Repeat("e", 40)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER source_event_failure BEFORE INSERT ON events WHEN NEW.event_type='review.observed' BEGIN SELECT RAISE(ABORT,'synthetic source event failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := importDatabaseState(t, f)
	oldBytes := nativeSnapshotBytes(t, f, old)
	if _, err := sourcePublish(f, pins, newBase); err == nil {
		t.Fatal("event failure ignored")
	}
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) || oldBytes != nativeSnapshotBytes(t, f, old) {
		t.Fatal("failed publication changed state")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER source_event_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err := sourcePublish(f, pins, newBase); err != nil {
		t.Fatal(err)
	}
	got := metadataRead(t, f, 1, "").Pull
	if got.BaseSHA != newBase.BaseSHA || got.CIState != "unknown" || got.Mergeability != "unknown" || !reflect.DeepEqual(got.Diff, p.Diff) {
		t.Fatal("retarget reused stale CI/merge facts or lost same-head display metadata", got)
	}
}

func TestSourceMetadataInvalidOptionalEntryPreservesOtherPublication(t *testing.T) {
	for _, field := range []string{"title", "base_ref", "merged_at", "diff"} {
		t.Run(field, func(t *testing.T) {
			f := newAPIFixture(t)
			f.observe(t, reviewRequest(0, []PullInput{nativePull(1), nativePull(2)}), "source-start")
			before := metadataRead(t, f, 1, "").Pull
			bad := sourcePull(1, "Invalid replacement")
			switch field {
			case "title":
				bad.Title = "bad\x7ftitle"
			case "base_ref":
				bad.BaseRef = "bad\x00ref"
			case "merged_at":
				bad.State, bad.MergedAt = "merged", "not-an-instant"
			case "diff":
				bad.Diff = &PullDiff{1, -1, 2}
			}
			good := sourcePull(2, "Valid fresh replacement")
			r, err := sourcePublish(f, sourcePins(t, f, 1, 2), bad, good)
			if err != nil || r.ID == "" {
				t.Fatal("optional refusal blocked valid peer publication", r, err)
			}
			if got := metadataRead(t, f, 1, "").Pull; !reflect.DeepEqual(before, got) {
				t.Fatal("invalid candidate changed current facts", got)
			}
			if got := metadataRead(t, f, 2, "").Pull; got.Title != good.Title {
				t.Fatal("valid candidate was lost", got)
			}
		})
	}
}

func TestSourceMetadataInvalidOptionalDoesNotMaskMandatoryFailure(t *testing.T) {
	for _, field := range []string{"number", "head", "base", "state", "evidence", "duplicate", "pin", "repository", "retained_metadata"} {
		t.Run(field, func(t *testing.T) {
			f := newAPIFixture(t)
			f.observe(t, reviewRequest(0, []PullInput{nativePull(1)}), "source-start")
			pins := sourcePins(t, f, 1)
			bad := sourcePull(1, "bad\x00title")
			pulls := []PullInput{bad}
			switch field {
			case "number":
				pulls[0].Number = 0
			case "head":
				pulls[0].HeadSHA = "not-a-sha"
			case "base":
				pulls[0].BaseSHA = "not-a-sha"
			case "state":
				pulls[0].State = "unrecognized"
			case "evidence":
				pulls[0].Complete = true
			case "duplicate":
				pulls = append(pulls, bad)
			case "repository":
				if _, err := f.store.DB().Exec(`UPDATE projects SET github_repository_id=999 WHERE id=?`, f.project); err != nil {
					t.Fatal(err)
				}
			case "retained_metadata":
				if _, err := f.store.DB().Exec(`UPDATE review_pull_versions SET metadata_json=? WHERE project_id=? AND number=1`, `{"diff":{"files":-1,"additions":0,"deletions":0}}`, f.project); err != nil {
					t.Fatal(err)
				}
				pins = sourcePins(t, f, 1)
			case "pin":
				pin := pins[1]
				pin.project = "another-project"
				pins[1] = pin
			}
			before := importDatabaseState(t, f)
			if _, err := sourcePublish(f, pins, pulls...); err == nil {
				t.Fatal("mandatory failure was ignored")
			}
			if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
				t.Fatal("mandatory failure changed retained state")
			}
		})
	}
}
