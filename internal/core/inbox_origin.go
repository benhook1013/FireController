package core

import (
	"context"
	"net/http"
)

const InboxOriginHeader = "X-FC-Inbox-Origin"

type inboxOriginKey struct{}

// This selects a read projection only; it grants no project or worker authority.
func WithControllerInbox(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, inboxOriginKey{}, enabled)
}
func IncludesControllerInbox(ctx context.Context) bool {
	v, _ := ctx.Value(inboxOriginKey{}).(bool)
	return v
}
func RequestsControllerInbox(r *http.Request) bool {
	return len(r.Header.Values(InboxOriginHeader)) == 1 && r.Header.Get(InboxOriginHeader) == "controller-v1"
}
