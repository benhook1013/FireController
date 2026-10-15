package jobs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	serviceclient "github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/jobs"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/benhook1013/FireController/internal/server"
)

type checkpointAPIValue struct {
	ID          string   `json:"id"`
	ProjectID   string   `json:"project_id"`
	JobID       string   `json:"job_id"`
	JobRevision int64    `json:"job_revision"`
	Sequence    int64    `json:"sequence"`
	ActorID     string   `json:"actor_id"`
	WorkerID    string   `json:"worker_id"`
	Done        string   `json:"done"`
	Next        string   `json:"next"`
	Blocker     string   `json:"blocker"`
	Evidence    []string `json:"evidence"`
	CreatedAt   string   `json:"created_at"`
}

type checkpointReceiptValue struct {
	ID          string `json:"id"`
	Sequence    int64  `json:"sequence"`
	JobRevision int64  `json:"job_revision"`
	Outcome     string `json:"outcome"`
}

func TestCheckpointRoutesPreserveJobAndPageStableArchive(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("checkpoints", "example/checkpoints")
	client := a.grant(project, "checkpoint-client", "read", "write")
	worker := a.createWorker(project, "checkpoint-builder")
	job := createCheckpointTestJob(t, a, project, client)
	base := "/api/v1/projects/" + project + "/jobs/" + job.ID

	firstBody := map[string]any{"expected_job_revision": 1, "done": "  finished exactly  ", "next": "continue\nnext", "evidence": []string{"ref-one"}}
	first := a.callWorker(http.MethodPost, base+"/checkpoints", client, worker, "checkpoint-one", firstBody)
	if first.Code != http.StatusCreated {
		t.Fatalf("append status=%d body=%s", first.Code, first.Body.String())
	}
	var firstReceipt checkpointReceiptValue
	decodeResponse(t, first, &firstReceipt)
	if firstReceipt.ID == "" || firstReceipt.Sequence < 1 || firstReceipt.JobRevision != 1 || firstReceipt.Outcome != "created" {
		t.Fatalf("unexpected compact receipt: %+v", firstReceipt)
	}
	if bytes.Contains(first.Body.Bytes(), []byte("finished exactly")) || bytes.Contains(first.Body.Bytes(), []byte("ref-one")) {
		t.Fatal("checkpoint receipt leaked submitted body text")
	}

	// Exact replay returns the retained compact receipt and creates no second
	// checkpoint or event.
	replay := a.callWorker(http.MethodPost, base+"/checkpoints", client, worker, "checkpoint-one", firstBody)
	if replay.Code != http.StatusCreated || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay did not preserve receipt: status=%d body=%s first=%s", replay.Code, replay.Body.String(), first.Body.String())
	}
	changed := a.callWorker(http.MethodPost, base+"/checkpoints", client, worker, "checkpoint-one",
		map[string]any{"expected_job_revision": 1, "done": "different", "next": "continue", "evidence": []string{"ref-one"}})
	if changed.Code != http.StatusConflict {
		t.Fatalf("changed-request replay status=%d want409 body=%s", changed.Code, changed.Body.String())
	}

	secondBody := map[string]any{"expected_job_revision": 1, "done": "second", "next": "next step"}
	second := a.callWorker(http.MethodPost, base+"/checkpoints", client, worker, "checkpoint-two", secondBody)
	if second.Code != http.StatusCreated {
		t.Fatalf("second append status=%d body=%s", second.Code, second.Body.String())
	}
	var secondReceipt checkpointReceiptValue
	decodeResponse(t, second, &secondReceipt)

	// First page is newest first. A later append cannot enter the continuation
	// page because its sequence is above that page's signed boundary.
	page1 := a.call(http.MethodGet, base+"/checkpoints?limit=1", client, "", nil)
	if page1.Code != http.StatusOK {
		t.Fatalf("page1 status=%d body=%s", page1.Code, page1.Body.String())
	}
	var page checkpointPageValue
	decodeResponse(t, page1, &page)
	if len(page.Items) != 1 || page.Items[0].ID != secondReceipt.ID || !page.HasMore || page.Cursor == "" {
		t.Fatalf("unexpected first archive page: %+v", page)
	}
	third := a.callWorker(http.MethodPost, base+"/checkpoints", client, worker, "checkpoint-three",
		map[string]any{"expected_job_revision": 1, "done": "third", "next": "later"})
	if third.Code != http.StatusCreated {
		t.Fatalf("third append status=%d body=%s", third.Code, third.Body.String())
	}
	var thirdReceipt checkpointReceiptValue
	decodeResponse(t, third, &thirdReceipt)
	page2Path := base + "/checkpoints?limit=1&cursor=" + url.QueryEscape(page.Cursor)
	page2 := a.call(http.MethodGet, page2Path, client, "", nil)
	if page2.Code != http.StatusOK {
		t.Fatalf("page2 status=%d body=%s", page2.Code, page2.Body.String())
	}
	decodeResponse(t, page2, &page)
	if len(page.Items) != 1 || page.Items[0].ID != firstReceipt.ID || page.Items[0].Done != "  finished exactly  " || page.Items[0].Next != "continue\nnext" {
		t.Fatalf("continuation duplicated, skipped or changed older entry: %+v", page)
	}

	latest := a.call(http.MethodGet, base+"/checkpoints/latest", client, "", nil)
	if latest.Code != http.StatusOK {
		t.Fatalf("latest status=%d body=%s", latest.Code, latest.Body.String())
	}
	var latestValue struct {
		JobID       string              `json:"job_id"`
		JobRevision int64               `json:"job_revision"`
		Checkpoint  *checkpointAPIValue `json:"checkpoint"`
	}
	decodeResponse(t, latest, &latestValue)
	if latestValue.JobID != job.ID || latestValue.JobRevision != 1 || latestValue.Checkpoint == nil || latestValue.Checkpoint.ID != thirdReceipt.ID {
		t.Fatalf("unexpected latest response: %+v", latestValue)
	}
	var jobAfter jobValue
	detail := a.call(http.MethodGet, base, client, "", nil)
	decodeResponse(t, detail, &jobAfter)
	if jobAfter.Brief != job.Brief || jobAfter.Revision != 1 || jobAfter.Status != job.Status {
		t.Fatalf("checkpoint append altered current job: before=%+v after=%+v", job, jobAfter)
	}
	var historyCount, eventCount, receiptCount int
	if err := a.store.DB().QueryRow("SELECT count(*) FROM job_history WHERE project_id=? AND job_id=?", project, job.ID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow("SELECT count(*) FROM events WHERE project_id=? AND resource_id=? AND event_type='job.checkpointed'", project, firstReceipt.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := a.store.DB().QueryRow("SELECT count(*) FROM receipts WHERE project_id=? AND operation='jobs.checkpoint' AND target=?", project, job.ID).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 1 || eventCount != 1 || receiptCount != 3 {
		t.Fatalf("checkpoint append changed job history or duplicated durable records: history=%d first-event=%d receipts=%d", historyCount, eventCount, receiptCount)
	}
	var eventPayload string
	if err := a.store.DB().QueryRow("SELECT payload_json FROM events WHERE project_id=? AND resource_id=? AND event_type='job.checkpointed'", project, firstReceipt.ID).Scan(&eventPayload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(eventPayload), []byte("finished exactly")) || bytes.Contains([]byte(eventPayload), []byte("ref-one")) {
		t.Fatalf("checkpoint event included private content: %s", eventPayload)
	}
}

type checkpointPageValue struct {
	Items   []checkpointAPIValue `json:"items"`
	Cursor  string               `json:"cursor"`
	HasMore bool                 `json:"has_more"`
}

func TestCheckpointRevisionIsolationAttributionAndRevocation(t *testing.T) {
	a := setupAPI(t)
	projectA := a.createProject("checkpoint-isolation-a", "example/checkpoint-isolation-a")
	projectB := a.createProject("checkpoint-isolation-b", "example/checkpoint-isolation-b")
	clientA := a.grant(projectA, "checkpoint-isolation-client-a", "read", "write")
	clientB := a.grant(projectB, "checkpoint-isolation-client-b", "read", "write")
	readOnly := a.grant(projectA, "checkpoint-readonly", "read")
	worker := a.createWorker(projectA, "checkpoint-owner")
	job := createCheckpointTestJob(t, a, projectA, clientA)
	base := "/api/v1/projects/" + projectA + "/jobs/" + job.ID
	prior := a.callWorker(http.MethodPost, base+"/checkpoints", clientA, worker, "prior-checkpoint",
		map[string]any{"expected_job_revision": 1, "done": "captured before edit", "next": "continue"})
	if prior.Code != http.StatusCreated {
		t.Fatalf("prior checkpoint status=%d body=%s", prior.Code, prior.Body.String())
	}

	edit := a.call(http.MethodPatch, base, clientA, "checkpoint-edit", map[string]any{"expected_revision": 1, "brief": "new current brief"})
	if edit.Code != http.StatusOK {
		t.Fatalf("edit status=%d body=%s", edit.Code, edit.Body.String())
	}
	priorLatest := a.call(http.MethodGet, base+"/checkpoints/latest", clientA, "", nil)
	var priorLatestValue struct {
		JobRevision int64               `json:"job_revision"`
		Checkpoint  *checkpointAPIValue `json:"checkpoint"`
	}
	decodeResponse(t, priorLatest, &priorLatestValue)
	if priorLatest.Code != http.StatusOK || priorLatestValue.JobRevision != 2 || priorLatestValue.Checkpoint == nil || priorLatestValue.Checkpoint.JobRevision != 1 {
		t.Fatalf("latest did not distinguish current revision from checkpoint capture: status=%d value=%+v", priorLatest.Code, priorLatestValue)
	}
	before := checkpointCounts(t, a, projectA, job.ID)
	stale := a.callWorker(http.MethodPost, base+"/checkpoints", clientA, worker, "stale-checkpoint",
		map[string]any{"expected_job_revision": 1, "done": "stale", "next": "retry after reload"})
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale checkpoint status=%d body=%s", stale.Code, stale.Body.String())
	}
	var staleBody struct {
		Error struct {
			Code    string           `json:"code"`
			Details map[string]int64 `json:"details"`
		} `json:"error"`
	}
	decodeResponse(t, stale, &staleBody)
	if staleBody.Error.Code != "revision_conflict" || staleBody.Error.Details["current_revision"] != 2 {
		t.Fatalf("stale response lacks current job revision: %+v", staleBody)
	}
	if after := checkpointCounts(t, a, projectA, job.ID); after != before {
		t.Fatalf("stale request changed checkpoint/event/receipt counts: before=%+v after=%+v", before, after)
	}
	created := a.callWorker(http.MethodPost, base+"/checkpoints", clientA, worker, "fresh-checkpoint",
		map[string]any{"expected_job_revision": 2, "done": "ready", "next": "continue"})
	if created.Code != http.StatusCreated {
		t.Fatalf("fresh checkpoint status=%d body=%s", created.Code, created.Body.String())
	}
	latest := a.call(http.MethodGet, base+"/checkpoints/latest", clientA, "", nil)
	var latestValue struct {
		JobRevision int64               `json:"job_revision"`
		Checkpoint  *checkpointAPIValue `json:"checkpoint"`
	}
	decodeResponse(t, latest, &latestValue)
	if latest.Code != http.StatusOK || latestValue.JobRevision != 2 || latestValue.Checkpoint == nil || latestValue.Checkpoint.JobRevision != 2 || latestValue.Checkpoint.ActorID == "" || latestValue.Checkpoint.WorkerID != worker {
		t.Fatalf("latest did not report current revision and server-derived attribution: status=%d value=%+v", latest.Code, latestValue)
	}
	if cross := a.call(http.MethodGet, base+"/checkpoints/latest", clientB, "", nil); cross.Code != http.StatusForbidden {
		t.Fatalf("cross-project checkpoint read status=%d want403", cross.Code)
	}
	if denied := a.call(http.MethodPost, base+"/checkpoints", readOnly, "readonly-checkpoint", map[string]any{"expected_job_revision": 2, "done": "x", "next": "y"}); denied.Code != http.StatusForbidden {
		t.Fatalf("read-only checkpoint write status=%d want403", denied.Code)
	}
	credentialHash := fmt.Sprintf("%x", sha256Bytes([]byte(clientA)))
	var credentialID string
	if err := a.store.DB().QueryRow("SELECT id FROM credentials WHERE token_sha256=?", credentialHash).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	revoked := a.call(http.MethodDelete, "/api/v1/projects/"+projectA+"/credentials/"+credentialID, a.admin, "revoke-checkpoint-client", nil)
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	if denied := a.call(http.MethodGet, base+"/checkpoints/latest", clientA, "", nil); denied.Code != http.StatusUnauthorized {
		t.Fatalf("revoked latest read status=%d want401", denied.Code)
	}
}

func TestCheckpointAtomicFailureLeavesRetryableKey(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("checkpoint-atomic", "example/checkpoint-atomic")
	client := a.grant(project, "checkpoint-atomic-client", "read", "write")
	job := createCheckpointTestJob(t, a, project, client)
	base := "/api/v1/projects/" + project + "/jobs/" + job.ID
	if _, err := a.store.DB().Exec(`CREATE TRIGGER fail_checkpoint_event BEFORE INSERT ON events
		WHEN NEW.event_type='job.checkpointed' BEGIN SELECT RAISE(FAIL,'fixture failure'); END`); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"expected_job_revision": 1, "done": "one atomic unit", "next": "retry"}
	failed := a.call(http.MethodPost, base+"/checkpoints", client, "checkpoint-retryable", body)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("injected event failure status=%d body=%s", failed.Code, failed.Body.String())
	}
	if counts := checkpointCounts(t, a, project, job.ID); counts != [3]int{} {
		t.Fatalf("partial checkpoint transaction survived failure: %+v", counts)
	}
	if _, err := a.store.DB().Exec("DROP TRIGGER fail_checkpoint_event"); err != nil {
		t.Fatal(err)
	}
	retry := a.call(http.MethodPost, base+"/checkpoints", client, "checkpoint-retryable", body)
	if retry.Code != http.StatusCreated {
		t.Fatalf("exact retry after rollback status=%d body=%s", retry.Code, retry.Body.String())
	}
	if counts := checkpointCounts(t, a, project, job.ID); counts != [3]int{1, 1, 1} {
		t.Fatalf("successful retry did not commit checkpoint/event/receipt atomically: %+v", counts)
	}
}

func TestCheckpointRetentionAcrossStoreRestart(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("checkpoint-restart", "example/checkpoint-restart")
	client := a.grant(project, "checkpoint-restart-client", "read", "write")
	job := createCheckpointTestJob(t, a, project, client)
	base := "/api/v1/projects/" + project + "/jobs/" + job.ID
	response := a.call(http.MethodPost, base+"/checkpoints", client, "checkpoint-before-restart", map[string]any{
		"expected_job_revision": 1, "done": "durable work", "next": "resume from this point", "evidence": []string{"proof://commit"},
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("append before restart status=%d body=%s", response.Code, response.Body.String())
	}
	var receipt checkpointReceiptValue
	decodeResponse(t, response, &receipt)
	var dbPath string
	if err := a.store.DB().QueryRow("PRAGMA database_list").Scan(new(int), new(string), &dbPath); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Dir(dbPath)
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := jobs.EnsureSchema(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	a.store = store
	baseHandler := server.New(store, server.WithRepositoryResolver(func(_ context.Context, repository string) (server.RepositoryIdentity, error) {
		a.lookups++
		return server.RepositoryIdentity{ID: int64(a.lookups), FullName: repository}, nil
	}))
	a.mux = http.NewServeMux()
	a.mux.Handle("/", baseHandler)
	jobs.Register(a.mux, store)
	latest := a.call(http.MethodGet, base+"/checkpoints/latest", client, "", nil)
	var value struct {
		JobRevision int64               `json:"job_revision"`
		Checkpoint  *checkpointAPIValue `json:"checkpoint"`
	}
	decodeResponse(t, latest, &value)
	if latest.Code != http.StatusOK || value.JobRevision != 1 || value.Checkpoint == nil || value.Checkpoint.ID != receipt.ID || value.Checkpoint.Done != "durable work" || len(value.Checkpoint.Evidence) != 1 || value.Checkpoint.Evidence[0] != "proof://commit" {
		t.Fatalf("checkpoint not retained across restart: status=%d value=%+v", latest.Code, value)
	}
}

func TestCheckpointValidationAndMissingLatest(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("checkpoint-validation", "example/checkpoint-validation")
	client := a.grant(project, "checkpoint-validation-client", "read", "write")
	job := createCheckpointTestJob(t, a, project, client)
	base := "/api/v1/projects/" + project + "/jobs/" + job.ID
	latest := a.call(http.MethodGet, base+"/checkpoints/latest", client, "", nil)
	var noCheckpoint struct {
		JobRevision int64               `json:"job_revision"`
		Checkpoint  *checkpointAPIValue `json:"checkpoint"`
	}
	decodeResponse(t, latest, &noCheckpoint)
	if latest.Code != http.StatusOK || noCheckpoint.JobRevision != 1 || noCheckpoint.Checkpoint != nil {
		t.Fatalf("empty latest response=%d %+v", latest.Code, noCheckpoint)
	}
	for i, body := range []any{
		map[string]any{"expected_job_revision": 0, "done": "x", "next": "y"},
		map[string]any{"expected_job_revision": 1, "done": "\u0001bad", "next": "y"},
		map[string]any{"expected_job_revision": 1, "done": "x", "next": "y", "evidence": []string{" "}},
		map[string]any{"expected_job_revision": 1, "done": string(bytes.Repeat([]byte("d"), 8193)), "next": "y"},
	} {
		response := a.call(http.MethodPost, base+"/checkpoints", client, fmt.Sprintf("invalid-%d", i), body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid checkpoint %d status=%d body=%s", i, response.Code, response.Body.String())
		}
	}
	missing := a.call(http.MethodGet, "/api/v1/projects/"+project+"/jobs/missing/checkpoints/latest", client, "", nil)
	var missingError struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeResponse(t, missing, &missingError)
	if missing.Code != http.StatusNotFound || missingError.Error.Code != "not_found" {
		t.Fatalf("missing job latest status=%d body=%s", missing.Code, missing.Body.String())
	}
}

func TestCheckpointTextBoundariesAndOptionalNormalization(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("checkpoint-boundaries", "example/checkpoint-boundaries")
	client := a.grant(project, "checkpoint-boundaries-client", "read", "write")
	job := createCheckpointTestJob(t, a, project, client)
	base := "/api/v1/projects/" + project + "/jobs/" + job.ID
	validBase := func() map[string]any {
		return map[string]any{"expected_job_revision": 1, "done": "d", "next": "n"}
	}
	invalid := []struct {
		name string
		body map[string]any
	}{
		{"blank-done", map[string]any{"expected_job_revision": 1, "done": " \t\n ", "next": "n"}},
		{"blank-next", map[string]any{"expected_job_revision": 1, "done": "d", "next": " \t\n "}},
		{"c0-control-done", map[string]any{"expected_job_revision": 1, "done": "bad\x01text", "next": "n"}},
		{"c0-control-next", map[string]any{"expected_job_revision": 1, "done": "d", "next": "bad\x01text"}},
		{"del-control-done", map[string]any{"expected_job_revision": 1, "done": "bad\x7ftext", "next": "n"}},
		{"del-control-next", map[string]any{"expected_job_revision": 1, "done": "d", "next": "bad\x7ftext"}},
		{"blocker-overflow", map[string]any{"expected_job_revision": 1, "done": "d", "next": "n", "blocker": strings.Repeat("b", 4097)}},
		{"evidence-count-overflow", map[string]any{"expected_job_revision": 1, "done": "d", "next": "n", "evidence": stringsOf("e", 11)}},
		{"evidence-item-overflow", map[string]any{"expected_job_revision": 1, "done": "d", "next": "n", "evidence": []string{strings.Repeat("e", 1025)}}},
		{"aggregate-overflow", map[string]any{"expected_job_revision": 1, "done": strings.Repeat("d", 8192), "next": strings.Repeat("n", 8192), "blocker": "b"}},
	}
	before := checkpointCounts(t, a, project, job.ID)
	for i, test := range invalid {
		response := a.call(http.MethodPost, base+"/checkpoints", client, fmt.Sprintf("boundary-invalid-%d", i), test.body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s status=%d body=%s", test.name, response.Code, response.Body.String())
		}
	}
	if after := checkpointCounts(t, a, project, job.ID); after != before {
		t.Fatalf("boundary refusals created durable state: before=%+v after=%+v", before, after)
	}

	// The aggregate permits an exact 16 KiB payload; per-field caps and the
	// evidence count/item caps include their exact maxima.
	aggregate := map[string]any{"expected_job_revision": 1, "done": strings.Repeat("d", 8192), "next": strings.Repeat("n", 8191), "evidence": []string{"e"}}
	blocker := map[string]any{"expected_job_revision": 1, "done": "d", "next": "n", "blocker": strings.Repeat("b", 4096)}
	evidence := map[string]any{"expected_job_revision": 1, "done": "d", "next": "n", "evidence": stringsOf(strings.Repeat("e", 1024), 10)}
	for i, body := range []map[string]any{aggregate, blocker, evidence} {
		response := a.call(http.MethodPost, base+"/checkpoints", client, fmt.Sprintf("boundary-valid-%d", i), body)
		if response.Code != http.StatusCreated {
			t.Fatalf("exact boundary %d rejected: status=%d body=%s", i, response.Code, response.Body.String())
		}
	}

	// Allowed horizontal/line whitespace is preserved byte-for-byte.
	controls := validBase()
	controls["done"] = "tab\tone\r\nline two"
	controls["next"] = "next\nstep"
	controls["blocker"] = "blocked\r\nwaiting"
	controlsResponse := a.call(http.MethodPost, base+"/checkpoints", client, "boundary-allowed-controls", controls)
	if controlsResponse.Code != http.StatusCreated {
		t.Fatalf("allowed whitespace rejected: status=%d body=%s", controlsResponse.Code, controlsResponse.Body.String())
	}
	var controlsReceipt checkpointReceiptValue
	decodeResponse(t, controlsResponse, &controlsReceipt)

	// JSON null for optional fields means omission; nil evidence is canonicalized
	// to [] before the idempotency fingerprint is computed.
	nullBody := validBase()
	nullBody["blocker"] = nil
	nullBody["evidence"] = nil
	firstNull := a.call(http.MethodPost, base+"/checkpoints", client, "boundary-null-normalization", nullBody)
	if firstNull.Code != http.StatusCreated {
		t.Fatalf("null optional fields status=%d body=%s", firstNull.Code, firstNull.Body.String())
	}
	var nullReceipt checkpointReceiptValue
	decodeResponse(t, firstNull, &nullReceipt)
	omittedBody := validBase()
	replayNull := a.call(http.MethodPost, base+"/checkpoints", client, "boundary-null-normalization", omittedBody)
	if replayNull.Code != http.StatusCreated || replayNull.Body.String() != firstNull.Body.String() {
		t.Fatalf("null-to-omission retry changed receipt: status=%d body=%s first=%s", replayNull.Code, replayNull.Body.String(), firstNull.Body.String())
	}
	firstOmitted := a.call(http.MethodPost, base+"/checkpoints", client, "boundary-evidence-empty", omittedBody)
	if firstOmitted.Code != http.StatusCreated {
		t.Fatalf("omitted evidence status=%d body=%s", firstOmitted.Code, firstOmitted.Body.String())
	}
	emptyEvidence := validBase()
	emptyEvidence["evidence"] = []string{}
	replayEmpty := a.call(http.MethodPost, base+"/checkpoints", client, "boundary-evidence-empty", emptyEvidence)
	if replayEmpty.Code != http.StatusCreated || replayEmpty.Body.String() != firstOmitted.Body.String() {
		t.Fatalf("omitted-to-empty-evidence retry changed receipt: status=%d body=%s first=%s", replayEmpty.Code, replayEmpty.Body.String(), firstOmitted.Body.String())
	}

	archive := a.call(http.MethodGet, base+"/checkpoints?limit=100", client, "", nil)
	var page checkpointPageValue
	decodeResponse(t, archive, &page)
	if archive.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", archive.Code, archive.Body.String())
	}
	var controlsFound, blockerFound, evidenceFound, nullNormalized bool
	for _, item := range page.Items {
		if item.Evidence == nil {
			t.Fatal("checkpoint evidence was serialized as null instead of an array")
		}
		if item.ID == controlsReceipt.ID {
			controlsFound = item.Done == controls["done"] && item.Next == controls["next"] && item.Blocker == controls["blocker"]
		}
		if item.ID == nullReceipt.ID {
			nullNormalized = item.Blocker == "" && len(item.Evidence) == 0
		}
		if len(item.Blocker) == 4096 {
			blockerFound = true
		}
		if len(item.Evidence) == 10 && len(item.Evidence[0]) == 1024 && len(item.Evidence[9]) == 1024 {
			evidenceFound = true
		}
	}
	if !controlsFound || !blockerFound || !evidenceFound || !nullNormalized {
		t.Fatalf("accepted boundaries or optional normalization were not preserved: controls=%v blocker=%v evidence=%v null-normalized=%v", controlsFound, blockerFound, evidenceFound, nullNormalized)
	}
}

func stringsOf(value string, count int) []string {
	values := make([]string, count)
	for i := range values {
		values[i] = value
	}
	return values
}

func createCheckpointTestJob(t *testing.T, a *testAPI, project, token string) jobValue {
	t.Helper()
	created := a.call(http.MethodPost, "/api/v1/projects/"+project+"/jobs", token, "checkpoint-job-create", map[string]any{
		"title": "Checkpoint test", "brief": "Current brief must remain unchanged", "status": "active",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create job status=%d body=%s", created.Code, created.Body.String())
	}
	var receipt receipt
	decodeResponse(t, created, &receipt)
	response := a.call(http.MethodGet, "/api/v1/projects/"+project+"/jobs/"+receipt.ID, token, "", nil)
	var job jobValue
	decodeResponse(t, response, &job)
	return job
}

func (a *testAPI) callWorker(method, path, token, worker, key string, body any) *httptest.ResponseRecorder {
	a.t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			a.t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-FC-Instance", a.store.InstanceID())
	request.Header.Set("X-FC-Worker", worker)
	request.Header.Set("Idempotency-Key", key)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	a.mux.ServeHTTP(response, request)
	return response
}

func checkpointCounts(t *testing.T, a *testAPI, project, job string) [3]int {
	t.Helper()
	var values [3]int
	queries := []string{
		"SELECT count(*) FROM job_checkpoints WHERE project_id=? AND job_id=?",
		"SELECT count(*) FROM events WHERE project_id=? AND event_type='job.checkpointed' AND resource_id IN (SELECT id FROM job_checkpoints WHERE project_id=? AND job_id=?)",
		"SELECT count(*) FROM receipts WHERE project_id=? AND operation='jobs.checkpoint' AND target=?",
	}
	for i, query := range queries {
		var err error
		if i == 1 {
			err = a.store.DB().QueryRow(query, project, project, job).Scan(&values[i])
		} else {
			err = a.store.DB().QueryRow(query, project, job).Scan(&values[i])
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return values
}

func sha256Bytes(data []byte) [32]byte {
	return sha256.Sum256(data)
}

func TestCheckpointEquivalentEmptyBlockerCrossClientReplay(t *testing.T) {
	for _, firstShape := range []string{"omitted", "null", "empty", "typed-client"} {
		t.Run(firstShape, func(t *testing.T) {
			a := setupAPI(t)
			project := a.createProject("checkpoint-equivalent", "example/checkpoint-equivalent")
			token := a.grant(project, "checkpoint-equivalent-client", "read", "write")
			worker := a.createWorker(project, "Builder")
			job := createCheckpointTestJob(t, a, project, token)
			base := "/api/v1/projects/" + project + "/jobs/" + job.ID
			providerReads := a.lookups
			service := httptest.NewServer(a.mux)
			defer service.Close()
			credential := filepath.Join(t.TempDir(), "client.token")
			if err := os.WriteFile(credential, []byte(token), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := serviceclient.New(t.Context(), profile.Binding{Context: profile.Context{Profile: "checkpoint-builder", ServerURL: service.URL, InstanceID: a.store.InstanceID(), ProjectID: project, WorkerID: worker}, CredentialFile: credential})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			request := serviceclient.AppendCheckpointRequest{ExpectedJobRevision: 1, Done: "Exact completed work", Next: "Next explicit step"}
			body := func(shape string) map[string]any {
				value := map[string]any{"expected_job_revision": 1, "done": request.Done, "next": request.Next}
				if shape == "null" {
					value["blocker"] = nil
					value["evidence"] = nil
				}
				if shape == "empty" {
					value["blocker"] = ""
					value["evidence"] = []string{}
				}
				return value
			}
			const key = "equivalent-checkpoint"
			var first checkpointReceiptValue
			if firstShape == "typed-client" {
				receipt, err := client.AppendCheckpoint(t.Context(), key, job.ID, request)
				if err != nil {
					t.Fatal(err)
				}
				first = checkpointReceiptValue{receipt.ID, receipt.Sequence, receipt.JobRevision, receipt.Outcome}
			} else {
				response := a.callWorker(http.MethodPost, base+"/checkpoints", token, worker, key, body(firstShape))
				if response.Code != http.StatusCreated {
					t.Fatalf("initial append status=%d", response.Code)
				}
				decodeResponse(t, response, &first)
			}
			assertReplay := func() {
				t.Helper()
				for _, shape := range []string{"omitted", "null", "empty"} {
					response := a.callWorker(http.MethodPost, base+"/checkpoints", token, worker, key, body(shape))
					var got checkpointReceiptValue
					if response.Code != http.StatusCreated {
						t.Fatalf("%s equivalent retry status=%d body=%s", shape, response.Code, response.Body.String())
					}
					decodeResponse(t, response, &got)
					if got != first {
						t.Fatalf("%s retry changed retained receipt", shape)
					}
				}
				got, err := client.AppendCheckpoint(t.Context(), key, job.ID, request)
				if err != nil || got.ID != first.ID || got.Sequence != first.Sequence || got.JobRevision != first.JobRevision || got.Outcome != first.Outcome {
					t.Fatalf("typed-client equivalent retry failed: %v", err)
				}
				if counts := checkpointCounts(t, a, project, job.ID); counts != [3]int{1, 1, 1} {
					t.Fatalf("equivalent retries duplicated effects: %v", counts)
				}
			}
			assertReplay()
			// Replay resolves the retained request before current job-revision checks.
			edited := a.call(http.MethodPatch, base, token, "advance-equivalent-job", map[string]any{"expected_revision": 1, "brief": "New current brief"})
			if edited.Code != http.StatusOK {
				t.Fatal("fixture job did not advance")
			}
			assertReplay()
			for _, blocker := range []string{"blocked", " "} {
				changed := body("empty")
				changed["blocker"] = blocker
				response := a.callWorker(http.MethodPost, base+"/checkpoints", token, worker, key, changed)
				var failure struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				decodeResponse(t, response, &failure)
				if response.Code != http.StatusConflict || failure.Error.Code != "idempotency_conflict" {
					t.Fatalf("changed blocker failed to conflict: %d %s", response.Code, response.Body.String())
				}
			}
			latest := a.call(http.MethodGet, base+"/checkpoints/latest", token, "", nil)
			var current struct {
				JobRevision int64              `json:"job_revision"`
				Checkpoint  checkpointAPIValue `json:"checkpoint"`
			}
			decodeResponse(t, latest, &current)
			if current.JobRevision != 2 || current.Checkpoint.JobRevision != 1 || current.Checkpoint.Blocker != "" || current.Checkpoint.Evidence == nil {
				t.Fatal("normalization changed stored meaning or captured/current revisions")
			}
			if counts := checkpointCounts(t, a, project, job.ID); counts != [3]int{1, 1, 1} {
				t.Fatal("changed-blocker rejection created effects", counts)
			}
			if a.lookups != providerReads {
				t.Fatal("checkpoint retries performed provider work")
			}
		})
	}
}

func TestCheckpointHistoricalEmptyBlockerReceiptSurvivesCanonicalRecovery(t *testing.T) {
	a := setupAPI(t)
	project := a.createProject("checkpoint-historical", "example/checkpoint-historical")
	token := a.grant(project, "checkpoint-historical-client", "read", "write")
	worker := a.createWorker(project, "Builder")
	job := createCheckpointTestJob(t, a, project, token)
	base := "/api/v1/projects/" + project + "/jobs/" + job.ID
	request := httptest.NewRequest(http.MethodPost, base+"/checkpoints", nil)
	request.SetPathValue("project_id", project)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-FC-Worker", worker)
	scope, err := a.store.Scope(request, "write")
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	// Reconstruct the exact pre-normalization request shape through the actual
	// core fingerprint/receipt transaction, not an invented fingerprint hash.
	legacy := struct {
		ExpectedJobRevision int64    `json:"expected_job_revision"`
		Done                string   `json:"done"`
		Next                string   `json:"next"`
		Blocker             *string  `json:"blocker"`
		Evidence            []string `json:"evidence"`
	}{1, "Historical exact work", "Historical next step", &empty, []string{}}
	const key = "historical-empty-blocker"
	id := core.NewID("checkpoint")
	receipt, err := a.store.Mutate(t.Context(), scope, core.Mutation{Operation: "jobs.checkpoint", Target: job.ID, Key: key, Request: legacy}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var sequence int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO job_checkpoints (id,project_id,job_id,job_revision,actor_id,worker_id,done,next,blocker,evidence_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) RETURNING sequence`, id, project, job.ID, 1, scope.PrincipalID, scope.WorkerID, legacy.Done, legacy.Next, "", "[]", time.Now().UTC().Format(time.RFC3339Nano)).Scan(&sequence); err != nil {
			return nil, err
		}
		if err := core.AppendEvent(ctx, tx, scope, "job.checkpointed", id, 1, map[string]any{"job_id": job.ID, "sequence": sequence}); err != nil {
			return nil, err
		}
		return checkpointReceiptValue{id, sequence, 1, "created"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var originalFingerprint string
	if err := a.store.DB().QueryRow(`SELECT fingerprint FROM receipts WHERE project_id=? AND operation='jobs.checkpoint' AND target=? AND key=?`, project, job.ID, key).Scan(&originalFingerprint); err != nil {
		t.Fatal(err)
	}
	body := func(shape string) map[string]any {
		value := map[string]any{"expected_job_revision": 1, "done": legacy.Done, "next": legacy.Next}
		if shape == "null" {
			value["blocker"] = nil
			value["evidence"] = nil
		}
		if shape == "empty" {
			value["blocker"] = ""
			value["evidence"] = []string{}
		}
		return value
	}
	assertReplays := func() {
		t.Helper()
		for _, shape := range []string{"omitted", "null", "empty"} {
			response := a.callWorker(http.MethodPost, base+"/checkpoints", token, worker, key, body(shape))
			if response.Code != http.StatusCreated || !bytes.Equal(bytes.TrimSpace(response.Body.Bytes()), receipt) {
				t.Fatalf("historical %s recovery failed: %d %s", shape, response.Code, response.Body.String())
			}
		}
		var fingerprint, currentReceipt string
		if err := a.store.DB().QueryRow(`SELECT fingerprint,response_json FROM receipts WHERE project_id=? AND operation='jobs.checkpoint' AND target=? AND key=?`, project, job.ID, key).Scan(&fingerprint, &currentReceipt); err != nil {
			t.Fatal(err)
		}
		if fingerprint != originalFingerprint || currentReceipt != string(receipt) {
			t.Fatal("recovery rewrote historical receipt")
		}
		if counts := checkpointCounts(t, a, project, job.ID); counts != [3]int{1, 1, 1} {
			t.Fatal("recovery allocated checkpoint/event/receipt", counts)
		}
	}
	assertReplays()
	edited := a.call(http.MethodPatch, base, token, "advance-historical-job", map[string]any{"expected_revision": 1, "brief": "Current revision two"})
	if edited.Code != http.StatusOK {
		t.Fatal("fixture job did not advance")
	}
	assertReplays()
	var dbPath string
	if err := a.store.DB().QueryRow(`PRAGMA database_list`).Scan(new(int), new(string), &dbPath); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := core.Open(t.Context(), filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := jobs.EnsureSchema(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	a.store = store
	a.mux = http.NewServeMux()
	a.mux.Handle("/", server.New(store))
	jobs.Register(a.mux, store)
	assertReplays()
	for _, field := range []string{"done", "next", "blocker", "evidence"} {
		changed := body("empty")
		if field == "evidence" {
			changed[field] = []string{"different-evidence"}
		} else {
			changed[field] = "different"
		}
		response := a.callWorker(http.MethodPost, base+"/checkpoints", token, worker, key, changed)
		if response.Code != http.StatusConflict || !bytes.Contains(response.Body.Bytes(), []byte(`"code":"idempotency_conflict"`)) {
			t.Fatalf("changed %s incorrectly recovered legacy receipt: %d %s", field, response.Code, response.Body.String())
		}
	}
	var credentialID string
	if err := store.DB().QueryRow(`SELECT id FROM credentials WHERE token_sha256=?`, fmt.Sprintf("%x", sha256.Sum256([]byte(token)))).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	revoked := a.call(http.MethodDelete, "/api/v1/projects/"+project+"/credentials/"+credentialID, a.admin, "revoke-historical-client", nil)
	if revoked.Code != http.StatusOK {
		t.Fatal("fixture revoke failed")
	}
	response := a.callWorker(http.MethodPost, base+"/checkpoints", token, worker, key, body("empty"))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("historical recovery bypassed current authorization: %d", response.Code)
	}
	if counts := checkpointCounts(t, a, project, job.ID); counts != [3]int{1, 1, 1} {
		t.Fatal("recovery refusals created effects", counts)
	}
}
