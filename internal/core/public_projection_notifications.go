package core

import (
	"context"
	"sync"
)

const publicProjectionFeed = "notifications:public-projection"
const publicProjectionAnonymousPrincipal = "anonymous-public:"

func validPublicProjectionIdentity(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// MarkPublicProjectionChanged records a coalesced hint during a Mutate or
// BackgroundWrite callback. The owning transaction publishes it only after a
// successful commit. It inserts no durable event and grants no read authority.
func MarkPublicProjectionChanged(ctx context.Context, projectID string) error {
	if ctx == nil || !validPublicProjectionIdentity(projectID) {
		return Fail("validation_failed", "A valid project and active publication transaction are required.", 400)
	}
	if err := ctx.Err(); err != nil {
		return storageError(err)
	}
	changes, ok := ctx.Value(eventChangesKey{}).(*eventChanges)
	if !ok || changes == nil || changes.topics == nil {
		return Fail("validation_failed", "Public projection changes must be marked inside their owning write transaction.", 400)
	}
	changes.topics[messageTopic{project: projectID, feed: publicProjectionFeed}] = struct{}{}
	return nil
}

// SubscribePublicProjection provides hints only, never public data, revisions,
// cursors or authorization. The trusted caller must check current publication
// before every delivery and suppress unchanged projection bytes. Client keys
// are bounded opaque identifiers supplied by that caller, not public selectors.
func (s *Store) SubscribePublicProjection(projectID, anonymousClientKey string) (<-chan struct{}, func(), error) {
	if s == nil || !validPublicProjectionIdentity(projectID) || !validPublicProjectionIdentity(anonymousClientKey) {
		return nil, nil, Fail("validation_failed", "A valid project and bounded anonymous client key are required.", 400)
	}
	// The colon separates anonymous accounting from valid authenticated
	// principal IDs. One client's limit is shared across all public projects.
	scope := Scope{ProjectID: projectID, PrincipalID: publicProjectionAnonymousPrincipal + anonymousClientKey}
	waiter, release, err := s.addEventWaiter(scope, messageTopic{project: projectID, feed: publicProjectionFeed})
	if err != nil {
		return nil, nil, err
	}
	var once sync.Once
	return waiter.ready, func() { once.Do(release) }, nil
}
