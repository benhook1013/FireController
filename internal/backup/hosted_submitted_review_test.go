package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
	"github.com/benhook1013/FireController/internal/reviewruntime"
)

// Complete the fixture's original admitted operation using the real hosted
// retainer. Synthetic provider objects supply source evidence, not final ledger
// JSON or comparison credit.
func submittedReviewInventoryFixture(t *testing.T) (*restoreFixture, reviewexec.ReviewEvidence, string) {
	t.Helper()
	f := newRestoreFixture(t)
	e, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "restore-cap", Provider: "coderabbit", Account: "fixture-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("f", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	var attemptID string
	var revision int64
	if err := f.store.DB().QueryRow(`SELECT id,revision FROM review_execution_attempts WHERE state='preparing'`).Scan(&attemptID, &revision); err != nil {
		t.Fatal(err)
	}
	query, err := e.ReadHostedAllowanceQuery(t.Context(), f.project, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	intent := reviewprovider.HostedTriggerIntent{OperationID: query.OperationID, AttemptID: attemptID, HeadSHA: query.Anchor.CandidateSHA}
	command, err := reviewprovider.HostedTriggerBody(intent)
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.Admit(t.Context(), f.project, attemptID, revision, query.Anchor, command)
	if err != nil {
		t.Fatal("admit original hosted operation", err)
	}
	a, err = e.Advance(t.Context(), f.project, a.ID, a.Revision, "active", "synthetic_posted", "100")
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := e.ReadHostedRecovery(t.Context(), f.project, a.ID, "restore-cap")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	const runID = "33333333-3333-4333-8333-333333333333"
	body := "<!-- coderabbit-ai-generated:v1 -->\n\n**Actionable comments posted: 1**\n\n---\n<details>\n<summary>ℹ️ Review info</summary>\n<details>\n<summary>⚙️ Run configuration</summary>\n- **Configuration used**: Repository: synthetic/restore/.coderabbit.yaml\n- **Review profile**: ASSERTIVE\n- **Plan**: Advanced\n- **Run ID**: `" + runID + "`\n</details>\n<details>\n<summary>📥 Commits</summary>\nReviewing files that changed from the base of the PR and between " + query.Anchor.ParentSHA + " and " + intent.HeadSHA + ".\n</details>\n<details>\n<summary>📒 Files selected for processing: 1</summary>\n* `source.go`\n</details>\n</details>\n<!-- This is an auto-generated comment by CodeRabbit for review status -->"
	root := "<!-- coderabbit-ai-generated:v1 -->\n\n**🔒 Security & Privacy** | **🟡 Minor** | **⚡ Quick win**\n\n**Preserve exact evidence.**\n\nLiteral <b>immutable detail</b> & source spacing.\n\n<!-- cr-comment:v1:000000000000000000000001 -->\n\n<!-- This is an auto-generated comment by CodeRabbit -->"
	ack := "<!-- coderabbit-ai-generated:v1 -->\n\n<!-- This is an auto-generated reply by CodeRabbit -->\n<!-- CodeRabbit review command invocation: v2:" + strings.Repeat("a", 64) + " -->\n<details>\n<summary>✅ Action performed</summary>\nFull review finished.\n</details>"
	line := int64(7)
	record := reviewruntime.HostedCaptureRecord{
		RepositoryID: query.Anchor.RepositoryID, Repository: query.Anchor.Repository, PullNumber: query.Anchor.PullNumber,
		Intent: intent, PostingActor: query.PostingActor, ProviderActor: query.ProviderActor, EarliestPosting: recovery.EarliestPosting,
		StartedAt: at.Add(400 * time.Millisecond), FinishedAt: at.Add(600 * time.Millisecond),
		Snapshot: reviewprovider.HostedPublicationSnapshot{
			Complete: true, ObservedAt: at.Add(500 * time.Millisecond),
			IssueComments: []reviewprovider.IssueComment{
				{ID: 100, Body: command, Author: query.PostingActor, CreatedAt: at, UpdatedAt: at},
				{ID: 400, Body: ack, Author: query.ProviderActor, CreatedAt: at.Add(300 * time.Millisecond), UpdatedAt: at.Add(300 * time.Millisecond)},
			},
			Reviews:        []reviewprovider.HostedReview{{ID: 200, Body: body, State: "commented", HeadSHA: intent.HeadSHA, Author: query.ProviderActor, SubmittedAt: at.Add(200 * time.Millisecond)}},
			ReviewComments: []reviewprovider.ReviewComment{{ID: 300, ReviewID: 200, Body: root, Author: query.ProviderActor, Path: "source.go", OriginalLine: &line, Side: "RIGHT", HeadSHA: intent.HeadSHA, OriginalHeadSHA: intent.HeadSHA, CreatedAt: at.Add(100 * time.Millisecond), UpdatedAt: at.Add(100 * time.Millisecond)}},
		},
	}
	captures, err := reviewruntime.NewHostedCaptureStore(filepath.Join(f.dir, "hosted-review-captures"))
	if err != nil {
		t.Fatal(err)
	}
	retained, err := reviewruntime.RetainHosted(t.Context(), captures, e, a, record)
	if err != nil || !retained.Complete || retained.Reason != "capture_recorded" {
		t.Fatalf("retain submitted hosted source: %+v %v", retained, err)
	}
	loaded, err := captures.Read(t.Context(), a.ID, retained.Capture)
	if err != nil || !reflect.DeepEqual(loaded, record) {
		t.Fatal("retainer changed original provider record", err)
	}
	evidence, err := e.ReadReviewEvidence(t.Context(), routePolicyInventoryScope(t, f), retained.Receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := evidence.Capture.Hosted
	if h == nil || h.SubmittedReview == nil || h.SubmittedReview.ProviderRunID != runID || h.SubmittedReview.AcknowledgementCommentID != 400 || !reflect.DeepEqual(h.ReviewIDs, []int64{200}) || h.TriggerCommentID != 100 || h.PublicationID != "" || h.ProviderAttemptID != "" || h.StandingSummary != nil || h.ObservedBaseSHA != query.Anchor.ParentSHA || len(h.Sources) != 1 || h.Sources[0].ReviewID != 200 || h.Sources[0].CommentID != 300 || len(evidence.Capture.Review.Findings) != 1 || evidence.Capture.FinishedAt != record.Snapshot.IssueComments[1].UpdatedAt.Format(time.RFC3339Nano) {
		t.Fatal("actual retainer lost submitted source identity, finding or ACK timing")
	}
	// Finding detail ends at its fingerprint; the complete original root,
	// including its generated footer, remains in the immutable raw capture.
	if evidence.Capture.Review.Findings[0].Title != "Preserve exact evidence." || evidence.Capture.Review.Findings[0].Detail != strings.TrimSuffix(root, "\n\n<!-- This is an auto-generated comment by CodeRabbit -->") {
		t.Fatal("actual retainer changed submitted finding detail")
	}
	if _, err = e.Advance(t.Context(), f.project, a.ID, a.Revision, "completed", "synthetic_captured", "100"); err != nil {
		t.Fatal("complete original hosted operation", err)
	}
	if err := reviewexec.ValidateComparisonProofs(t.Context(), f.store.DB()); err != nil {
		t.Fatal("completed submitted fixture has no valid comparison authority", err)
	}
	return f, evidence, "hosted-review-captures/" + a.ID + "-hosted-" + retained.Capture.SHA256 + ".json"
}

func TestHostedSubmittedReviewCanonicalBackupInactiveRestoreAndMixedShapeRefusal(t *testing.T) {
	f, evidence, capturePath := submittedReviewInventoryFixture(t)
	before := backupRows(t, f.store.DB())
	sourceRaw, err := os.ReadFile(filepath.Join(f.dir, capturePath))
	if err != nil {
		t.Fatal(err)
	}
	var signingKey string
	if err := f.store.DB().QueryRow(`SELECT value FROM metadata WHERE key='signing_key'`).Scan(&signingKey); err != nil {
		t.Fatal(err)
	}
	bundle := restoreBundle(t, f)
	immutable := restoreTree(t, bundle.Path)
	inspected, err := Inspect(t.Context(), bundle.Path)
	if err != nil || inspected.State != "verified" {
		t.Fatal("inspect genuine completed submitted review", err)
	}
	var m Manifest
	manifest, err := os.ReadFile(filepath.Join(bundle.Path, "manifest.json"))
	if err != nil || json.Unmarshal(manifest, &m) != nil {
		t.Fatal("read verified artifact inventory", err)
	}
	if immutable[capturePath] != string(sourceRaw) {
		t.Fatal("backup changed raw submitted review/ACK capture")
	}
	destination := filepath.Join(t.TempDir(), "inactive-submitted-review")
	summary, err := Restore(t.Context(), bundle.Path, destination)
	if err != nil || summary.State != "inactive" || summary.SourceInstanceID != f.store.InstanceID() || summary.InstanceID == summary.SourceInstanceID {
		t.Fatal("inactive submitted review restore", summary, err)
	}
	restored, err := core.Open(t.Context(), destination)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	backupCode(t, StartupGuard(t.Context(), restored), "restore_inactive")
	var newKey string
	var enabled int
	if err := restored.DB().QueryRow(`SELECT value FROM metadata WHERE key='signing_key'`).Scan(&newKey); err != nil || newKey == signingKey || !digest.MatchString(newKey) {
		t.Fatal("restore reused signing identity", err)
	}
	if err := restored.DB().QueryRow(`SELECT count(*) FROM credentials WHERE revoked<>1`).Scan(&enabled); err != nil || enabled != 0 {
		t.Fatal("restore enabled old credentials", err)
	}
	if err := reviewexec.ValidateComparisonProofs(t.Context(), restored.DB()); err != nil {
		t.Fatal("restored submitted comparison authority invalid", err)
	}
	after := backupRows(t, restored.DB())
	for table, rows := range before {
		if table != "metadata" && table != "credentials" && after[table] != rows {
			t.Fatalf("restore changed submitted evidence or retained records: %s", table)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(destination, capturePath)); err != nil || string(raw) != string(sourceRaw) {
		t.Fatal("restore changed original submitted raw bytes", err)
	}
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) || !reflect.DeepEqual(immutable, restoreTree(t, bundle.Path)) {
		t.Fatal("create/inspect/restore changed source or immutable bundle")
	}

	// Mix the positive submitted branch with a batched-publication identity in
	// the disposable bundle. Recompute canonical JSON and its fingerprint and
	// reseal the manifest: rejection must not merely detect stale hashes. Keep
	// the exact invalidation trigger so physical schema/FK validation still pass.
	var trigger string
	for _, statement := range reviewexec.ComparisonSchemaSQL() {
		if strings.HasPrefix(statement, "CREATE TRIGGER review_comparison_dirty_review_execution_evidence_update ") {
			trigger = statement
		}
	}
	if trigger == "" {
		t.Fatal("canonical evidence invalidation guard missing")
	}
	evidence.Capture.Hosted.PublicationID = "11111111-1111-4111-8111-111111111111"
	capture, err := json.Marshal(evidence.Capture)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(capture)
	body, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	mutateBundleSQL(t, bundle.Path, `DROP TRIGGER review_comparison_dirty_review_execution_evidence_update;
UPDATE review_execution_evidence SET body_json=?,fingerprint=? WHERE id=?;
`+trigger, string(body), hex.EncodeToString(fingerprint[:]), evidence.ID)
	db, err := openSnapshot(t.Context(), filepath.Join(bundle.Path, "firecontroller.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	refs := make(map[string]Artifact, len(m.Artifacts))
	for _, artifact := range m.Artifacts {
		refs[artifact.Path] = artifact
	}
	if err := validateEvidenceReferences(t.Context(), db, refs); err != nil {
		db.Close()
		t.Fatal("mixed-shape fixture must retain canonical evidence/fingerprint/raw references", err)
	}
	if err := reviewexec.ValidateComparisonProofs(t.Context(), db); err == nil {
		db.Close()
		t.Fatal("completed submitted mixed metadata was accepted as comparison authority")
	}
	db.Close()
	assertCurrentSummaryInventoryRefused(t, bundle, true)
	if !reflect.DeepEqual(before, backupRows(t, f.store.DB())) {
		t.Fatal("rejected mixed-shape bundle changed source")
	}
}
