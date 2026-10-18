package jobs_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/inbox"
)

type contextInboxPage struct {
	Items []struct {
		ID                string `json:"id"`
		ConversationID    string `json:"conversation_id"`
		RecipientWorkerID string `json:"recipient_worker_id"`
		Preview           string `json:"preview"`
		Seen              bool   `json:"seen"`
		Acknowledged      bool   `json:"acknowledged"`
	} `json:"items"`
	Cursor  string `json:"cursor"`
	HasMore bool   `json:"has_more"`
}
type jobContextValue struct {
	ProjectID   string              `json:"project_id"`
	WorkerID    string              `json:"worker_id"`
	Job         jobValue            `json:"job"`
	Checkpoint  *checkpointAPIValue `json:"checkpoint"`
	Checklist   progressChecklist   `json:"checklist"`
	Primary     primaryValue        `json:"primary"`
	Pause       pauseValue          `json:"pause"`
	Notes       notesPageValue      `json:"notes"`
	Inbox       contextInboxPage    `json:"inbox"`
	EventCursor string              `json:"event_cursor"`
	Complete    bool                `json:"complete"`
	Omitted     []string            `json:"omitted"`
}

func contextFixture(t *testing.T) (*testAPI, string, string, string, string, string) {
	t.Helper()
	a, p, token, worker, base := progressFixture(t)
	if err := inbox.EnsureSchema(t.Context(), a.store); err != nil {
		t.Fatal(err)
	}
	inbox.Register(a.mux, a.store)
	other := a.createWorker(p, "other")
	return a, p, token, worker, other, base
}
func contextRead(t *testing.T, a *testAPI, base, token, worker string) jobContextValue {
	t.Helper()
	r := noteCall(a, "GET", base+"/context", token, worker, "", nil)
	noteOK(t, r, 200)
	var result jobContextValue
	decodeResponse(t, r, &result)
	return result
}
func contextState(t *testing.T, a *testAPI) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, table := range []string{"jobs", "job_history", "job_checkpoints", "job_updates", "job_checklists", "job_checklist_history", "worker_primary_jobs", "worker_primary_job_history", "worker_pause_state", "worker_pause_history", "notes", "note_revisions", "inbox_messages", "inbox_conversations", "inbox_recipient_state", "events", "receipts"} {
		rows, err := a.store.DB().Query(`SELECT * FROM ` + table + ` ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		values := [][]any{}
		for rows.Next() {
			row := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range row {
				pointers[i] = &row[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			values = append(values, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		out[table] = sha256.Sum256(encoded)
	}
	return out
}
func contextMessage(t *testing.T, a *testAPI, p, token, sender, recipient, key, body string) string {
	t.Helper()
	r := noteCall(a, "POST", "/api/v1/projects/"+p+"/messages", token, sender, key, map[string]any{"recipient_worker_id": recipient, "body": body})
	noteOK(t, r, 201)
	var receipt struct {
		ID string `json:"id"`
	}
	decodeResponse(t, r, &receipt)
	return receipt.ID
}

func TestJobContextCompleteAuthoritativeCurrentAndIndependentWorkerState(t *testing.T) {
	a, p, token, worker, other, base := contextFixture(t)
	before, lookups := contextState(t, a), a.lookups
	got := contextRead(t, a, base, token, worker)
	if got.ProjectID != p || got.WorkerID != worker || got.Job.ID != strings.TrimPrefix(base, "/api/v1/projects/"+p+"/jobs/") || !got.Complete || got.Omitted == nil || len(got.Omitted) != 0 || got.Checkpoint != nil || got.Checklist.Revision != 0 || got.Checklist.Items == nil || got.Primary.WorkerID != worker || got.Primary.Revision != 0 || got.Primary.JobID != "" || got.Pause.WorkerID != worker || got.Pause.Paused || got.Pause.Revision != 0 || got.Notes.Items == nil || got.Inbox.Items == nil || got.EventCursor == "" {
		t.Fatalf("incomplete empty context: %+v", got)
	}
	if !reflect.DeepEqual(before, contextState(t, a)) || lookups != a.lookups {
		t.Fatal("context read changed state or contacted provider")
	}
	// This selected job belongs to somebody else; caller coordination must not
	// silently change identity to match it.
	brief := strings.Repeat("<", 32<<10)
	noteOK(t, noteCall(a, "POST", base+"/checkpoints", token, other, "old-checkpoint", map[string]any{"expected_job_revision": 1, "done": "prior milestone", "next": "old next"}), 201)
	noteOK(t, noteCall(a, "PATCH", base, token, other, "current-brief", map[string]any{"expected_revision": 1, "brief": brief, "worker_id": other}), 200)
	noteOK(t, noteCall(a, "PUT", base+"/checklist", token, other, "checklist", progressReplace(2, 0, []progressItem{{"a", "current plan", false}, {"b", "finished", true}})), 200)
	focus := a.call("POST", "/api/v1/projects/"+p+"/jobs", token, "other-focus", map[string]any{"title": "independent focus", "worker_id": worker})
	noteOK(t, focus, 201)
	var selected receipt
	decodeResponse(t, focus, &selected)
	noteOK(t, noteCall(a, "PUT", "/api/v1/projects/"+p+"/workers/"+worker+"/primary-job", token, worker, "focus", map[string]any{"expected_revision": 0, "job_id": selected.ID}), 200)
	noteOK(t, noteCall(a, "PUT", pausePath(p, worker), token, worker, "pause", pauseRequest(0, true, "explicit hold")), 200)
	before = contextState(t, a)
	got = contextRead(t, a, base, token, worker)
	if got.Job.Brief != brief || got.Job.WorkerID != other || got.Job.Revision != 2 || got.WorkerID != worker || got.Checkpoint == nil || got.Checkpoint.JobRevision != 1 || got.Checkpoint.Done != "prior milestone" || got.Checklist.JobRevision != 2 || got.Checklist.Revision != 1 || len(got.Checklist.Items) != 2 || got.Primary.JobID != selected.ID || !got.Pause.Paused || got.Pause.Reason != "explicit hold" || !got.Complete {
		t.Fatal("current authority, older checkpoint or bound worker states were mixed")
	}
	if !reflect.DeepEqual(before, contextState(t, a)) || a.lookups != lookups {
		t.Fatal("full context changed state or fetched provider")
	}
}

func TestJobContextPendingScopeExactOmissionsAndOrdinaryContinuation(t *testing.T) {
	a, p, token, worker, other, base := contextFixture(t)
	jobID := strings.TrimPrefix(base, "/api/v1/projects/"+p+"/jobs/")
	noteOK(t, noteCall(a, "PATCH", base, token, worker, "assign", map[string]any{"expected_revision": 1, "worker_id": worker}), 200)
	notesBase := "/api/v1/projects/" + p + "/notes"
	// Phase-only, other worker and consumed notes are outside this packet.
	// This is retained legacy phase-only state: create it with an immutable
	// revision directly so the fixture does not exercise today's stricter API.
	legacyID := "note-context-legacy-phase"
	legacyTime := "2024-01-02T03:04:05.000000000Z"
	if _, err := a.store.DB().Exec(`INSERT INTO notes(project_id,id,worker_id,job_id,phase,kind,body,status,status_reason,revision,created_at,updated_at) VALUES(?,?,NULL,NULL,'phase','reminder','outside phase','pending','',1,?,?)`, p, legacyID, legacyTime, legacyTime); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.DB().Exec(`INSERT INTO note_revisions(project_id,note_id,revision,worker_id,job_id,phase,kind,body,status,status_reason,created_at,updated_at,actor_id,actor_worker_id,recorded_at) VALUES(?,?,1,NULL,NULL,'phase','reminder','outside phase','pending','',?,?,?,?,?)`, p, legacyID, legacyTime, legacyTime, other, other, legacyTime); err != nil {
		t.Fatal(err)
	}
	noteCreate(t, a, notesBase, token, other, "other", other, "", "", "outside worker")
	consumed := noteCreate(t, a, notesBase, token, worker, "consumed", worker, "", "", "already handled")
	noteOK(t, noteCall(a, "PATCH", notesBase+"/"+consumed.ID, token, worker, "consume", map[string]any{"expected_revision": 1, "status": "consumed"}), 200)
	assigned := noteCreate(t, a, notesBase, token, other, "assigned", other, jobID, "", "assigned-job coordination")
	direct := noteCreate(t, a, notesBase, token, worker, "direct", worker, "", "", "short")
	acked := contextMessage(t, a, p, token, other, worker, "acked", "handled")
	noteOK(t, noteCall(a, "POST", "/api/v1/projects/"+p+"/messages/"+acked+"/ack", token, worker, "ack", map[string]any{}), 200)
	seen := contextMessage(t, a, p, token, other, worker, "seen", "seen but unhandled")
	noteOK(t, noteCall(a, "POST", "/api/v1/projects/"+p+"/messages/"+seen+"/seen", token, worker, "seen", map[string]any{}), 200)
	unseen := contextMessage(t, a, p, token, other, worker, "unseen", "unseen and unhandled")
	contextMessage(t, a, p, token, worker, other, "outgoing", "other worker inbox")
	before, lookups := contextState(t, a), a.lookups
	r := noteCall(a, "GET", base+"/context?limit=1", token, worker, "", nil)
	noteOK(t, r, 200)
	var got jobContextValue
	decodeResponse(t, r, &got)
	if got.Complete || !reflect.DeepEqual(got.Omitted, []string{"note_bodies", "additional_notes", "message_bodies", "additional_messages"}) || len(got.Notes.Items) != 1 || got.Notes.Items[0].ID != direct.ID || got.Notes.Items[0].BodyPreview != "short" || !got.Notes.HasMore || len(got.Inbox.Items) != 1 || got.Inbox.Items[0].ID != seen || !got.Inbox.Items[0].Seen || got.Inbox.Items[0].Acknowledged || !got.Inbox.HasMore {
		t.Fatalf("wrong selected pending packet: %+v", got)
	}
	continued := noteList(t, a, notesBase+"?worker_id="+worker+"&include_assigned_jobs=true&status=pending&cursor="+url.QueryEscape(got.Notes.Cursor), token)
	if len(continued.Items) != 1 || continued.Items[0].ID != assigned.ID {
		t.Fatal("context notes cursor did not continue ordinary selection")
	}
	inboxPath := "/api/v1/projects/" + p + "/inbox/" + worker
	w := noteCall(a, "GET", inboxPath+"?unread=false&unacknowledged=true&cursor="+url.QueryEscape(got.Inbox.Cursor), token, worker, "", nil)
	noteOK(t, w, 200)
	var page contextInboxPage
	decodeResponse(t, w, &page)
	if len(page.Items) != 1 || page.Items[0].ID != unseen {
		t.Fatal("context inbox cursor did not continue ordinary selection")
	}
	noteError(t, noteCall(a, "GET", notesBase+"?worker_id="+worker+"&status=pending&cursor="+url.QueryEscape(got.Notes.Cursor), token, worker, "", nil), 400, "validation_failed")
	noteError(t, noteCall(a, "GET", inboxPath+"?unread=true&unacknowledged=true&cursor="+url.QueryEscape(got.Inbox.Cursor), token, worker, "", nil), 400, "validation_failed")
	if !reflect.DeepEqual(before, contextState(t, a)) || lookups != a.lookups {
		t.Fatal("context/continuation changed handling or other durable state")
	}
	newMessage := contextMessage(t, a, p, token, other, worker, "after-context", "arrived after context")
	events := noteCall(a, "GET", "/api/v1/projects/"+p+"/message-events?cursor="+url.QueryEscape(got.EventCursor), token, worker, "", nil)
	noteOK(t, events, 200)
	if !strings.Contains(events.Body.String(), newMessage) || strings.Contains(events.Body.String(), "arrived after context") {
		t.Fatal("context watermark lost following event or leaked body")
	}
	// Short previews still explicitly omit bodies; a final page only removes
	// additional_* omissions rather than claiming full authoritative content.
	final := contextRead(t, a, base, token, worker)
	if !reflect.DeepEqual(final.Omitted, []string{"note_bodies", "message_bodies"}) {
		t.Fatal(final.Omitted)
	}
}

func TestJobContextStrictScopeCorruptionAndNoArchiveExpansion(t *testing.T) {
	a, p, token, worker, other, base := contextFixture(t)
	foreign := a.createProject("foreign-context", "example/foreign-context")
	foreignToken := a.grant(foreign, "foreign-context-token", "read")
	reader := a.grant(p, "context-reader", "read")
	before := contextState(t, a)
	noteError(t, noteCall(a, "GET", base+"/context", token, "", "", nil), 400, "worker_required")
	for _, query := range []string{"cursor=x", "worker_id=" + other, "limit=1&limit=2", "limit=101", "limit=0", "limit=-1", "unread=true", "limit=%zz"} {
		noteOK(t, noteCall(a, "GET", base+"/context?"+query, token, worker, "", nil), 400)
	}
	noteOK(t, noteCall(a, "GET", base+"/context", foreignToken, worker, "", nil), 403)
	noteOK(t, noteCall(a, "GET", base+"/context", token, "missing-worker", "", nil), 404)
	noteOK(t, noteCall(a, "GET", "/api/v1/projects/"+p+"/jobs/missing/context", token, worker, "", nil), 404)
	noteOK(t, noteCall(a, "GET", base+"/context", reader, worker, "", nil), 200)
	req := httptest.NewRequest("GET", base+"/context", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-FC-Worker", worker)
	req.Header.Set("X-FC-Instance", "different-instance")
	w := httptest.NewRecorder()
	a.mux.ServeHTTP(w, req)
	noteError(t, w, 409, "instance_mismatch")
	if !reflect.DeepEqual(before, contextState(t, a)) {
		t.Fatal("refused contexts changed state")
	}
	// Missing unrelated archives must not be mistaken for absent current data.
	for _, table := range []string{"job_history", "job_updates", "job_checklist_history", "worker_primary_job_history", "worker_pause_history", "note_revisions"} {
		if _, err := a.store.DB().Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	contextRead(t, a, base, token, worker)
	// Malformed retained *current* data refuses the whole operation.
	if _, err := a.store.DB().Exec(`UPDATE jobs SET updated_at='invalid' WHERE id=?`, strings.TrimPrefix(base, "/api/v1/projects/"+p+"/jobs/")); err != nil {
		t.Fatal(err)
	}
	noteError(t, noteCall(a, "GET", base+"/context", token, worker, "", nil), 500, "storage_unavailable")
	if _, err := a.store.DB().Exec(`UPDATE credentials SET revoked=1 WHERE token_sha256=?`, fmt.Sprintf("%x", sha256.Sum256([]byte(token)))); err != nil {
		t.Fatal(err)
	}
	noteOK(t, noteCall(a, "GET", base+"/context", token, worker, "", nil), 401)
}

func TestJobContextConcurrentAssignmentNeverMixesCoordinationSnapshots(t *testing.T) {
	a, p, token, worker, other, base := contextFixture(t)
	id := strings.TrimPrefix(base, "/api/v1/projects/"+p+"/jobs/")
	noteCreate(t, a, "/api/v1/projects/"+p+"/notes", token, other, "related", other, id, "", "only relevant while assigned to caller")
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		for revision := int64(1); revision <= 40; revision++ {
			target := worker
			if revision%2 == 0 {
				target = other
			}
			r := noteCall(a, "PATCH", base, token, other, fmt.Sprint("move-", revision), map[string]any{"expected_revision": revision, "worker_id": target})
			if r.Code != 200 {
				done <- fmt.Errorf("assignment status%d: %s", r.Code, r.Body.String())
				return
			}
		}
		done <- nil
	}()
	close(start)
	for range 50 {
		got := contextRead(t, a, base, token, worker)
		expected := 0
		if got.Job.WorkerID == worker {
			expected = 1
		}
		if len(got.Notes.Items) != expected || got.Checklist.JobRevision != got.Job.Revision {
			t.Fatal("mixed job assignment and pending-note/checklist snapshots")
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestJobContextEncodedBudgetRefusesPartialPacket(t *testing.T) {
	a, p, token, _, _, base := contextFixture(t)
	worker, sender := strings.Repeat("&", 128), strings.Repeat("<", 128)
	for i, id := range []string{worker, sender} {
		if _, err := a.store.DB().Exec(`INSERT INTO workers(project_id,id,name) VALUES(?,?,?)`, p, id, fmt.Sprint("bounded identity ", i)); err != nil {
			t.Fatal(err)
		}
	}
	job := strings.Repeat(">", 128)
	brief := strings.Repeat("<", 32<<10)
	// Native selectors permit these literal IDs; their encoded size matters.
	if _, err := a.store.DB().Exec(`INSERT INTO jobs(project_id,id,title,brief,worker_id,status,revision,created_at,updated_at) VALUES(?,?,?,?,?,'active',1,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, p, job, "large context", brief, worker); err != nil {
		t.Fatal(err)
	}
	base = "/api/v1/projects/" + p + "/jobs/" + url.PathEscape(job)
	noteOK(t, noteCall(a, "POST", base+"/checkpoints", token, sender, "large-checkpoint", map[string]any{"expected_job_revision": 1, "done": strings.Repeat("<", 8192), "next": strings.Repeat("<", 8192)}), 201)
	items := make([]progressItem, 16)
	for i := range items {
		items[i] = progressItem{fmt.Sprint(i), strings.Repeat("<", 1022), false}
	}
	noteOK(t, noteCall(a, "PUT", base+"/checklist", token, sender, "large-checklist", progressReplace(1, 0, items)), 200)
	for i := range 100 {
		noteCreate(t, a, "/api/v1/projects/"+p+"/notes", token, sender, fmt.Sprint("large-note-", i), worker, job, strings.Repeat("<", 200), strings.Repeat("<", 240))
		contextMessage(t, a, p, token, sender, worker, fmt.Sprint("large-message-", i), strings.Repeat("<", 160))
	}
	before, lookups := contextState(t, a), a.lookups
	w := noteCall(a, "GET", base+"/context?limit=100", token, worker, "", nil)
	noteError(t, w, 413, "context_too_large")
	if strings.Contains(w.Body.String(), "large context") || strings.Contains(w.Body.String(), "\\u003c") {
		t.Fatal("oversized response exposed a partial packet")
	}
	w = noteCall(a, "GET", base+"/context?limit=1", token, worker, "", nil)
	noteOK(t, w, 200)
	var got jobContextValue
	decodeResponse(t, w, &got)
	if got.Job.Brief != brief || got.Checkpoint == nil || len(got.Checklist.Items) != 16 || len(w.Body.Bytes()) > 1<<20 {
		t.Fatal("smaller pending pages shortened authoritative context")
	}
	if !reflect.DeepEqual(before, contextState(t, a)) || lookups != a.lookups {
		t.Fatal("budget failure/read changed durable state or provider work")
	}
}

// Keep the cancellation path on the public handler rather than an internal read.
func TestJobContextCancelledRequestDoesNotAcknowledgeOrWrite(t *testing.T) {
	a, p, token, worker, other, base := contextFixture(t)
	contextMessage(t, a, p, token, other, worker, "pending", "still pending")
	before := contextState(t, a)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequestWithContext(ctx, "GET", base+"/context", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-FC-Instance", a.store.InstanceID())
	r.Header.Set("X-FC-Worker", worker)
	w := httptest.NewRecorder()
	a.mux.ServeHTTP(w, r)
	if w.Code == http.StatusOK || !reflect.DeepEqual(before, contextState(t, a)) {
		t.Fatal("cancelled context succeeded or changed state")
	}
}

func TestJobContextMalformedRetainedSectionsFailWholeRead(t *testing.T) {
	for _, corrupt := range []struct{ name, statement string }{
		{"future checkpoint", `UPDATE job_checkpoints SET job_revision=2`},
		{"null checkpoint inventory", `UPDATE job_checkpoints SET evidence_json='null'`},
		{"invalid checkpoint text", `UPDATE job_checkpoints SET done=''`},
		{"null checklist inventory", `UPDATE job_checklists SET items_json='null'`},
		{"invalid pause", `UPDATE worker_pause_state SET reason=char(1)`},
		{"stale primary assignment", `UPDATE jobs SET worker_id=NULL`},
		{"invalid note", `UPDATE notes SET body=''`},
		{"invalid incoming time", `UPDATE inbox_messages SET created_at='invalid'`},
		{"missing incoming state", `DELETE FROM inbox_recipient_state`},
	} {
		t.Run(corrupt.name, func(t *testing.T) {
			a, p, token, worker, other, base := contextFixture(t)
			id := strings.TrimPrefix(base, "/api/v1/projects/"+p+"/jobs/")
			noteOK(t, noteCall(a, "POST", base+"/checkpoints", token, other, "checkpoint", map[string]any{"expected_job_revision": 1, "done": "done", "next": "next"}), 201)
			noteOK(t, noteCall(a, "PUT", base+"/checklist", token, worker, "checklist", progressReplace(1, 0, []progressItem{{"a", "current", false}})), 200)
			// Seed a valid primary through its normal API, without incrementing the
			// job revision used by the checkpoint for this corruption matrix.
			if _, err := a.store.DB().Exec(`UPDATE jobs SET worker_id=? WHERE id=?`, worker, id); err != nil {
				t.Fatal(err)
			}
			noteOK(t, noteCall(a, "PUT", "/api/v1/projects/"+p+"/workers/"+worker+"/primary-job", token, worker, "primary", map[string]any{"expected_revision": 0, "job_id": id}), 200)
			noteOK(t, noteCall(a, "PUT", pausePath(p, worker), token, worker, "pause", pauseRequest(0, true, "current pause")), 200)
			noteCreate(t, a, "/api/v1/projects/"+p+"/notes", token, worker, "note", worker, "", "", "current note")
			contextMessage(t, a, p, token, other, worker, "message", "current incoming")
			contextRead(t, a, base, token, worker)
			if _, err := a.store.DB().Exec(corrupt.statement); err != nil {
				t.Fatal(err)
			}
			before := contextState(t, a)
			w := noteCall(a, "GET", base+"/context", token, worker, "", nil)
			noteError(t, w, 500, "storage_unavailable")
			if strings.Contains(w.Body.String(), `"job":`) || !reflect.DeepEqual(before, contextState(t, a)) {
				t.Fatal("corrupt component returned partial context or changed state")
			}
		})
	}
}

func TestJobContextDefaultLimitBoundsEachPendingSectionIndependently(t *testing.T) {
	a, p, token, worker, other, base := contextFixture(t)
	for i := range 21 {
		noteCreate(t, a, "/api/v1/projects/"+p+"/notes", token, worker, fmt.Sprint("pending-", i), worker, "", "", "small note")
	}
	contextMessage(t, a, p, token, other, worker, "one-message", "one small incoming")
	before := contextState(t, a)
	got := contextRead(t, a, base, token, worker)
	if len(got.Notes.Items) != 20 || !got.Notes.HasMore || len(got.Inbox.Items) != 1 || got.Inbox.HasMore || !reflect.DeepEqual(got.Omitted, []string{"note_bodies", "additional_notes", "message_bodies"}) {
		t.Fatal("default limit did not bound sections independently")
	}
	r := noteCall(a, "GET", base+"/context?limit=100", token, worker, "", nil)
	noteOK(t, r, 200)
	decodeResponse(t, r, &got)
	if len(got.Notes.Items) != 21 || got.Notes.HasMore || !reflect.DeepEqual(got.Omitted, []string{"note_bodies", "message_bodies"}) {
		t.Fatal("explicit bounded page lost selected data")
	}
	if !reflect.DeepEqual(before, contextState(t, a)) {
		t.Fatal("bounded pages changed state")
	}
}
