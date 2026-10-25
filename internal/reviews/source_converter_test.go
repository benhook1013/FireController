package reviews

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Keep the optional source adapter and the native transfer contract connected.
// Only the Python suite's disposable mocked-source setup is reused here; none
// of its unittest assertions or optional installed/copied-source cases run.
func TestSourceReviewConverterNativeTransfer(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "freebsd", "openbsd", "netbsd", "dragonfly", "illumos", "solaris", "aix":
	default:
		t.Skip("source converter fixture requires POSIX private-file semantics")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("optional source converter regression requires python3")
	}
	python, err = filepath.Abs(python)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := filepath.Abs(filepath.Join("..", "..", "tools", "legacyimport"))
	if err != nil {
		t.Fatal(err)
	}
	owned := t.TempDir()
	output := filepath.Join(owned, "prepared.json")
	const fixture = `import os,pathlib,shutil,subprocess,sys
os.umask(0o077)
sys.dont_write_bytecode=True
sys.path.insert(0,sys.argv[1])
from test_prepare_source_reviews import ReviewsAdapterTests
fixture=ReviewsAdapterTests(methodName='runTest')
try:
 fixture.setUp()
 fixture.selection['repository_id']=701
 fixture.save_selection()
 before=fixture.snapshot.read_bytes()
 result=subprocess.run(fixture.command(),stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=10)
 if result.returncode!=0:raise RuntimeError('synthetic converter refused')
 if fixture.snapshot.read_bytes()!=before:raise RuntimeError('synthetic source changed')
 shutil.copyfile(fixture.output,sys.argv[2])
finally:fixture.doCleanups()
`
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-B", "-c", fixture, tools, output)
	// Allowlist environment: no real source selection, credentials, user Python
	// packages, proxy settings or home configuration reach either child process.
	command.Env = []string{"PATH=" + filepath.Dir(python), "HOME=" + owned, "TMPDIR=" + owned,
		"PYTHONDONTWRITEBYTECODE=1", "PYTHONNOUSERSITE=1", "LC_ALL=C.UTF-8"}
	if err := command.Run(); err != nil {
		t.Fatalf("disposable source converter failed: %T (deadline=%v)", err, ctx.Err() != nil)
	}
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var batch TransferRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&batch); err != nil {
		t.Fatal("converter output no longer matches native transfer fields", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		t.Fatal("converter output has trailing content")
	}
	if batch.Version != 1 || batch.RepositoryID != 701 || batch.Publisher != "migration" ||
		len(batch.Groups) != 1 || batch.Groups[0].Source != "original-controller" || len(batch.Groups[0].Pulls) != 1 {
		t.Fatal("converter changed the explicit transfer identity")
	}
	pull := batch.Groups[0].Pulls[0]
	if pull.Number != 7 || pull.Title != "Selected PR" || pull.State != "merged" || pull.HeadSHA != strings.Repeat("a", 40) || pull.BaseSHA != strings.Repeat("b", 40) ||
		pull.BaseRef != "main" || pull.CIState != "unknown" || pull.Mergeability != "unknown" || pull.Complete || len(pull.Runs) != 1 || len(pull.Attempts) != 0 {
		t.Fatal("converter changed selected metadata, coverage or omissions")
	}
	run := pull.Runs[0]
	if run.SourceRunID != "real-source-run" || run.Channel != "cli" || run.HeadSHA != pull.HeadSHA || run.Reviewer != "" || run.Scope != "diff" ||
		run.Outcome != "completed" || !run.Attributable || !run.Finalized || !run.Complete ||
		run.StartedAt != "2026-10-01T00:00:00Z" || run.FinishedAt != "2026-10-01T01:00:00Z" ||
		run.Found != 1 || run.Accepted != 1 || run.Routed != 0 || len(run.Findings) != 1 {
		t.Fatal("converter lost source run chronology, identity or counts")
	}
	finding := run.Findings[0]
	if finding.SourceFindingID != "source-finding" || finding.Title != "Original <title>" ||
		finding.Detail != "Original\n<script>inert</script> & text" || finding.Severity != "high" ||
		finding.Disposition != "accepted" || finding.Actor != "original-actor" ||
		finding.DecisionReason != "Original reason" || finding.DecidedAt != "2026-10-01T01:00:00Z" ||
		finding.FixSHA != "" || finding.Route != nil {
		t.Fatal("converter changed retained finding identity/text/decision")
	}
	f := newAPIFixture(t)
	preview := transferPreviewFor(t, f, batch) // Checks every table is unchanged.
	if preview.Counts != (TransferCounts{Pulls: 1, Runs: 1, Findings: 1}) {
		t.Fatal("native preview disagrees with converter scope")
	}
	receipt, _, _ := transferApplyFor(t, f, batch, "source-converter")
	var detail RunDetailResponse
	nativeRead(t, f, "/runs/"+runOpaqueID(f.project, "original-controller", run.SourceRunID)+"?snapshot="+receipt.ID, &detail)
	if detail.Run.Source != "original-controller" || detail.Run.SourceRunID != run.SourceRunID || detail.Run.HeadSHA != run.HeadSHA || detail.Run.Channel != run.Channel ||
		detail.Run.StartedAt != run.StartedAt || detail.Run.FinishedAt != run.FinishedAt ||
		detail.Run.Found != run.Found || detail.Run.Accepted != run.Accepted ||
		!reflect.DeepEqual(plainFindingsForTest(detail.Findings), run.Findings) {
		t.Fatal("native apply/readback changed actual converter records")
	}
	var queue QueueResponse
	nativeRead(t, f, "/queue", &queue)
	if len(queue.Items) != 1 || queue.Items[0].Complete || queue.Items[0].AttemptCount != 0 {
		t.Fatal("selected partial history became complete or imported attempts")
	}
}
