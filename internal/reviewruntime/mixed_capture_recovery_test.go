package reviewruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewexec"
)

// Independent composition: a mixed inventory survives failed browser publication,
// and restart recovery uses its final evidence while its provider is unavailable.
func TestMixedCaptureOfflinePublicationRecovery(t *testing.T) {
	f, captures, attempt, reader := recoveryRuntimeFixture(t, "active")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	recoveryRuntimeMetadata(t, f, attempt)
	root := &reader.record.Snapshot.ReviewComments[0]
	root.Body += "\n\n" + hostedRetainInline(2, "Second atomic finding in the same root.")
	duplicate := "<details>\n<summary>♻️ Duplicate comments (1)</summary><blockquote>\n\n<details>\n<summary>design/source.md (1)</summary><blockquote>\n\n`5-5`: _⚠️ Potential issue_ | _🔵 Trivial_ | _💤 Low value_\n\n**Keep duplicate source identity.**\n\nLiteral duplicate detail & spacing.\n\n<!-- cr-comment:v1:000000000000000000000003 -->\n\n</blockquote></details>\n\n</blockquote></details>"
	reader.record.Snapshot.Reviews[0].Body = strings.Replace(reader.record.Snapshot.Reviews[0].Body, hostedRetainPublicationMarker(), duplicate+"\n\n"+hostedRetainPublicationMarker(), 1)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER second_data_fail_projection BEFORE INSERT ON review_snapshots BEGIN SELECT RAISE(ABORT,'controlled publication failure'); END`); err != nil {
		t.Fatal(err)
	}
	first, err := RecoverHosted(ctx, reader, captures, f.engine, f.project, attempt.ID, "runtime-capability", reader.record.PostingActor, reader.record.ProviderActor)
	if err == nil || first.Reason != "publication_pending" || first.EvidenceID == "" {
		t.Fatal("fixture failed to reach retained evidence/publication boundary", first, err)
	}
	if recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 || recoveryRuntimeCount(t, f, "review_execution_reservations") != 1 || recoveryRuntimeCount(t, f, "review_round_completions") != 0 {
		t.Fatal("failed publication fabricated terminal credit or released admission")
	}
	evidence, err := f.engine.ReadReviewEvidence(ctx, f.scopeWithAccess(t, f.reader, false, "read"), first.EvidenceID)
	if err != nil || evidence.Capture.Hosted == nil || len(evidence.Capture.Hosted.Sources) != 3 || len(evidence.Capture.Review.Findings) != 3 {
		t.Fatal("mixed capture was not retained in full", evidence, err)
	}
	sources := evidence.Capture.Hosted.Sources
	if sources[0].CommentID != sources[1].CommentID || sources[0].SourceKey == sources[1].SourceKey || sources[2].Kind != "duplicate" || sources[2].CommentID != 0 || sources[2].ReviewID != 200 {
		t.Fatal("atomic finding source identities collapsed", sources)
	}
	files := map[string]string{}
	for _, name := range hostedCapacityArtifacts(t, captures) {
		b, err := os.ReadFile(filepath.Join(captures.root, name))
		if err != nil {
			t.Fatal(err)
		}
		files[name] = string(b)
	}
	if len(files) != 1 {
		t.Fatal("unexpected capture copies", len(files))
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER second_data_fail_projection`); err != nil {
		t.Fatal(err)
	}
	reader.failure = errors.New("controlled provider outage after final evidence")
	calls := len(reader.calls)
	restarted, err := reviewexec.New(f.store, []reviewexec.Capability{{ID: "runtime-capability", Provider: "coderabbit", Account: "runtime-account", Channels: []string{"cli", "hosted"}}}, reviewexec.WithNativeProjection())
	if err != nil {
		t.Fatal(err)
	}
	f.engine = restarted
	if err := f.engine.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	final, err := RecoverHosted(ctx, reader, captures, f.engine, f.project, attempt.ID, "runtime-capability", reader.record.PostingActor, reader.record.ProviderActor)
	if err != nil || final.State != "completed" || final.EvidenceID != first.EvidenceID || len(reader.calls) != calls || recoveryRuntimeCount(t, f, "review_execution_reservations") != 0 {
		t.Fatal("offline recovery lost evidence, recollected source or retained reservation", final, err)
	}
	after, err := f.engine.ReadReviewEvidence(ctx, f.scopeWithAccess(t, f.reader, false, "read"), final.EvidenceID)
	if err != nil || !reflect.DeepEqual(after, evidence) {
		t.Fatal("recovery altered captured source", err)
	}
	adjudication, err := f.engine.ReadReviewAdjudication(ctx, f.scope(t, f.writer, false), final.EvidenceID)
	if err != nil || adjudication.Finalized {
		t.Fatal("execution invented finding adjudication", err)
	}
	for name, old := range files {
		b, err := os.ReadFile(filepath.Join(captures.root, name))
		if err != nil || string(b) != old {
			t.Fatal("recovery replaced raw capture", err)
		}
	}
	if len(hostedCapacityArtifacts(t, captures)) != len(files) || recoveryRuntimeCount(t, f, "review_execution_evidence") != 1 {
		t.Fatal("offline recovery duplicated capture/evidence")
	}
}
