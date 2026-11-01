package reviews

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
)

func transferAdmissionPull(number int64) PullInput {
	p := samplePull(number, fmt.Sprintf("Selected pull %d", number))
	p.Complete = false
	return p
}

func TestReviewTransferMaximumBatchUsesOrdinaryWriteBudget(t *testing.T) {
	f := newAPIFixture(t)
	group := TransferGroup{Source: "maximum-selected-producer"}
	for n := int64(1); n <= 100; n++ {
		p := transferAdmissionPull(n)
		for i := 0; i < 2; i++ {
			id := fmt.Sprintf("selected-%d-%d", n, i)
			p.Runs = append(p.Runs, RunInput{SourceRunID: id, Channel: "cli", Scope: "full", Outcome: "unknown"})
			p.Attempts = append(p.Attempts, Attempt{SourceAttemptID: id, SourceRunID: id, Channel: "cli", State: "unknown"})
		}
		group.Pulls = append(group.Pulls, p)
	}
	req := transferRequest(0, group)
	preview := transferPreviewFor(t, f, req)
	if preview.Counts != (TransferCounts{Pulls: 100, Runs: 200, Attempts: 200}) {
		t.Fatal("maximum batch counts changed", preview.Counts)
	}
	apply := TransferApplyRequest{preview.PreviewSHA256, req}
	if len(importJSON(t, apply)) > 256<<10 {
		t.Fatal("fixture exceeds selected transport budget")
	}
	started := time.Now()
	response := importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", "maximum-batch", importJSON(t, apply))
	requireImportResponse(t, response, 201, "")
	t.Logf("100-pull/200-run/200-attempt apply completed in %s through ordinary core.Mutate deadline", time.Since(started))
	for table, want := range map[string]int{"review_current_pulls": 100, "review_current_runs": 200, "review_current_attempts": 200, "receipts": 1, "events": 1} {
		if got := countRows(t, f.store, table); got != want {
			t.Fatal("maximum batch not committed atomically", table, got, want)
		}
	}
	if err := ValidateHistoryReferences(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentSummaries(t.Context(), f.store.DB()); err != nil {
		t.Fatal(err)
	}
}

func TestReviewTransferMixedSelectedTotalsSurviveReplacement(t *testing.T) {
	f := newAPIFixture(t)
	one, two, three := transferAdmissionPull(1), transferAdmissionPull(2), transferAdmissionPull(3)
	one.Runs = []RunInput{{SourceRunID: "changing", Channel: "cli", Scope: "selected totals", Outcome: "active"}}
	one.Attempts = []Attempt{currentSummaryAttempt("changing", "cli", "2026-10-01T00:00:00Z", "")}
	three.Runs = []RunInput{{SourceRunID: "unselected", Channel: "manual", Scope: "selected totals", Outcome: "active"}}
	initial := reviewRequest(0, []PullInput{one, two, three})
	f.observe(t, initial, "totals-original")
	untouched := assertCurrentSummaryReduction(t, f, 3).Pull

	// Mix an existing record correction with existing and new metadata-only
	// pulls. The omitted PR and its incomplete run must remain unchanged.
	one.Runs[0].Complete, one.Runs[0].Outcome = true, "completed"
	one.Runs[0].StartedAt, one.Runs[0].FinishedAt = "2026-10-01T00:00:00Z", "2026-10-01T00:01:00Z"
	one.Attempts[0].FinishedAt = "2026-10-01T00:01:00Z"
	transferApplyFor(t, f, transferRequest(1, TransferGroup{Source: initial.Source, Pulls: []PullInput{one, two, transferAdmissionPull(4)}}), "totals-selected")
	for _, number := range []int64{1, 2, 3, 4} {
		assertCurrentSummaryReduction(t, f, number)
	}
	if got := assertCurrentSummaryReduction(t, f, 3).Pull; !reflect.DeepEqual(got, untouched) {
		t.Fatal("selected transfer changed an omitted PR", got, untouched)
	}
	var incomplete int64
	if err := f.store.DB().QueryRow(`SELECT incomplete_run_count FROM review_current_totals WHERE project_id=? AND pull_number=1`, f.project).Scan(&incomplete); err != nil || incomplete != 0 {
		t.Fatal("completed correction retained an incomplete-run delta", incomplete, err)
	}

	// A later complete observation independently replaces membership and
	// ordering, including deleting metadata-only pulls and the corrected run.
	replacement := reviewRequest(2, []PullInput{three, transferAdmissionPull(1)})
	replacement.Source = initial.Source
	replacement.Retired = []RetiredPull{{Number: 2, Reason: "defunct"}, {Number: 4, Reason: "defunct"}}
	f.observe(t, replacement, "totals-replacement")
	for _, number := range []int64{1, 3} {
		assertCurrentSummaryReduction(t, f, number)
	}
	if got := assertCurrentSummaryReduction(t, f, 1).Pull; got.RunCount != 0 || got.AttemptCount != 0 {
		t.Fatal("later replacement reused stale transfer totals", got)
	}
	for _, number := range []int64{2, 4} {
		var count int
		if err := f.store.DB().QueryRow(`SELECT count(*) FROM review_current_totals WHERE project_id=? AND pull_number=?`, f.project, number).Scan(&count); err != nil || count != 0 {
			t.Fatal("removed pull retained current totals", number, count, err)
		}
	}
}

func TestReviewTransferCurrentRunLinksAndHistoricalOmission(t *testing.T) {
	f := newAPIFixture(t)
	pull := transferAdmissionPull(1)
	pull.Runs = []RunInput{{SourceRunID: "retained-original-run", Channel: "cli", HeadSHA: strings.Repeat("a", 40), Scope: "full", Outcome: "unknown"}}
	transferApplyFor(t, f, transferRequest(0, TransferGroup{Source: "original", Pulls: []PullInput{pull}}), "seed-current-run")
	selected := transferAdmissionPull(1)
	selected.Attempts = []Attempt{{SourceAttemptID: "later-batch-attempt", SourceRunID: pull.Runs[0].SourceRunID, Channel: "cli", HeadSHA: strings.Repeat("a", 40), State: "unknown"}}
	transferApplyFor(t, f, transferRequest(1, TransferGroup{Source: "original", Pulls: []PullInput{selected}}), "link-current-run")
	for _, tc := range []struct {
		name   string
		change func(*TransferGroup)
	}{
		{"cross-producer", func(g *TransferGroup) { g.Source = "other-producer" }},
		{"cross-pr", func(g *TransferGroup) { g.Pulls[0].Number = 2; g.Pulls[0].Title = "Selected pull 2" }},
		{"cross-channel", func(g *TransferGroup) { g.Pulls[0].Attempts[0].Channel = "hosted" }},
		{"cross-head", func(g *TransferGroup) { g.Pulls[0].Attempts[0].HeadSHA = strings.Repeat("b", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var candidate PullInput
			if err := json.Unmarshal(importJSON(t, selected), &candidate); err != nil {
				t.Fatal(err)
			}
			candidate.Attempts[0].SourceAttemptID = "new-attempt"
			group := TransferGroup{Source: "original", Pulls: []PullInput{candidate}}
			tc.change(&group)
			assertTransferAdmissionRefused(t, f, transferRequest(2, group), 409, "observation_conflict")
		})
	}
	// A complete ordinary observation can omit the source run from current
	// membership while its immutable historical version remains retained.
	omission := reviewRequest(2, []PullInput{transferAdmissionPull(1)})
	omission.Source = "original"
	f.observe(t, omission, "omit-original-from-current")
	selected.Attempts[0].SourceAttemptID = "after-omission"
	assertTransferAdmissionRefused(t, f, transferRequest(3, TransferGroup{Source: "original", Pulls: []PullInput{selected}}), 409, "observation_conflict")
	selected.Runs = pull.Runs
	transferApplyFor(t, f, transferRequest(3, TransferGroup{Source: "original", Pulls: []PullInput{selected}}), "selected-historical-link")
}

// A stateless binding is not an admission token: deliberately manufacture a
// correctly scoped digest to exercise the apply transaction's own refusal too.
func assertTransferAdmissionRefused(t *testing.T, f *apiFixture, req TransferRequest, status int, code string) {
	t.Helper()
	body, err := normalizeTransfer(&req)
	if err != nil {
		t.Fatal("refusal fixture malformed before destination admission", err)
	}
	binding, err := transferPreview(f.store.InstanceID(), transferTestScope(t, f), req, body)
	if err != nil {
		t.Fatal(err)
	}
	before := importDatabaseState(t, f)
	requireImportResponse(t, importHTTP(f, importBase(f)+"/transfer/preview", f.admin, "", "", body), status, code)
	requireImportResponse(t, importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", "refused-transfer", importJSON(t, TransferApplyRequest{binding.PreviewSHA256, req})), status, code)
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("refused preview/apply changed retained values")
	}
}

func TestReviewTransferDestinationBindingAdmissionAndStaleCAS(t *testing.T) {
	f := newAPIFixture(t)
	req := transferRequest(0, TransferGroup{Source: "original", Pulls: []PullInput{transferAdmissionPull(1)}})
	original := transferPreviewFor(t, f, req)
	otherToken := "second-transfer-administrator"
	sum := sha256.Sum256([]byte(otherToken))
	if _, err := f.store.DB().Exec(`INSERT INTO credentials VALUES('second-transfer-admin','second-transfer-principal',?,1,0)`, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO workers VALUES(?,'transfer-worker','Transfer worker')`, f.project); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, token, worker string }{{"principal", otherToken, ""}, {"worker", f.admin, "transfer-worker"}} {
		t.Run(tc.name, func(t *testing.T) {
			before := importDatabaseState(t, f)
			w := importHTTP(f, importBase(f)+"/transfer/preview", tc.token, tc.worker, "", importJSON(t, req))
			requireImportResponse(t, w, 200, "")
			var got TransferPreview
			if json.Unmarshal(w.Body.Bytes(), &got) != nil || got.PayloadSHA256 != original.PayloadSHA256 || got.PreviewSHA256 == original.PreviewSHA256 {
				t.Fatal("destination not independently bound")
			}
			requireImportResponse(t, importHTTP(f, importBase(f)+"/transfer/apply", tc.token, tc.worker, "foreign-binding", importJSON(t, TransferApplyRequest{original.PreviewSHA256, req})), 409, "import_preview_mismatch")
			if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
				t.Fatal("scope mismatch changed state")
			}
		})
	}
	otherInstance := newAPIFixture(t)
	if _, err := otherInstance.store.DB().Exec(`UPDATE credentials SET principal_id=? WHERE is_admin=1`, importPrincipal(t, f)); err != nil {
		t.Fatal(err)
	}
	otherPreview := transferPreviewFor(t, otherInstance, req)
	if otherPreview.PayloadSHA256 != original.PayloadSHA256 || otherPreview.PreviewSHA256 == original.PreviewSHA256 {
		t.Fatal("instance not independently bound")
	}
	requireImportResponse(t, importHTTP(otherInstance, importBase(otherInstance)+"/transfer/apply", otherInstance.admin, "", "foreign-instance", importJSON(t, TransferApplyRequest{original.PreviewSHA256, req})), 409, "import_preview_mismatch")
	if _, err := f.store.DB().Exec(`INSERT INTO projects VALUES('transfer-other-project','Other',702,'owner/other')`); err != nil {
		t.Fatal(err)
	}
	otherReq := req
	otherReq.RepositoryID = 702
	otherPath := "/api/v1/projects/transfer-other-project/reviews/transfer/"
	requireImportResponse(t, importHTTP(f, otherPath+"apply", f.admin, "", "foreign-project", importJSON(t, TransferApplyRequest{original.PreviewSHA256, otherReq})), 409, "import_preview_mismatch")
	invalidRepo := req
	invalidRepo.RepositoryID = 702
	assertTransferAdmissionRefused(t, f, invalidRepo, 400, "validation_failed")
	before := importDatabaseState(t, f)
	requireImportResponse(t, importHTTP(f, importBase(f)+"/transfer/preview", f.readToken, "", "", importJSON(t, req)), 403, "forbidden")
	if !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("read credential preview changed state")
	}
	transferApplyFor(t, f, req, "advance-after-preview")
	assertTransferAdmissionRefused(t, f, req, 409, "revision_conflict")
	// The actual older, once-admitted preview is also refused under a fresh key.
	requireImportResponse(t, importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", "stale-admitted-preview", importJSON(t, TransferApplyRequest{original.PreviewSHA256, req})), 409, "revision_conflict")
}

type transferRevokingReader struct {
	reader     io.Reader
	beforeRead func()
}

func (r *transferRevokingReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		fn := r.beforeRead
		r.beforeRead = nil
		fn()
	}
	return r.reader.Read(p)
}

func TestReviewTransferReauthorizesAfterInitialScope(t *testing.T) {
	for _, operation := range []string{"preview", "apply"} {
		t.Run(operation, func(t *testing.T) {
			f := newAPIFixture(t)
			req := transferRequest(0, TransferGroup{Source: "original", Pulls: []PullInput{transferAdmissionPull(1)}})
			var input any = req
			if operation == "apply" {
				preview := transferPreviewFor(t, f, req)
				input = TransferApplyRequest{preview.PreviewSHA256, req}
			}
			var afterRevocation map[string][]string
			reader := &transferRevokingReader{reader: bytes.NewReader(importJSON(t, input)), beforeRead: func() {
				sum := sha256.Sum256([]byte(f.admin))
				if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE token_sha256=?`, hex.EncodeToString(sum[:])); err != nil {
					t.Fatal(err)
				}
				afterRevocation = importDatabaseState(t, f)
			}}
			r := httptest.NewRequest("POST", importBase(f)+"/transfer/"+operation, reader)
			r.Header.Set("Authorization", "Bearer "+f.admin)
			r.Header.Set("X-FC-Instance", f.store.InstanceID())
			r.Header.Set("Idempotency-Key", "revoked-after-scope")
			w := httptest.NewRecorder()
			f.mux.ServeHTTP(w, r)
			requireImportResponse(t, w, 401, "unauthorized")
			if afterRevocation == nil || !reflect.DeepEqual(afterRevocation, importDatabaseState(t, f)) {
				t.Fatal("revocation was not rechecked atomically before effects")
			}
		})
	}
}

func TestReviewTransferRestartReplayAndPinnedQueueContinuation(t *testing.T) {
	f := newAPIFixture(t)
	req := transferRequest(0, TransferGroup{Source: "original", Pulls: []PullInput{transferAdmissionPull(1), transferAdmissionPull(2)}})
	first, apply, receipt := transferApplyFor(t, f, req, "original-transfer-key")
	page := f.call("GET", importBase(f)+"/queue?limit=1", f.readToken, "", nil)
	requireImportResponse(t, page, 200, "")
	var firstPage QueueResponse
	if json.Unmarshal(page.Body.Bytes(), &firstPage) != nil || !firstPage.HasMore || firstPage.Cursor == "" {
		t.Fatal("pinned queue continuation fixture incomplete")
	}
	tailPath := importBase(f) + "/queue?limit=1&cursor=" + url.QueryEscape(firstPage.Cursor)
	oldTail := f.call("GET", tailPath, f.readToken, "", nil)
	requireImportResponse(t, oldTail, 200, "")
	transferApplyFor(t, f, transferRequest(1, TransferGroup{Source: "second", Pulls: []PullInput{transferAdmissionPull(3)}}), "advance-before-restart")
	var seq int
	var name, dbPath string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &dbPath); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(context.Background(), filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	f.store = store
	mux := http.NewServeMux()
	Register(mux, store)
	f.mux = mux
	before := importDatabaseState(t, f)
	w := importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", "original-transfer-key", importJSON(t, apply))
	requireImportResponse(t, w, 201, "")
	if w.Body.String() != receipt || !reflect.DeepEqual(before, importDatabaseState(t, f)) {
		t.Fatal("restart replay changed original receipt or state")
	}
	tail := f.call("GET", tailPath, f.readToken, "", nil)
	requireImportResponse(t, tail, 200, "")
	var parsed QueueResponse
	if tail.Body.String() != oldTail.Body.String() || json.Unmarshal(tail.Body.Bytes(), &parsed) != nil || parsed.SnapshotID != first.ID || len(parsed.Items) != 1 || parsed.Items[0].Number != 2 || parsed.HasMore {
		t.Fatal("old cursor drifted into additive current workspace")
	}
	sum := sha256.Sum256([]byte(f.admin))
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE token_sha256=?`, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	afterRevocation := importDatabaseState(t, f)
	requireImportResponse(t, importHTTP(f, importBase(f)+"/transfer/apply", f.admin, "", "original-transfer-key", importJSON(t, apply)), 401, "unauthorized")
	if !reflect.DeepEqual(afterRevocation, importDatabaseState(t, f)) {
		t.Fatal("revoked replay changed state")
	}
}
