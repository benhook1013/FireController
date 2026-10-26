package inbox

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/core"
)

// PendingPage is the ordinary compact inbox page, including its continuation.
type PendingPage = inboxPage

func inboxFilter(worker string, unread, unacknowledged bool) string {
	return "inbox:" + worker + ":unread=" + strconv.FormatBool(unread) + ":unacknowledged=" + strconv.FormatBool(unacknowledged)
}

func inboxOrderFilter(worker string, unread, unacknowledged, newest bool) string {
	filter := inboxFilter(worker, unread, unacknowledged)
	if newest {
		filter += ":newest=true"
	}
	return filter
}

// ReadPendingContext reads selected pending coordination and its replay watermark
// in an already-authorized transaction. It neither opens a transaction nor changes
// handling state. Its cursor continues through the ordinary inbox/snapshot read.
func ReadPendingContext(ctx context.Context, tx *sql.Tx, store *core.Store, scope core.Scope, limit int) (PendingPage, string, error) {
	if scope.WorkerID == "" || limit < 1 || limit > 100 {
		return PendingPage{}, "", core.Fail("validation_failed", "Select a bound worker and a bounded context page.", 400)
	}
	if err := checkInboxWorker(ctx, tx, scope.ProjectID, scope.WorkerID); err != nil {
		return PendingPage{}, "", err
	}
	h := handler{store: store}
	page, err := h.inboxPage(ctx, tx, scope, scope.WorkerID, false, true, false, inboxFilter(scope.WorkerID, false, true), limit, 0)
	if err != nil {
		return PendingPage{}, "", err
	}
	cursor, err := store.MessageEventCursor(ctx, tx, scope)
	return page, cursor, err
}

func validStoredInbox(item inboxItem, body, worker string) bool {
	at, err := time.Parse(time.RFC3339Nano, item.CreatedAt)
	return item.sequence > 0 && cleanID(item.ID) && utf8.ValidString(item.ID) && cleanID(item.ConversationID) && utf8.ValidString(item.ConversationID) && cleanID(item.SenderWorkerID) && utf8.ValidString(item.SenderWorkerID) && item.RecipientWorkerID == worker && len(body) <= MaxBodyBytes && utf8.ValidString(body) && strings.TrimSpace(body) != "" && err == nil && !at.IsZero() && len(item.CreatedAt) <= 64 && (!item.Acknowledged || item.Seen)
}
