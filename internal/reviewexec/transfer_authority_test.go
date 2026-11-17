package reviewexec

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

// Capture every physical row for refusal atomicity; successful imports compare
// only real executor/capture/adjudication/proof authority, excluding their own
// permitted projection, event and receipt writes.
func transferAuthorityRows(t *testing.T, f *attemptFixture, all bool) map[string][]string {
	t.Helper()
	rows, err := f.store.DB().Query(`SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		if all || strings.HasPrefix(table, "review_execution_") || strings.HasPrefix(table, "review_adjudication_") || strings.HasPrefix(table, "review_comparison_") || table == "review_operations" || table == "review_hosted_observations" {
			tables = append(tables, table)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	result := map[string][]string{}
	for _, table := range tables {
		rows, err := f.store.DB().Query(`SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		result[table] = []string{}
		for rows.Next() {
			values, pointers := make([]any, len(columns)), make([]any, len(columns))
			for i := range pointers {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			result[table] = append(result[table], encoded(values))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		sort.Strings(result[table])
	}
	return result
}

func transferAuthoritySetup(t *testing.T, f *attemptFixture) string {
	t.Helper()
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	return "/api/v1/projects/" + attemptProject + "/reviews/transfer"
}

func TestTransferAuthorityNativeLabelWithoutJournalCannotCreateCredit(t *testing.T) {
	f := newAttemptFixture(t)
	path := transferAuthoritySetup(t, f)
	pull := projectionPull(2)
	pull.Complete = false
	for _, channel := range []string{"cli", "hosted"} {
		id := "review_evidence_" + strings.Repeat("1", 32)
		attemptID := "review_attempt_" + strings.Repeat("1", 32)
		if channel == "hosted" {
			id = "review_evidence_" + strings.Repeat("2", 32)
			attemptID = "review_attempt_" + strings.Repeat("2", 32)
		}
		run := reviews.RunInput{SourceRunID: id, Channel: channel, HeadSHA: pull.HeadSHA, Reviewer: "Imported native label", Scope: "admitted comparison", Outcome: "completed", Attributable: true, Complete: true, Finalized: true, StartedAt: "2026-01-01T00:00:00Z", FinishedAt: "2026-01-01T00:01:00Z", CoverageLimits: []string{}, Findings: []reviews.Finding{}}
		pull.Runs = append(pull.Runs, run)
		pull.Attempts = append(pull.Attempts, reviews.Attempt{SourceAttemptID: attemptID, Channel: channel, HeadSHA: pull.HeadSHA, State: "completed", StartedAt: run.StartedAt, FinishedAt: run.FinishedAt, SourceRunID: id})
	}
	revision := int64(0)
	req := reviews.TransferRequest{Version: 1, ExpectedRevision: &revision, RepositoryID: 801, Publisher: "synthetic-transfer", Groups: []reviews.TransferGroup{{Source: "native-executor", Pulls: []reviews.PullInput{pull}}}}
	before := transferAuthorityRows(t, f, true)
	previewHTTP := f.request("POST", path+"/preview", f.admin, "", encoded(req))
	evidenceHTTPRequire(t, previewHTTP, 200, "")
	if !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
		t.Fatal("native-labelled preview changed retained state")
	}
	var preview reviews.TransferPreview
	if err := json.Unmarshal(previewHTTP.Body.Bytes(), &preview); err != nil || preview.NativeCoverage || !preview.ImportedHistory {
		t.Fatal("preview promoted imported authority", err)
	}
	authority := transferAuthorityRows(t, f, false)
	evidenceHTTPRequire(t, f.request("POST", path+"/apply", f.admin, "native-display", encoded(reviews.TransferApplyRequest{PreviewSHA256: preview.PreviewSHA256, Batch: req})), 201, "")
	var stored reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &stored)
	if len(stored.Items) != 2 || stored.Items[0].Source != "native-executor" || !stored.Items[0].Finalized {
		t.Fatal("display import was not actually installed")
	}
	plan := SourcePlan{Repository: reviewprovider.Repository{ID: 801, FullName: "owner/repo", DefaultBranch: "main"}, Pull: reviewprovider.Pull{Number: 2, RepositoryID: 801, State: "open", HeadSHA: pull.HeadSHA, BaseSHA: pull.BaseSHA, BaseRef: pull.BaseRef, ChangedFiles: 1}, ParentSHA: pull.BaseSHA, ObservedAt: stamp()}
	beforeReads := transferAuthorityRows(t, f, true)
	for _, channel := range []string{"cli", "hosted"} {
		got, err := f.e.Comparison(t.Context(), attemptProject, plan, channel)
		if err != nil || got != (ComparisonState{}) {
			t.Fatal("native-labelled imported display acquired comparison credit", channel, got, err)
		}
	}
	for _, run := range pull.Runs {
		_, err := f.e.ReadPolicyAssessment(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), run.SourceRunID)
		attemptCode(t, err, "not_found")
	}
	if !reflect.DeepEqual(authority, transferAuthorityRows(t, f, false)) || !reflect.DeepEqual(beforeReads, transferAuthorityRows(t, f, true)) {
		t.Fatal("import labels or authority reads changed journals, captures, adjudication or proofs")
	}
}

func TestTransferAuthorityActualCaptureAndJournalCollisionsRefuseAtomically(t *testing.T) {
	f, a, evidence := cliPublicationFixture(t, 0, "completed")
	coverageFinalize(t, f, a, evidence.ID, 0)
	path := transferAuthoritySetup(t, f)
	pull := projectionPull(2)
	if _, err := f.e.PublishEvidence(t.Context(), attemptProject, evidence.ID, &pull); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.PublishAttempt(t.Context(), attemptProject, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Read exact owner-generated records: refusal must still happen when an
	// importer supplies a byte-equivalent legitimate display projection.
	tx, err := f.store.DB().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	actualAttempt, err := readAttempt(t.Context(), tx, attemptProject, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := readOperation(t.Context(), tx, attemptProject, a.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	projectedAttempt, err := journalBrowserAttempt(t.Context(), tx, operation, actualAttempt)
	if err != nil {
		t.Fatal(err)
	}
	_, state, err := loadReviewAdjudication(t.Context(), tx, attemptProject, evidence.ID)
	if err != nil {
		t.Fatal(err)
	}
	projectedRun, err := publicationRun(evidence, state, actualAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !projectedRun.Finalized || projectedAttempt.SourceRunID != evidence.ID || actualAttempt.State != "completed" {
		t.Fatal("fixture did not retain genuine completed native authority")
	}
	// A separate real started operation has a pending operation.attempt_id and
	// a genuine attempt row. Neither key may be preempted before capture.
	pending := f.start(t, attemptProject, f.enqueue(t, attemptProject, "transfer-pending", "cli", 4, 1))
	pendingOperation, err := f.e.ReadOperation(t.Context(), evidenceReadScope(t, f, attemptProject, f.reader), pending.OperationID)
	if err != nil || pendingOperation.AttemptID != pending.ID || pendingOperation.State != "preparing" {
		t.Fatal("pending operation pointer was not installed", err)
	}
	for _, test := range []struct {
		name string
		pull reviews.PullInput
	}{
		{"equal-capture", func() reviews.PullInput { p := pull; p.Runs = []reviews.RunInput{projectedRun}; return p }()},
		{"equal-completed-attempt", func() reviews.PullInput { p := pull; p.Attempts = []reviews.Attempt{projectedAttempt}; return p }()},
		{"linked-local-evidence", func() reviews.PullInput {
			p := pull
			copy := projectedAttempt
			copy.SourceAttemptID = "foreign-attempt"
			p.Attempts = []reviews.Attempt{copy}
			return p
		}()},
		{"pending-attempt-and-pointer", func() reviews.PullInput {
			p := projectionPull(4)
			p.Attempts = []reviews.Attempt{{SourceAttemptID: pending.ID, Channel: "cli", HeadSHA: p.HeadSHA, State: "preparing"}}
			return p
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.pull.Complete = false
			var revision int64
			if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			req := reviews.TransferRequest{Version: 1, ExpectedRevision: &revision, RepositoryID: 801, Publisher: "synthetic-transfer", Groups: []reviews.TransferGroup{{Source: "native-executor", Pulls: []reviews.PullInput{test.pull}}}}
			before := transferAuthorityRows(t, f, true)
			evidenceHTTPRequire(t, f.request("POST", path+"/preview", f.admin, "", encoded(req)), 409, "observation_conflict")
			if !reflect.DeepEqual(before, transferAuthorityRows(t, f, true)) {
				t.Fatal("refused transfer changed original history, journal, captures, adjudication, proofs, receipts or events")
			}
		})
	}
}
