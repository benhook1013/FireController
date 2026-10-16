package reviewruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func hostedCaptureFixture(t *testing.T) (string, HostedCaptureRecord) {
	t.Helper()
	start := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	attempt := "review_attempt_" + strings.Repeat("a", 32)
	intent := reviewprovider.HostedTriggerIntent{AttemptID: attempt, OperationID: "review_operation_" + strings.Repeat("b", 32), HeadSHA: strings.Repeat("c", 40)}
	poster := reviewprovider.Author{ID: 10, Login: "poster", Kind: "User"}
	bot := reviewprovider.Author{ID: 20, Login: "coderabbitai", Kind: "Bot"}
	body, err := reviewprovider.HostedTriggerBody(intent)
	if err != nil {
		t.Fatal(err)
	}
	line := int64(7)
	r := HostedCaptureRecord{RepositoryID: 901, Repository: "owner/repo", PullNumber: 2, Intent: intent, PostingActor: poster, ProviderActor: bot, EarliestPosting: start.Add(-time.Minute), StartedAt: start, FinishedAt: start.Add(4 * time.Second)}
	r.Snapshot = reviewprovider.HostedPublicationSnapshot{
		Complete: true, ObservedAt: start.Add(3 * time.Second),
		IssueComments:  []reviewprovider.IssueComment{{ID: 100, Body: body, Author: poster, CreatedAt: start, UpdatedAt: start}},
		Reviews:        []reviewprovider.HostedReview{{ID: 200, Body: "**Actionable comments posted: 1**\nOutside-diff <script>literal & text</script>\n<!-- coderabbit-review-publication v1 publication=11111111-1111-1111-1111-111111111111 attempt=22222222-2222-2222-2222-222222222222 batch=1/1 -->", State: "commented", HeadSHA: intent.HeadSHA, Author: bot, SubmittedAt: start.Add(time.Second)}},
		ReviewComments: []reviewprovider.ReviewComment{{ID: 300, ReviewID: 200, Body: "Exact root\n\t`<&>` synthetic-private-token", Author: bot, Path: "source.go", Line: &line, HeadSHA: intent.HeadSHA, OriginalHeadSHA: intent.HeadSHA, CreatedAt: start.Add(time.Second), UpdatedAt: start.Add(time.Second)}},
	}
	return attempt, r
}

func newHostedTestStore(t *testing.T) (*HostedCaptureStore, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "private-capture-path")
	s, err := NewHostedCaptureStore(root)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func TestHostedCaptureRetainsExactObservationsAndReferences(t *testing.T) {
	ctx := context.Background()
	id, record := hostedCaptureFixture(t)
	s, root := newHostedTestStore(t)
	if strings.Contains(fmt.Sprintf("%v %#v", s, s), root) {
		t.Fatal("formatted store exposes private path")
	}
	ref, err := s.Save(ctx, id, record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, hostedCaptureFilename(id, ref))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	if ref.SHA256 != hex.EncodeToString(digest[:]) || ref.Bytes != int64(len(data)) || ref.ID != "review_capture_"+ref.SHA256[:32] || !bytes.Contains(data, []byte("<script>literal & text</script>")) || bytes.Contains(data, []byte(`\u003c`)) {
		t.Fatal("reference or literal source body changed")
	}
	before, _ := os.Stat(path)
	again, err := s.Save(ctx, id, record)
	after, _ := os.Stat(path)
	if err != nil || again != ref || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("replay replaced original observation")
	}
	reopened, err := NewHostedCaptureStore(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopened.Read(ctx, id, ref)
	if err != nil || !reflect.DeepEqual(loaded, record) {
		t.Fatalf("reopen lost precise record: %v", err)
	}
	*loaded.Snapshot.ReviewComments[0].Line = 99
	loaded.Snapshot.Reviews[0].Body = "caller replacement"
	readAgain, err := reopened.Read(ctx, id, ref)
	if err != nil || !reflect.DeepEqual(readAgain, record) {
		t.Fatal("returned pointers changed immutable storage")
	}
	record.FinishedAt = record.FinishedAt.Add(time.Second)
	record.Snapshot.ObservedAt = record.Snapshot.ObservedAt.Add(time.Second)
	record.Snapshot.Reviews[0].Body += "\nNew observation metadata"
	newRef, err := reopened.Save(ctx, id, record)
	if err != nil || newRef == ref {
		t.Fatal("different observation was not independently retained")
	}
	if _, err := reopened.Read(ctx, id, ref); err != nil {
		t.Fatal("new observation invalidated old reference")
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 2 {
		t.Fatal("unexpected files or leaked temp artifacts")
	}
	if runtime.GOOS != "windows" {
		dir, _ := os.Stat(root)
		if after.Mode().Perm() != 0600 || dir.Mode().Perm() != 0700 {
			t.Fatal("private mode not enforced")
		}
	}
}

func TestHostedCaptureConcurrentNoReplace(t *testing.T) {
	ctx := context.Background()
	id, r := hostedCaptureFixture(t)
	s, root := newHostedTestStore(t)
	type outcome struct {
		ref CaptureReference
		err error
	}
	results := make(chan outcome, 8)
	start := make(chan struct{})
	for i := 0; i < cap(results); i++ {
		go func() { <-start; ref, err := s.Save(ctx, id, r); results <- outcome{ref, err} }()
	}
	close(start)
	var first CaptureReference
	for i := 0; i < cap(results); i++ {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if i == 0 {
			first = result.ref
		} else if first != result.ref {
			t.Fatal("same observation references differ")
		}
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal("concurrent save leaked files")
	}
}

func TestHostedCaptureRetainsValidUnresolvedEvidence(t *testing.T) {
	for _, state := range []string{"active", "uncertain", "ambiguous"} {
		t.Run(state, func(t *testing.T) {
			id, r := hostedCaptureFixture(t)
			s, _ := newHostedTestStore(t)
			switch state {
			case "active":
				r.Snapshot.Reviews = nil
				r.Snapshot.ReviewComments = nil
			case "uncertain":
				r.Snapshot.IssueComments = nil
			case "ambiguous":
				r.Snapshot.Reviews[0].Body += "\n<!-- coderabbit-review-publication malformed -->"
			}
			publication, err := reviewprovider.ResolveHostedPublication(r.Snapshot, r.Intent, r.PostingActor, r.ProviderActor, r.EarliestPosting)
			if err != nil || publication.State != state {
				t.Fatalf("fixture: %s %v", publication.State, err)
			}
			ref, err := s.Save(context.Background(), id, r)
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := s.Read(context.Background(), id, ref)
			if err != nil || !reflect.DeepEqual(loaded, r) {
				t.Fatal("valid unresolved source lost")
			}
		})
	}
}

func TestHostedCaptureRejectsInvalidInputsWithoutPublication(t *testing.T) {
	cases := map[string]func(*HostedCaptureRecord){
		"attempt":                  func(r *HostedCaptureRecord) { r.Intent.AttemptID += "a" },
		"operation":                func(r *HostedCaptureRecord) { r.Intent.OperationID = "bad" },
		"head":                     func(r *HostedCaptureRecord) { r.Intent.HeadSHA = "bad" },
		"repositoryID":             func(r *HostedCaptureRecord) { r.RepositoryID = 0 },
		"repository":               func(r *HostedCaptureRecord) { r.Repository = "owner/../repo" },
		"repositoryDots":           func(r *HostedCaptureRecord) { r.Repository = "../repo" },
		"repositoryBound":          func(r *HostedCaptureRecord) { r.Repository = strings.Repeat("a", 101) + "/repo" },
		"pull":                     func(r *HostedCaptureRecord) { r.PullNumber = 0 },
		"pullBound":                func(r *HostedCaptureRecord) { r.PullNumber = 2147483648 },
		"incomplete":               func(r *HostedCaptureRecord) { r.Snapshot.Complete = false },
		"zeroStart":                func(r *HostedCaptureRecord) { r.StartedAt = time.Time{} },
		"zeroFinish":               func(r *HostedCaptureRecord) { r.FinishedAt = time.Time{} },
		"zeroEarliest":             func(r *HostedCaptureRecord) { r.EarliestPosting = time.Time{} },
		"zeroObserved":             func(r *HostedCaptureRecord) { r.Snapshot.ObservedAt = time.Time{} },
		"offset":                   func(r *HostedCaptureRecord) { r.StartedAt = r.StartedAt.In(time.FixedZone("offset", 3600)) },
		"startRange":               func(r *HostedCaptureRecord) { r.StartedAt = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) },
		"finishRange":              func(r *HostedCaptureRecord) { r.FinishedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
		"startAfterObservation":    func(r *HostedCaptureRecord) { r.StartedAt = r.Snapshot.ObservedAt.Add(time.Nanosecond) },
		"finishBeforeObservation":  func(r *HostedCaptureRecord) { r.FinishedAt = r.Snapshot.ObservedAt.Add(-time.Nanosecond) },
		"earliestAfterObservation": func(r *HostedCaptureRecord) { r.EarliestPosting = r.Snapshot.ObservedAt.Add(time.Nanosecond) },
		"actor":                    func(r *HostedCaptureRecord) { r.ProviderActor.ID = 0 },
		"actorsSame":               func(r *HostedCaptureRecord) { r.ProviderActor = r.PostingActor },
		"body":                     func(r *HostedCaptureRecord) { r.Snapshot.Reviews[0].Body = strings.Repeat("a", (64<<10)+1) },
		"unicode":                  func(r *HostedCaptureRecord) { r.Snapshot.Reviews[0].Body = string([]byte{255}) },
		"control":                  func(r *HostedCaptureRecord) { r.Snapshot.Reviews[0].Body = "bad\x00" },
		"duplicateID":              func(r *HostedCaptureRecord) { r.Snapshot.Reviews = append(r.Snapshot.Reviews, r.Snapshot.Reviews[0]) },
		"badLine":                  func(r *HostedCaptureRecord) { *r.Snapshot.ReviewComments[0].Line = -1 },
		"futureReview":             func(r *HostedCaptureRecord) { r.Snapshot.Reviews[0].SubmittedAt = r.FinishedAt.Add(time.Second) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			id, r := hostedCaptureFixture(t)
			change(&r)
			s, root := newHostedTestStore(t)
			_, err := s.Save(context.Background(), id, r)
			requireCaptureError(t, err, "validation_failed")
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("invalid input published bytes")
			}
		})
	}
	id, r := hostedCaptureFixture(t)
	r.StartedAt = r.Snapshot.ObservedAt
	r.FinishedAt = r.Snapshot.ObservedAt
	r.EarliestPosting = r.Snapshot.ObservedAt
	s, _ := newHostedTestStore(t)
	if _, err := s.Save(context.Background(), id, r); err != nil {
		t.Fatal("inclusive timing boundary rejected", err)
	}
}

// Re-hash intentionally corrupt fixture bytes to exercise envelope validation
// independently of the expected-reference checksum guard.
func hostedWriteFixture(t *testing.T, root, id string, data []byte) CaptureReference {
	t.Helper()
	ref := captureReference(data)
	if err := os.WriteFile(filepath.Join(root, hostedCaptureFilename(id, ref)), data, 0600); err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestHostedCaptureCorruptionAndStrictEnvelope(t *testing.T) {
	id, r := hostedCaptureFixture(t)
	data, err := encodeHostedCapture(id, r)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"unknownVersion": bytes.Replace(data, []byte(`"version":1`), []byte(`"version":2`), 1),
		"nullStart":      bytes.Replace(data, []byte(`"started_at":"`+r.StartedAt.Format(time.RFC3339Nano)+`"`), []byte(`"started_at":null`), 1),
		"missingStart":   bytes.Replace(data, []byte(`"started_at":"`+r.StartedAt.Format(time.RFC3339Nano)+`",`), nil, 1),
		"offsetTime":     bytes.Replace(data, []byte(r.StartedAt.Format(time.RFC3339Nano)), []byte(r.StartedAt.Format("2006-01-02T15:04:05.999999999-07:00")), 1),
		"duplicate":      bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"unknown":        bytes.Replace(data, []byte(`"version":1`), []byte(`"version":1,"unknown":true`), 1),
		"case":           bytes.Replace(data, []byte(`"version":1`), []byte(`"Version":1`), 1),
		"nullBody":       bytes.Replace(data, []byte(`"Body":"`+strings.ReplaceAll(r.Snapshot.Reviews[0].Body, "\n", `\n`)+`"`), []byte(`"Body":null`), 1),
		"attempt":        bytes.Replace(data, []byte(`"attempt_id":"`+id+`"`), []byte(`"attempt_id":"review_attempt_`+strings.Repeat("d", 32)+`"`), 1),
		"trailing":       append(append([]byte{}, data...), []byte("{}")...),
		"malformed":      []byte(`{"version":1`),
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(corrupt, data) {
				t.Fatal("fixture did not change bytes")
			}
			s, root := newHostedTestStore(t)
			ref := hostedWriteFixture(t, root, id, corrupt)
			_, err := s.Read(context.Background(), id, ref)
			requireCaptureError(t, err, "capture_unavailable")
			after, _ := os.ReadFile(filepath.Join(root, hostedCaptureFilename(id, ref)))
			if !bytes.Equal(corrupt, after) {
				t.Fatal("corrupt evidence rewritten")
			}
		})
	}
	s, root := newHostedTestStore(t)
	ref, err := s.Save(context.Background(), id, r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, hostedCaptureFilename(id, ref))
	corrupt := append([]byte{}, data...)
	corrupt[len(corrupt)-2] = '!'
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = s.Read(context.Background(), id, ref)
	requireCaptureError(t, err, "capture_unavailable")
	_, err = s.Save(context.Background(), id, r)
	requireCaptureError(t, err, "capture_unavailable")
	after, _ := os.ReadFile(path)
	if !bytes.Equal(corrupt, after) {
		t.Fatal("retry replaced conflicting retained bytes")
	}
}

func TestHostedCaptureReferenceIsolationCancellationAndPrivateFiles(t *testing.T) {
	ctx := context.Background()
	id, r := hostedCaptureFixture(t)
	s, root := newHostedTestStore(t)
	ref, err := s.Save(ctx, id, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, alter := range []func(*CaptureReference){
		func(r *CaptureReference) { r.ID = "wrong" }, func(r *CaptureReference) { r.Bytes++ },
		func(r *CaptureReference) { r.SHA256 = "../escape" }, func(r *CaptureReference) { r.Bytes = maxHostedCaptureFile + 1 },
		func(r *CaptureReference) { r.SHA256 = strings.ToUpper(r.SHA256) },
	} {
		bad := ref
		alter(&bad)
		if _, err := s.Read(ctx, id, bad); err == nil {
			t.Fatal("mismatched reference accepted")
		}
	}
	otherID := "review_attempt_" + strings.Repeat("f", 32)
	_, err = s.Read(ctx, otherID, ref)
	requireCaptureError(t, err, "capture_unavailable")
	// Even authentic bytes copied under another attempt's filename cannot be
	// adopted through a valid digest/reference for the original attempt.
	data, err := os.ReadFile(filepath.Join(root, hostedCaptureFilename(id, ref)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, hostedCaptureFilename(otherID, ref)), data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = s.Read(ctx, otherID, ref)
	requireCaptureError(t, err, "capture_unavailable")
	_, err = s.Read(ctx, "../escape", ref)
	requireCaptureError(t, err, "validation_failed")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.Save(cancelled, id, r)
	requireCaptureError(t, err, "capture_unavailable")
	_, err = s.Read(cancelled, id, ref)
	requireCaptureError(t, err, "capture_unavailable")
	_, err = s.Save(nil, id, r)
	requireCaptureError(t, err, "capture_unavailable")
	_, err = s.Read(nil, id, ref)
	requireCaptureError(t, err, "capture_unavailable")
	if runtime.GOOS == "windows" {
		return
	}
	path := filepath.Join(root, hostedCaptureFilename(id, ref))
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	_, err = s.Read(ctx, id, ref)
	requireCaptureError(t, err, "capture_unavailable")
	_, err = s.Save(ctx, id, r)
	requireCaptureError(t, err, "capture_unavailable")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	_, err = s.Read(ctx, id, ref)
	requireCaptureError(t, err, "capture_unavailable")
	_, err = NewHostedCaptureStore(root)
	requireCaptureError(t, err, "configuration_invalid")
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	// A symlink to a valid private capture is still forbidden.
	aliasID := "review_attempt_" + strings.Repeat("e", 32)
	if err := os.Symlink(path, filepath.Join(root, hostedCaptureFilename(aliasID, ref))); err != nil {
		t.Fatal(err)
	}
	_, err = s.Read(ctx, aliasID, ref)
	requireCaptureError(t, err, "capture_unavailable")
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	_, err = NewHostedCaptureStore(filepath.Join(link, "child"))
	requireCaptureError(t, err, "configuration_invalid")
	for _, directory := range []bool{false, true} {
		blockedStore, blockedRoot := newHostedTestStore(t)
		blocked := filepath.Join(blockedRoot, hostedCaptureFilename(id, ref))
		if directory {
			err = os.Mkdir(blocked, 0700)
		} else {
			err = os.Symlink(path, blocked)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, err = blockedStore.Save(ctx, id, r)
		requireCaptureError(t, err, "capture_unavailable")
		_, err = blockedStore.Read(ctx, id, ref)
		requireCaptureError(t, err, "capture_unavailable")
	}
}

func TestHostedCaptureEncodedAndObservationBudgets(t *testing.T) {
	id, r := hostedCaptureFixture(t)
	s, root := newHostedTestStore(t)
	r.Snapshot.IssueComments = nil
	r.Snapshot.ReviewComments = nil
	r.Snapshot.Reviews = make([]reviewprovider.HostedReview, 257)
	for i := range r.Snapshot.Reviews {
		r.Snapshot.Reviews[i] = reviewprovider.HostedReview{ID: int64(i + 1), Body: strings.Repeat("x", 64<<10), State: "commented", HeadSHA: r.Intent.HeadSHA, Author: r.ProviderActor, SubmittedAt: r.StartedAt}
	}
	_, err := s.Save(context.Background(), id, r)
	requireCaptureError(t, err, "validation_failed")
	r.Snapshot.Reviews = make([]reviewprovider.HostedReview, 10001)
	_, err = s.Save(context.Background(), id, r)
	requireCaptureError(t, err, "validation_failed")
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("overbudget observation published")
	}
	// Exercise the encoded writer cap independently: the provider body budget
	// currently bounds realizable JSON below this additional defensive ceiling.
	var writer hostedCaptureBuffer
	block := bytes.Repeat([]byte{'x'}, 1<<20)
	for i := 0; i < 48; i++ {
		if _, err := writer.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Write([]byte{'x'}); err == nil || writer.Len() != maxHostedCaptureFile {
		t.Fatal("encoded file cap not enforced")
	}
	// A sparse oversized artifact must fail before decoding/reading its contents.
	_, r = hostedCaptureFixture(t)
	data, _ := encodeHostedCapture(id, r)
	ref := captureReference(data)
	path := filepath.Join(root, hostedCaptureFilename(id, ref))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxHostedCaptureFile + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = s.Read(context.Background(), id, ref)
	requireCaptureError(t, err, "capture_unavailable")
	// Ensure the public reference itself contains no source bodies or paths.
	encoded, _ := json.Marshal(ref)
	if bytes.Contains(encoded, []byte(root)) || bytes.Contains(encoded, []byte("synthetic-private-token")) {
		t.Fatal("reference exposed private evidence")
	}
}
