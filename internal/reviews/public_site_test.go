package reviews

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/presentationmodel"
	"github.com/benhook1013/FireController/internal/publicsite"
)

func TestPublicSiteActualReviewHooksPreservePrivacyAndHistory(t *testing.T) {
	f := newAPIFixture(t)
	if err := publicsite.EnsureSchema(t.Context(), f.store); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,'private-publication-worker','PRIVATE_WORKER_NAME')`, f.project); err != nil {
		t.Fatal(err)
	}
	site := publicsite.New(f.store, PublicSiteReviewFacts)
	site.Register(f.mux.(*http.ServeMux), t.Context())
	request := httptest.NewRequest("PUT", "/api/v1/projects/"+f.project+"/publication", nil)
	request.SetPathValue("project_id", f.project)
	request.Header.Set("Authorization", "Bearer "+f.admin)
	request.Header.Set("X-FC-Instance", f.store.InstanceID())
	request.Header.Set("X-FC-Worker", "private-publication-worker")
	scope, err := f.store.AdminScope(request)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := publicsite.NormalizeManifest(publicsite.Manifest{Slug: "review-showcase", Presentation: presentationmodel.Config{Name: "Approved public project"}, Reviews: []publicsite.EnrolledReview{{Number: 1, Title: "Approved public PR title", URL: "https://github.com/owner/reviews/pull/1"}}})
	if err != nil {
		t.Fatal(err)
	}
	read := func() (publicsite.Snapshot, []byte) {
		t.Helper()
		response := f.call("GET", "/api/v1/public/projects/review-showcase", "", "", nil)
		var snapshot publicsite.Snapshot
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &snapshot) != nil {
			t.Fatal("anonymous read", response.Code, response.Body.String())
		}
		for _, sentinel := range []string{"PRIVATE_", f.project, f.store.InstanceID(), f.admin, f.readToken, "private-publication-worker", "source_run_id", "snapshot_id", "actor_id", "round_salt", "private_diagnostic_sentinel"} {
			if bytes.Contains(response.Body.Bytes(), []byte(sentinel)) {
				t.Fatal("private review facts exposed", sentinel)
			}
		}
		if len(snapshot.Content.Reviews) != 1 || snapshot.Content.Reviews[0].Title != manifest.Reviews[0].Title || snapshot.Content.Reviews[0].URL != manifest.Reviews[0].URL {
			t.Fatal("approved enrollment changed")
		}
		return snapshot, append([]byte(nil), response.Body.Bytes()...)
	}
	var publicHints <-chan struct{}
	unchanged := func(before []byte) {
		t.Helper()
		_, after := read()
		if !bytes.Equal(before, after) {
			t.Fatal("private-only activity changed anonymous bytes/revision/freshness")
		}
		select {
		case <-publicHints:
			t.Fatal("private-only activity emitted public hint")
		default:
		}
	}
	changedHint := func() {
		t.Helper()
		select {
		case <-publicHints:
		default:
			t.Fatal("changed public projection missing committed hint")
		}
	}
	privateRevision := func() int64 {
		t.Helper()
		var revision int64
		if f.store.DB().QueryRow(`SELECT revision FROM review_workspace WHERE project_id=?`, f.project).Scan(&revision) != nil {
			t.Fatal("private revision")
		}
		return revision
	}

	// The actual administrator observation hook sees source metadata and an
	// unfinalized completed run; only the enrolled PR's lifecycle may publish.
	imported := nativeRun("PRIVATE_IMPORTED_RUN")
	imported.Channel = "hosted"
	imported.Finalized = false
	imported.Reviewer = "PRIVATE_REVIEWER"
	imported.Scope = "PRIVATE_SCOPE"
	imported.Findings[0].Detail = "PRIVATE_FINDING_BODY"
	imported.Findings[0].Actor = "PRIVATE_ACTOR"
	imported.Findings[0].ProofNote = "PRIVATE_FIX_PROOF"
	pull := nativePull(1)
	pull.Title = "PRIVATE_SOURCE_TITLE"
	pull.Runs = []RunInput{imported}
	other := nativePull(2)
	other.Title = "PRIVATE_UNENROLLED_TITLE"
	f.observe(t, reviewRequest(0, []PullInput{pull, other}), "private-initial")
	input := publicsite.Approved{ExpectedRevision: 0, Enabled: true, Manifest: manifest}
	preview, err := site.Preview(t.Context(), scope, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = site.Apply(t.Context(), scope, "publish-review-showcase", input, preview.SHA256); err != nil {
		t.Fatal(err)
	}
	var release func()
	publicHints, release, err = f.store.SubscribePublicProjection(f.project, "review-integration-client")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	first, firstBytes := read()
	if first.Revision != 1 || first.Content.Reviews[0].Lifecycle != "merged" || len(first.Content.Reviews[0].Hosted) != 0 {
		t.Fatal("unfinalized source gained public round")
	}
	pull.Title = "PRIVATE_NEW_SOURCE_TITLE"
	f.observe(t, reviewRequest(privateRevision(), []PullInput{pull, other}), "private-source-refresh")
	unchanged(firstBytes)

	// Finalizing via the real observation endpoint adds one historical source
	// summary; the publication hook must use the persisted salt and safe DTO.
	imported.Finalized = true
	pull.Runs = []RunInput{imported}
	f.observe(t, reviewRequest(privateRevision(), []PullInput{pull, other}), "source-finalized")
	second, secondBytes := read()
	if second.Revision != 2 || len(second.Content.Reviews[0].Hosted) != 1 {
		t.Fatal("finalized observation hook missing")
	}
	changedHint()
	var salt string
	if err = f.store.DB().QueryRow(`SELECT round_salt FROM project_publications WHERE project_id=?`, f.project).Scan(&salt); err != nil {
		t.Fatal(err)
	}
	key, err := hex.DecodeString(strings.TrimPrefix(salt, "public_salt_"))
	if err != nil || len(key) != 16 || !strings.HasPrefix(salt, "public_salt_") {
		t.Fatal("private salt contract")
	}
	mac := hmac.New(sha256.New, key)
	for _, part := range []string{f.project, "1", "test-import", imported.SourceRunID, "hosted"} {
		mac.Write([]byte(strconv.Itoa(len(part)) + ":" + part))
	}
	wantKey := "public_round_" + hex.EncodeToString(mac.Sum(nil)[:16])
	round := second.Content.Reviews[0].Hosted[0]
	if round.Key != wantKey || round.Found != 1 || round.Accepted != 1 || round.ObservedAt != imported.FinishedAt || round.DurationMilliseconds == nil || *round.DurationMilliseconds != 60000 {
		t.Fatal("safe HMAC/count/time projection", round)
	}

	// Real native publication and adjudication both advance private snapshots
	// before source finalization; none may create a public activity signal.
	native := nativeUndecidedRun("PRIVATE_NATIVE_SOURCE", 1)
	pull.Runs = nil
	previous := privateRevision()
	mustNativePublish(t, f, NativePublication{Pull: pull, Run: &native})
	if privateRevision() <= previous {
		t.Fatal("native draft did not exercise projection hook")
	}
	unchanged(secondBytes)
	attempt := nativeAttempt("PRIVATE_ACTIVE_ATTEMPT", "active")
	mustNativePublish(t, f, NativePublication{Pull: pull, Attempt: &attempt})
	unchanged(secondBytes)
	diagnostic := nativeAttempt("PRIVATE_UNCERTAIN_ATTEMPT", "uncertain")
	diagnostic.Reason = "private_diagnostic_sentinel"
	mustNativePublish(t, f, NativePublication{Pull: pull, Attempt: &diagnostic})
	unchanged(secondBytes)
	decision := nativeAdjudicationInput(native)
	decideNativeFinding(&decision.Findings[0], "accepted")
	decision.Findings[0].DecisionReason = "PRIVATE_DECISION_REASON"
	if _, changed, err := applyNativeAdjudication(f, decision); err != nil || !changed {
		t.Fatal("native unfinalized decision", changed, err)
	}
	unchanged(secondBytes)
	decision.Finalized = true
	decision.FrozenCounts = &NativeCounts{Found: 1, Accepted: 1}
	if _, changed, err := applyNativeAdjudication(f, decision); err != nil || !changed {
		t.Fatal("native finalization", changed, err)
	}
	third, _ := read()
	if third.Revision != 3 || len(third.Content.Reviews[0].CLI) != 1 || len(third.Content.Reviews[0].Hosted) != 1 {
		t.Fatal("adjudication hook did not add CLI summary")
	}
	changedHint()
	if got := third.Content.Reviews[0].CLI[0]; got.Key == round.Key || got.Found != 1 || got.Accepted != 1 {
		t.Fatal("channel/source identity collided")
	}

	zero := nativeUndecidedRun("PRIVATE_ZERO_HOSTED_SOURCE", 0)
	zero.Channel = "hosted"
	zero.Finalized = true
	mustNativePublish(t, f, NativePublication{Pull: pull, Run: &zero})
	fourth, fourthBytes := read()
	if fourth.Revision != 4 || len(fourth.Content.Reviews[0].Hosted) != 2 || len(fourth.Content.Reviews[0].CLI) != 1 {
		t.Fatal("native finalized publication hook")
	}
	changedHint()
	foundZero := false
	for _, r := range fourth.Content.Reviews[0].Hosted {
		if r.Found == 0 && r.Accepted == 0 {
			foundZero = true
		}
	}
	if !foundZero {
		t.Fatal("genuine zero-finding round missing")
	}

	// Replacing a private observation can omit old runs or explicitly retire a
	// PR. Neither action removes its already-published enrolled merged history.
	pull.Runs = nil
	f.observe(t, reviewRequest(privateRevision(), []PullInput{pull, other}), "current-without-old-runs")
	unchanged(fourthBytes)
	retired := reviewRequest(privateRevision(), []PullInput{other})
	retired.Retired = []RetiredPull{{Number: 1, Reason: "defunct"}}
	f.observe(t, retired, "current-without-enrolled-pr")
	unchanged(fourthBytes)
	var missing int
	if err = f.store.DB().QueryRow(`SELECT count(*) FROM review_pulls p JOIN review_workspace w ON p.project_id=w.project_id AND p.snapshot_id=w.snapshot_id WHERE w.project_id=? AND p.number=1`, f.project).Scan(&missing); err != nil || missing != 0 {
		t.Fatal("missing-PR fixture did not remove current private row", err, missing)
	}
	if fourth.Content.Reviews[0].Lifecycle != "merged" {
		t.Fatal("merged public history not retained")
	}
}

func TestPublicSiteReviewFactsRejectsInvalidSaltAndKeepsOpaqueReferences(t *testing.T) {
	f := newAPIFixture(t)
	pull := nativePull(1)
	pull.Runs = []RunInput{nativeRun("PRIVATE_OPAQUE_SOURCE")}
	f.observe(t, reviewRequest(0, []PullInput{pull}), "source")
	manifest := publicsite.Manifest{Reviews: []publicsite.EnrolledReview{{Number: 1}}}
	read := func(salt string) ([]publicsite.ReviewFacts, error) {
		tx, err := f.store.DB().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		return PublicSiteReviewFacts(context.Background(), tx, f.project, manifest, salt)
	}
	for _, bad := range []string{"", strings.Repeat("a", 64), "public_salt_" + strings.Repeat("a", 64), "public_salt_invalid"} {
		if _, err := read(bad); err == nil {
			t.Fatal("invalid salt accepted")
		}
	}
	first, err := read("public_salt_" + strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	same, err := read("public_salt_" + strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	changed, err := read("public_salt_" + strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(first[0].CLI) != 1 || first[0].CLI[0].Key != same[0].CLI[0].Key || first[0].CLI[0].Key == changed[0].CLI[0].Key {
		t.Fatal("opaque per-publication references unstable or unkeyed")
	}
	raw, _ := json.Marshal(first)
	if bytes.Contains(raw, []byte("PRIVATE_OPAQUE_SOURCE")) {
		t.Fatal("source identity leaked")
	}
}
