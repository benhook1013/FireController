package reviewexec

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviews"
)

func TestExternalHostedAbandonedLateOriginalHeadPreservedOnce(t *testing.T) {
	f, a, _ := pendingHostedSettlementFixture(t)
	if err := reviews.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	f.e.Register(mux)
	reviews.Register(mux, f.store)
	f.handler = mux
	original := projectionPull(2)
	original.HeadSHA = a.Anchor.CandidateSHA
	publishMetadata := func(pull reviews.PullInput) {
		t.Helper()
		var revision int64
		if err := f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, attemptProject).Scan(&revision); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		response := f.request("POST", "/api/v1/projects/"+attemptProject+"/reviews/observations", f.admin, "metadata-"+pull.HeadSHA, encoded(reviews.ObservationRequest{ExpectedRevision: &revision, RepositoryID: a.Anchor.RepositoryID, Source: "synthetic-metadata", ObservedAt: stamp(), Queue: []reviews.PullInput{pull}}))
		if response.Code != 201 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	publishMetadata(original)
	target := lifecycleTarget{project: attemptProject, repository: a.Anchor.RepositoryID, number: 2}
	at := time.Now().UTC()
	snapshot := externalSnapshot(1, a.Anchor.CandidateSHA)
	snapshot.ObservedAt = at.Add(2 * time.Second)
	snapshot.IssueComments = []reviewprovider.IssueComment{{ID: 101, Author: reviewprovider.Author{ID: 201, Login: "operator", Kind: "User"}, Body: a.Intent, CreatedAt: at, UpdatedAt: at}}
	snapshot.Reviews[0].SubmittedAt = at.Add(time.Second)
	snapshot.Reviews[0].Body = strings.Replace(snapshot.Reviews[0].Body, "posted: 0", "posted: 1", 1)
	line := int64(7)
	snapshot.ReviewComments = []reviewprovider.ReviewComment{{ID: 501, ReviewID: 401, Author: snapshot.Reviews[0].Author, HeadSHA: a.Anchor.CandidateSHA, OriginalHeadSHA: a.Anchor.CandidateSHA, Path: "original.go", Line: &line, OriginalLine: &line, Side: "RIGHT", CreatedAt: at.Add(time.Second), UpdatedAt: at.Add(time.Second), Body: "_⚠️ Potential issue_ | _🟠 Major_\n\n**Preserve a late genuine finding.**\n\nKeep this provider finding after local abandonment.\n\n<!-- cr-comment:v1:000000000000000000000001 -->"}}
	runs, err := reviewprovider.DiscoverExternalHosted(snapshot, snapshot.Reviews[0].Author)
	if err != nil || len(runs) != 1 || !runs[0].Inventory.Complete || len(runs[0].Inventory.Findings) != 1 || runs[0].NativeOperationID != a.OperationID || runs[0].TriggerCommentID != 101 {
		t.Fatal("supported original provider source not discovered", runs, err)
	}
	run := runs[0]
	if count, held := reconciliationObserve(t, f, target, run, 100); count != 0 || held {
		t.Fatal("live native request was imported externally", count, held)
	}
	body := hostedSettlementBody(a)
	body["external_action_stopped"] = false
	if response := hostedSettlementCall(t, f, a, "abandon-pending-late", body); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	// Publishing prepared fixes changes current metadata while the provider can
	// still finish the abandoned original comparison. Neither may replace the other.
	moved := original
	moved.HeadSHA = strings.Repeat("c", 40)
	publishMetadata(moved)
	before := transferAuthorityRows(t, f, true)
	if count, held := reconciliationObserve(t, f, target, run, 100); count != 1 || held {
		t.Fatal("late original result lost after head movement", count, held)
	}
	if count, held := reconciliationObserve(t, f, target, run, 100); count != 0 || held {
		t.Fatal("late result replay duplicated", count, held)
	}
	f.e, err = New(f.store, attemptCapabilities, WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	if count, held := reconciliationObserve(t, f, target, run, 100); count != 0 || held {
		t.Fatal("restart duplicated late source", count, held)
	}
	after := transferAuthorityRows(t, f, true)
	for _, table := range []string{"review_operations", "review_execution_attempts", "review_execution_evidence", "review_hosted_observations", "review_round_controls", "review_round_completions", "review_round_changes"} {
		if !reflect.DeepEqual(before[table], after[table]) {
			t.Fatal("late archive changed native authority/accounting", table)
		}
	}
	var detail reviews.RunsResponse
	projectionRead(t, f, "/pulls/2/runs", &detail)
	if len(detail.Items) != 1 || detail.Items[0].HeadSHA != a.Anchor.CandidateSHA || detail.Items[0].Found != 1 || detail.Items[0].Accepted != 0 || detail.Items[0].Finalized || !detail.Items[0].Complete {
		t.Fatal("late source identity/findings changed or gained credit", detail)
	}
	if current := lifecycleRead(t, f); current.HeadSHA != moved.HeadSHA {
		t.Fatal("historical result replaced current PR head", current.HeadSHA)
	}
	// Other cancellation reasons must not acquire the narrow abandonment exception.
	admissionSQL(t, f, `UPDATE review_operations SET reason='operator_verified_external_stop' WHERE id=?`, a.OperationID)
	err = f.e.readAdmission(t.Context(), func(ctx context.Context, tx *sql.Tx) error {
		owned, held, err := externalHostedNativeBinding(ctx, tx, target, run)
		if owned || !held {
			t.Fatal("unrelated cancellation used abandon exception", owned, held)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
