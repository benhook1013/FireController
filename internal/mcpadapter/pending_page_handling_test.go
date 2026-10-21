package mcpadapter_test

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/benhook1013/FireController/internal/client"
)

// Workers normally handle records while traversing a pending page. The cursor
// must seek past handled records rather than use shifting numeric offsets.
func TestPendingPagesRemainCompleteWhileHandlingWork(t *testing.T) {
	f := mutationFixture(t)
	service := httptest.NewServer(f.handler)
	defer service.Close()
	sender := connect(t, f.adapter(t, service.URL, f.sender), "")
	worker := connect(t, f.adapter(t, service.URL, f.recipient), "")
	wantMessages, wantJobs, wantNotes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var last client.MessageReceipt
	for i := 0; i < 23; i++ {
		message := map[string]any{"idempotency_key": fmt.Sprintf("paging-message-%d", i), "recipient_worker_id": f.recipient, "body": fmt.Sprintf("Distinct instruction %d", i)}
		if last.ID != "" {
			message["reply_to_message_id"] = last.ID
		}
		decodeResult(t, call(t, sender, "inbox_send", message), &last)
		wantMessages[last.ID] = true
		var job client.JobReceipt
		decodeResult(t, call(t, sender, "jobs_create", map[string]any{"idempotency_key": fmt.Sprintf("paging-job-%d", i), "title": fmt.Sprintf("Assignment %d", i), "worker_id": f.recipient}), &job)
		wantJobs[job.ID] = true
		var note client.NoteReceipt
		decodeResult(t, call(t, sender, "note_create", map[string]any{"idempotency_key": fmt.Sprintf("paging-note-%d", i), "worker_id": f.recipient, "body": fmt.Sprintf("Reminder %d", i)}), &note)
		wantNotes[note.ID] = true
	}
	var snapshot client.InboxSnapshot
	decodeResult(t, call(t, worker, "inbox_snapshot", map[string]any{"limit": 7}), &snapshot)
	initialEventCursor := snapshot.EventCursor
	seenMessages, seenJobs, seenNotes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	messagePage := snapshot.Inbox
	for page := 0; ; page++ {
		for _, message := range messagePage.Items {
			if !wantMessages[message.ID] || seenMessages[message.ID] || message.Acknowledged {
				t.Fatal("pending page duplicated or returned another message")
			}
			seenMessages[message.ID] = true
			var receipt client.MessageReceipt
			decodeResult(t, call(t, worker, "inbox_seen", map[string]any{"idempotency_key": "paging-seen-" + message.ID, "message_id": message.ID}), &receipt)
			decodeResult(t, call(t, worker, "inbox_ack", map[string]any{"idempotency_key": "paging-ack-" + message.ID, "message_id": message.ID}), &receipt)
		}
		if page == 1 {
			decodeResult(t, call(t, sender, "inbox_send", map[string]any{"idempotency_key": "paging-late-message", "recipient_worker_id": f.recipient, "body": "Arrived while handling backlog", "reply_to_message_id": last.ID}), &last)
			wantMessages[last.ID] = true
		}
		if !messagePage.HasMore {
			break
		}
		decodeResult(t, call(t, worker, "inbox_list", map[string]any{"limit": 7, "cursor": messagePage.Cursor}), &messagePage)
	}
	if len(seenMessages) != len(wantMessages) {
		t.Fatal("acknowledging current pages skipped later pending messages", len(seenMessages))
	}
	for _, work := range []string{"jobs", "notes"} {
		cursor := ""
		for {
			args := map[string]any{"limit": 7}
			if cursor != "" {
				args["cursor"] = cursor
			}
			hasMore := false
			if work == "jobs" {
				var page client.Page[client.JobSummary]
				decodeResult(t, call(t, worker, "jobs_assigned", args), &page)
				for _, job := range page.Items {
					if !wantJobs[job.ID] || seenJobs[job.ID] {
						t.Fatal("assignment page duplicated or crossed selection")
					}
					seenJobs[job.ID] = true
					var receipt client.JobReceipt
					decodeResult(t, call(t, worker, "job_update", map[string]any{"job_id": job.ID, "idempotency_key": "paging-complete-" + job.ID, "expected_revision": job.Revision, "status": "completed"}), &receipt)
				}
				cursor, hasMore = page.Cursor, page.HasMore
			} else {
				var page client.Page[client.NoteSummary]
				decodeResult(t, call(t, worker, "note_list", args), &page)
				for _, note := range page.Items {
					if !wantNotes[note.ID] || seenNotes[note.ID] {
						t.Fatal("reminder page duplicated or crossed selection")
					}
					seenNotes[note.ID] = true
					var receipt client.NoteReceipt
					decodeResult(t, call(t, worker, "note_update", map[string]any{"note_id": note.ID, "idempotency_key": "paging-consume-" + note.ID, "expected_revision": note.Revision, "status": "consumed"}), &receipt)
				}
				cursor, hasMore = page.Cursor, page.HasMore
			}
			if !hasMore {
				break
			}
		}
	}
	if len(seenJobs) != 23 || len(seenNotes) != 23 {
		t.Fatal("handling current page skipped later work", len(seenJobs), len(seenNotes))
	}
	// Snapshot replay remains independently complete despite subsequent inbox
	// receipt changes. Follow bounded event pages rather than assuming one page.
	eventCursor, sent := initialEventCursor, 0
	for {
		var page client.MessageEventPage
		decodeResult(t, call(t, worker, "inbox_events", map[string]any{"cursor": eventCursor, "limit": 3}), &page)
		for _, event := range page.Items {
			if event.Type == "message.sent" {
				if event.ResourceID != last.ID {
					t.Fatal("event replay returned pre-snapshot or foreign send")
				}
				sent++
			}
		}
		eventCursor = page.Cursor
		if !page.HasMore {
			break
		}
	}
	if sent != 1 || f.providers.Load() != 1 {
		t.Fatal("snapshot replay missed concurrent arrival or contacted providers", sent)
	}
	t.Log("23 assignments, 23 reminders and 24 messages were handled across 7-item pages without skips/duplicates; 3-item event replay retained the concurrent send")
}
