package client

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/benhook1013/FireController/internal/coordimportmodel"
)

// Check bounded local message facts before credentials or transport. Resolving
// source parents and current workers remains the destination's responsibility.
func validPreparedMessages(items []coordimportmodel.PreparedMessage) bool {
	keys := map[string]bool{}
	key := func(s string) bool { return len(s) <= 200 && utf8.ValidString(s) && strings.TrimSpace(s) != "" }
	at := func(s string) (time.Time, bool) {
		v, e := time.Parse(time.RFC3339Nano, s)
		return v, len(s) <= 64 && e == nil && !v.IsZero()
	}
	for _, m := range items {
		if !key(m.SourceKey) || keys[m.SourceKey] || m.ReplyToSourceKey != "" && (!key(m.ReplyToSourceKey) || m.ReplyToSourceKey == m.SourceKey) || !validID(m.SenderWorkerID) || !validID(m.RecipientWorkerID) || !utf8.ValidString(m.Body) || strings.TrimSpace(m.Body) == "" || len(m.Body) > 16<<10 {
			return false
		}
		keys[m.SourceKey] = true
		created, ok := at(m.CreatedAt)
		if !ok {
			return false
		}
		var seen time.Time
		if m.SeenAt != "" {
			seen, ok = at(m.SeenAt)
			if !ok || seen.Before(created) {
				return false
			}
		}
		if m.AcknowledgedAt != "" {
			ack, valid := at(m.AcknowledgedAt)
			if !valid || seen.IsZero() || ack.Before(seen) {
				return false
			}
		}
	}
	return true
}
