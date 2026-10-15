package reviewexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func evidenceFixture(t *testing.T, channel string, admit bool) (*attemptFixture, Attempt, CLIEvidenceInput) {
	t.Helper()
	f := newAttemptFixture(t)
	if err := EnsureEvidenceSchema(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	f.configure(t, attemptProject, "test-capability", 0, false)
	a := f.start(t, attemptProject, f.enqueue(t, attemptProject, "capture-operation", channel, 2, 1))
	anchor := attemptAnchor(attemptProject, 2)
	if admit {
		intent := ""
		if channel == "hosted" {
			intent = "test-hosted-intent"
		}
		var err error
		a, err = f.e.Admit(context.Background(), attemptProject, a.ID, a.Revision, anchor, intent)
		if err != nil {
			t.Fatal(err)
		}
	}
	digest := strings.Repeat("1", 64)
	in := CLIEvidenceInput{CaptureID: "review_capture_" + digest[:32], CaptureSHA256: digest, CaptureBytes: 1024,
		StartedAt: time.Now().Add(-2 * time.Second).UTC().Format(time.RFC3339Nano), FinishedAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), ExpectedAnchor: anchor,
		Review: reviewprovider.CLIReview{Outcome: "completed", Complete: true, ReportedFindings: 1, ReviewedFiles: []string{"internal/changed.go"}, Findings: []reviewprovider.CLIFinding{{Title: "Plain finding", Detail: "Literal <script>source text</script>\nwith details", Severity: "high", Path: "outside/diff.go", StartLine: 8, EndLine: 12}}}}
	return f, a, in
}

func evidenceReadScope(t *testing.T, f *attemptFixture, project, token string) core.Scope {
	t.Helper()
	r := httptest.NewRequest("GET", "/", nil)
	r.SetPathValue("project_id", project)
	r.Header.Set("Authorization", "Bearer "+token)
	scope, err := f.store.Scope(r, "read")
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

func evidenceClone(in CLIEvidenceInput) CLIEvidenceInput {
	var out CLIEvidenceInput
	if err := json.Unmarshal([]byte(encoded(in)), &out); err != nil {
		panic(err)
	}
	return out
}

func evidenceState(t *testing.T, f *attemptFixture, a Attempt) string {
	t.Helper()
	var evidence, events string
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(body_json||fingerprint,''),'') FROM review_execution_evidence`).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if err := f.store.DB().QueryRow(`SELECT coalesce(group_concat(sequence||payload_json,''),'') FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	return attemptState(t, f, a) + "/" + evidence + "/" + events + fmt.Sprint(attemptCount(t, f.store, `SELECT count(*) FROM receipts`))
}

func TestCLIEvidenceRoundtripReplayAndOriginalIdentity(t *testing.T) {
	f, a, in := evidenceFixture(t, "cli", true)
	ctx := context.Background()
	before := attemptState(t, f, a)
	receipts := attemptCount(t, f.store, `SELECT count(*) FROM receipts`)
	var originalFingerprint string
	if err := f.store.DB().QueryRow(`SELECT capability_fingerprint FROM review_operations WHERE id=?`, a.OperationID).Scan(&originalFingerprint); err != nil {
		t.Fatal(err)
	}
	// A new runtime configuration cannot relabel a previously admitted capture.
	var err error
	f.e, err = New(f.store, []Capability{{ID: "test-capability", Provider: "new-provider", Account: "new-account", Channels: []string{"hosted"}, Fingerprint: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	original := evidenceClone(in)
	r, err := f.e.RecordCLIEvidence(ctx, attemptProject, a.ID, in)
	var compact map[string]any
	if err != nil || r.ID == "" || r.State != "captured" || r.Revision != 1 || json.Unmarshal([]byte(encoded(r)), &compact) != nil || len(compact) != 3 {
		t.Fatal("capture receipt was not compact", r, err)
	}
	in.Review.Findings[0].Detail = "caller changed its slice"
	in.Review.ReviewedFiles[0] = "changed.go"
	scope := evidenceReadScope(t, f, attemptProject, f.reader)
	got, err := f.e.ReadCLIEvidence(ctx, scope, r.ID)
	if err != nil || !reflect.DeepEqual(got.Capture, original) || got.Anchor != *a.Anchor || got.CapabilityFingerprint != originalFingerprint || got.ProjectID != attemptProject || got.AttemptID != a.ID || got.OperationID != a.OperationID || got.RecordedAt == "" {
		t.Fatal("capture did not retain original owned input and admission", got, err)
	}
	if before != attemptState(t, f, a) || receipts != attemptCount(t, f.store, `SELECT count(*) FROM receipts`) {
		t.Fatal("capture settled journal, released reservation or allocated a user receipt")
	}
	var actor, payload string
	if err := f.store.DB().QueryRow(`SELECT actor_id,payload_json FROM events WHERE event_type='review.captured' AND resource_id=?`, r.ID).Scan(&actor, &payload); err != nil || actor != "exec-writer-principal" || strings.Contains(payload, "source text") || !strings.Contains(payload, a.ID) {
		t.Fatal("capture event missing retained attribution or exposed finding content", err)
	}
	if _, err := f.e.Advance(ctx, attemptProject, a.ID, a.Revision, "completed", "capture_retained", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-writer-cred'`); err != nil {
		t.Fatal(err)
	}
	settled := evidenceState(t, f, a)
	replay, err := f.e.RecordCLIEvidence(ctx, attemptProject, a.ID, original)
	if err != nil || replay != r || evidenceState(t, f, a) != settled {
		t.Fatal("exact retry after settlement/revocation did not retain receipt", err)
	}
	for name, change := range map[string]func(*CLIEvidenceInput){
		"digest": func(x *CLIEvidenceInput) {
			x.CaptureSHA256 = strings.Repeat("2", 64)
			x.CaptureID = "review_capture_" + x.CaptureSHA256[:32]
		},
		"findings":        func(x *CLIEvidenceInput) { x.Review.Findings[0].Title = "Different title" },
		"text":            func(x *CLIEvidenceInput) { x.Review.Findings[0].Detail = "Different detail" },
		"time":            func(x *CLIEvidenceInput) { x.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano) },
		"expected_anchor": func(x *CLIEvidenceInput) { x.ExpectedAnchor.PatchSHA256 = strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			x := evidenceClone(original)
			change(&x)
			_, err := f.e.RecordCLIEvidence(ctx, attemptProject, a.ID, x)
			attemptCode(t, err, "review_conflict")
			if evidenceState(t, f, a) != settled {
				t.Fatal("changed retry mutated canonical capture or journal")
			}
		})
	}
}

func TestCLIEvidenceConcurrentCaptureAndAtomicEventFailure(t *testing.T) {
	f, a, in := evidenceFixture(t, "cli", true)
	before := attemptState(t, f, a)
	if _, err := f.store.DB().Exec(`CREATE TRIGGER reject_capture_event BEFORE INSERT ON events WHEN NEW.event_type='review.captured' BEGIN SELECT RAISE(ABORT,'synthetic event failure'); END`); err != nil {
		t.Fatal(err)
	}
	r, err := f.e.RecordCLIEvidence(context.Background(), attemptProject, a.ID, in)
	if err == nil || r != (Receipt{}) || attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 0 || before != attemptState(t, f, a) || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.captured'`) != 0 {
		t.Fatal("event failure did not atomically roll back capture", err)
	}
	if strings.Contains(err.Error(), "synthetic") {
		t.Fatal("storage failure leaked SQL diagnostics")
	}
	if _, err := f.store.DB().Exec(`DROP TRIGGER reject_capture_event`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		r   Receipt
		err error
	}
	results := make(chan result, 6)
	start := make(chan struct{})
	for i := 0; i < 6; i++ {
		go func() {
			<-start
			r, err := f.e.RecordCLIEvidence(context.Background(), attemptProject, a.ID, in)
			results <- result{r, err}
		}()
	}
	close(start)
	var first Receipt
	for i := 0; i < 6; i++ {
		got := <-results
		if got.err != nil || first.ID != "" && first != got.r {
			t.Fatal("duplicate capture failed or changed identity", got.err)
		}
		first = got.r
	}
	if attemptCount(t, f.store, `SELECT count(*) FROM review_execution_evidence`) != 1 || attemptCount(t, f.store, `SELECT count(*) FROM events WHERE event_type='review.captured'`) != 1 || before != attemptState(t, f, a) {
		t.Fatal("concurrent duplicate capture duplicated effects or settled review")
	}
}

func TestCLIEvidenceRejectsWrongAnchorScopeAndJournalState(t *testing.T) {
	for _, field := range []string{"head", "patch", "observed", "repository", "repository_id", "pull", "base", "parent", "parent_pull", "base_ref", "merge_base"} {
		t.Run("expected_"+field, func(t *testing.T) {
			f, a, in := evidenceFixture(t, "cli", true)
			switch field {
			case "head":
				in.ExpectedAnchor.CandidateSHA = strings.Repeat("f", 40)
			case "patch":
				in.ExpectedAnchor.PatchSHA256 = strings.Repeat("f", 64)
			case "observed":
				in.ExpectedAnchor.ObservedAt = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
			case "repository":
				in.ExpectedAnchor.Repository = "other/repo"
			case "repository_id":
				in.ExpectedAnchor.RepositoryID = 802
			case "pull":
				in.ExpectedAnchor.PullNumber = 4
			case "base":
				in.ExpectedAnchor.BaseSHA = strings.Repeat("f", 40)
			case "parent":
				in.ExpectedAnchor.ParentSHA = strings.Repeat("f", 40)
			case "parent_pull":
				in.ExpectedAnchor.ParentPullNumber = 1
			case "base_ref":
				in.ExpectedAnchor.BaseRef = "other-branch"
			case "merge_base":
				in.ExpectedAnchor.MergeBaseSHA = strings.Repeat("f", 40)
			}
			before := evidenceState(t, f, a)
			_, err := f.e.RecordCLIEvidence(context.Background(), attemptProject, a.ID, in)
			attemptCode(t, err, "review_conflict")
			if evidenceState(t, f, a) != before {
				t.Fatal("wrong expected anchor mutated state")
			}
		})
	}
	for _, name := range []string{"preparing", "hosted", "terminal", "missing_anchor", "db_repository", "db_pull", "db_head", "other_project", "missing_attempt"} {
		t.Run(name, func(t *testing.T) {
			channel := "cli"
			if name == "hosted" {
				channel = "hosted"
			}
			f, a, in := evidenceFixture(t, channel, name != "preparing")
			project, id, code := attemptProject, a.ID, "review_conflict"
			var err error
			switch name {
			case "terminal":
				_, err = f.e.Advance(context.Background(), project, a.ID, a.Revision, "failed", "test_failed", "")
			case "missing_anchor":
				_, err = f.store.DB().Exec(`UPDATE review_execution_attempts SET anchor_json='null' WHERE id=?`, a.ID)
			case "db_repository":
				_, err = f.store.DB().Exec(`UPDATE projects SET github_repository_id=999 WHERE id=?`, project)
			case "db_pull":
				_, err = f.store.DB().Exec(`UPDATE review_operations SET pull_number=4 WHERE id=?`, a.OperationID)
			case "db_head":
				_, err = f.store.DB().Exec(`UPDATE review_operations SET head_sha=? WHERE id=?`, strings.Repeat("f", 40), a.OperationID)
			case "other_project":
				project, code = otherAttemptProject, "not_found"
			case "missing_attempt":
				id, code = "review_attempt_missing", "not_found"
			}
			if err != nil {
				t.Fatal(err)
			}
			before := evidenceState(t, f, a)
			_, err = f.e.RecordCLIEvidence(context.Background(), project, id, in)
			attemptCode(t, err, code)
			if evidenceState(t, f, a) != before || strings.Contains(err.Error(), a.ID) {
				t.Fatal("refusal changed state or exposed identity")
			}
		})
	}
}

func TestCLIEvidenceIncompleteRetentionAndUncertainAttempt(t *testing.T) {
	for _, outcome := range []string{"failed", "cancelled", "rate_limited", "uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			f, a, in := evidenceFixture(t, "cli", true)
			var err error
			a, err = f.e.Advance(context.Background(), attemptProject, a.ID, a.Revision, "uncertain", "capture_interrupted", "")
			if err != nil {
				t.Fatal(err)
			}
			in.Review.Outcome, in.Review.Complete, in.Review.ReportedFindings = outcome, false, 0
			in.Review.ReviewedFiles = nil
			in.Review.Findings[0].Path = "" // A summary-only partial finding has no file location.
			before := attemptState(t, f, a)
			r, err := f.e.RecordCLIEvidence(context.Background(), attemptProject, a.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.e.ReadCLIEvidence(context.Background(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID)
			if err != nil || got.Capture.Review.Complete || got.Capture.Review.Outcome != outcome || len(got.Capture.Review.Findings) != 1 || before != attemptState(t, f, a) {
				t.Fatal("partial capture fabricated completion or changed reservation", err)
			}
		})
	}
}

func TestCLIEvidenceReaderAuthorizationAndRestartRetention(t *testing.T) {
	f, a, in := evidenceFixture(t, "cli", true)
	r, err := f.e.RecordCLIEvidence(context.Background(), attemptProject, a.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	readScope := evidenceReadScope(t, f, attemptProject, f.reader)
	first, err := f.e.ReadCLIEvidence(context.Background(), readScope, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.e.ReadCLIEvidence(context.Background(), evidenceReadScope(t, f, otherAttemptProject, f.other), r.ID)
	attemptCode(t, err, "not_found")
	wrongProject := readScope
	wrongProject.ProjectID = otherAttemptProject
	_, err = f.e.ReadCLIEvidence(context.Background(), wrongProject, r.ID)
	attemptCode(t, err, "forbidden")
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE id='exec-reader-cred'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.e.ReadCLIEvidence(context.Background(), readScope, r.ID)
	attemptCode(t, err, "unauthorized")
	if _, err := f.store.DB().Exec(`UPDATE credentials SET revoked=0 WHERE id='exec-reader-cred'`); err != nil {
		t.Fatal(err)
	}
	var sequence int
	var databaseName, databasePath string
	if err := f.store.DB().QueryRow(`PRAGMA database_list`).Scan(&sequence, &databaseName, &databasePath); err != nil {
		t.Fatal(err)
	}
	instance := f.store.InstanceID()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := core.Open(context.Background(), filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	for i := 0; i < 2; i++ {
		if err := EnsureSchema(context.Background(), reopened); err != nil {
			t.Fatal(err)
		}
		if err := EnsureEvidenceSchema(context.Background(), reopened); err != nil {
			t.Fatal(err)
		}
	}
	f.e, err = New(reopened, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := f.e.ReadCLIEvidence(context.Background(), evidenceReadScope(t, f, attemptProject, f.reader), r.ID)
	if err != nil || !reflect.DeepEqual(first, second) || reopened.InstanceID() != instance {
		t.Fatal("restart altered retained capture", err)
	}
	replay, err := f.e.RecordCLIEvidence(context.Background(), attemptProject, a.ID, in)
	if err != nil || replay != r || attemptCount(t, reopened, `SELECT count(*) FROM review_execution_evidence`) != 1 || attemptCount(t, reopened, `SELECT count(*) FROM events WHERE event_type='review.captured'`) != 1 {
		t.Fatal("restart replay duplicated capture", err)
	}
}

func TestCLIEvidenceValidationIsBoundedAndAtomic(t *testing.T) {
	f, a, in := evidenceFixture(t, "cli", true)
	cases := map[string]func(*CLIEvidenceInput){
		"capture_id":           func(x *CLIEvidenceInput) { x.CaptureID += "x" },
		"capture_hash":         func(x *CLIEvidenceInput) { x.CaptureSHA256 = strings.Repeat("A", 64) },
		"zero_bytes":           func(x *CLIEvidenceInput) { x.CaptureBytes = 0 },
		"large_bytes":          func(x *CLIEvidenceInput) { x.CaptureBytes = 24<<20 + 1 },
		"start":                func(x *CLIEvidenceInput) { x.StartedAt = "not-a-time" },
		"zero_time":            func(x *CLIEvidenceInput) { x.StartedAt = time.Time{}.Format(time.RFC3339Nano) },
		"end":                  func(x *CLIEvidenceInput) { x.FinishedAt = "not-a-time" },
		"reversed_time":        func(x *CLIEvidenceInput) { x.StartedAt = time.Now().UTC().Format(time.RFC3339Nano) },
		"future":               func(x *CLIEvidenceInput) { x.FinishedAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano) },
		"outcome":              func(x *CLIEvidenceInput) { x.Review.Outcome = "success" },
		"incomplete_completed": func(x *CLIEvidenceInput) { x.Review.Complete = false },
		"complete_failed":      func(x *CLIEvidenceInput) { x.Review.Outcome = "failed" },
		"negative_count":       func(x *CLIEvidenceInput) { x.Review.ReportedFindings = -1 },
		"mismatched_count":     func(x *CLIEvidenceInput) { x.Review.ReportedFindings = 2 },
		"empty_files":          func(x *CLIEvidenceInput) { x.Review.ReviewedFiles = nil },
		"duplicate_files":      func(x *CLIEvidenceInput) { x.Review.ReviewedFiles = []string{"a.go", "a.go"} },
		"too_many_files":       func(x *CLIEvidenceInput) { x.Review.ReviewedFiles = make([]string, 10001) },
		"too_many_findings":    func(x *CLIEvidenceInput) { x.Review.Findings = make([]reviewprovider.CLIFinding, 101) },
		"title_empty":          func(x *CLIEvidenceInput) { x.Review.Findings[0].Title = " \n" },
		"title_large":          func(x *CLIEvidenceInput) { x.Review.Findings[0].Title = strings.Repeat("é", 251) },
		"title_utf8":           func(x *CLIEvidenceInput) { x.Review.Findings[0].Title = "raw\xff" },
		"detail_large":         func(x *CLIEvidenceInput) { x.Review.Findings[0].Detail = strings.Repeat("x", 16<<10+1) },
		"detail_utf8":          func(x *CLIEvidenceInput) { x.Review.Findings[0].Detail = "raw\xff" },
		"detail_control":       func(x *CLIEvidenceInput) { x.Review.Findings[0].Detail = "raw\x00" },
		"severity":             func(x *CLIEvidenceInput) { x.Review.Findings[0].Severity = "urgent" },
		"negative_start":       func(x *CLIEvidenceInput) { x.Review.Findings[0].StartLine = -1 },
		"negative_end":         func(x *CLIEvidenceInput) { x.Review.Findings[0].EndLine = -1 },
		"reversed_lines":       func(x *CLIEvidenceInput) { x.Review.Findings[0].EndLine = 1 },
		"anchor_utf8":          func(x *CLIEvidenceInput) { x.ExpectedAnchor.BaseRef = "raw\xff" },
		"total_bytes": func(x *CLIEvidenceInput) {
			x.Review.Findings = make([]reviewprovider.CLIFinding, 100)
			for i := range x.Review.Findings {
				x.Review.Findings[i] = reviewprovider.CLIFinding{Title: "Finding", Severity: "unknown", Detail: strings.Repeat("x", 16<<10)}
			}
			x.Review.ReportedFindings = 100
			x.Review.ReviewedFiles = make([]string, 10000)
			for i := range x.Review.ReviewedFiles {
				x.Review.ReviewedFiles[i] = fmt.Sprintf("source/%05d/%s.go", i, strings.Repeat("x", 80))
			}
		},
	}
	for _, path := range []string{"", "/tmp/local.go", "../escape.go", "source/../escape.go", "source/./file.go", "source//file.go", "C:/local.go", `C:\local.go`, "source\nfile.go", "source\x00file.go", "source\x7ffile.go", "source\xfffile.go"} {
		p := path
		cases["reviewed_path_"+fmt.Sprintf("%x", []byte(path))] = func(x *CLIEvidenceInput) { x.Review.ReviewedFiles = []string{p} }
		if path != "" {
			cases["finding_path_"+fmt.Sprintf("%x", []byte(path))] = func(x *CLIEvidenceInput) { x.Review.Findings[0].Path = p }
		}
	}
	before := evidenceState(t, f, a)
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			x := evidenceClone(in)
			change(&x)
			if name == "total_bytes" {
				if err := validateCLIEvidence(x); err != nil || len(encoded(x)) <= evidenceInputLimit {
					t.Fatal("oversize fixture did not isolate the aggregate bound", err)
				}
			}
			r, err := f.e.RecordCLIEvidence(context.Background(), attemptProject, a.ID, x)
			attemptCode(t, err, "validation_failed")
			if r != (Receipt{}) || evidenceState(t, f, a) != before || strings.Contains(err.Error(), "raw") {
				t.Fatal("invalid capture mutated state or exposed input")
			}
		})
	}
	for _, ids := range [][2]string{{"bad/project", a.ID}, {attemptProject, "bad/attempt"}} {
		_, err := f.e.RecordCLIEvidence(context.Background(), ids[0], ids[1], in)
		attemptCode(t, err, "validation_failed")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := f.e.RecordCLIEvidence(cancelled, attemptProject, a.ID, in); err == nil || r != (Receipt{}) || evidenceState(t, f, a) != before {
		t.Fatal("cancelled capture left a partial write", err)
	}
	// Full boundary-sized fields, offset times and all supported severities remain valid.
	in.CaptureBytes = 24 << 20
	finishedAt, err := time.Parse(time.RFC3339Nano, in.FinishedAt)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the interval valid even when the preceding rejection cases take
	// longer than the fixture's original one-second interval.
	in.StartedAt = finishedAt.Add(-time.Second).In(time.FixedZone("offset", 12*3600)).Format(time.RFC3339Nano)
	in.Review.Findings[0].Title = strings.Repeat("x", 500)
	in.Review.Findings[0].Detail = strings.Repeat("x", 16<<10)
	for _, severity := range []string{"info", "low", "medium", "high", "critical", "unknown"} {
		in.Review.Findings[0].Severity = severity
		if err := validateCLIEvidence(in); err != nil {
			t.Fatal("valid boundary refused", severity, err)
		}
	}
	if _, err := f.e.RecordCLIEvidence(context.Background(), attemptProject, a.ID, in); err != nil {
		t.Fatal("valid boundary capture refused", err)
	}
}
