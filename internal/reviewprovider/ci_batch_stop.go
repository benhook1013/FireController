package reviewprovider

import (
	"context"
	"errors"
	"sync"
)

// CIBatchAccountFailure is account-level, not evidence that another member failed.
// Scheduler callers discard all prepared results and apply ordinary account backoff.
type CIBatchAccountFailure struct{ Cause error }

func (e *CIBatchAccountFailure) Error() string {
	return "The prepared CI batch stopped after an account-level provider failure."
}
func (e *CIBatchAccountFailure) Unwrap() error { return e.Cause }

type ciBatchStopKey struct{}
type ciBatchStop struct {
	mu     sync.Mutex
	cause  error
	cancel context.CancelFunc
}

// Mirrors lifecycleInventoryInconclusive. Default provider calls have no scope.
func noteCIBatchFailure(ctx context.Context, err error) {
	s, _ := ctx.Value(ciBatchStopKey{}).(*ciBatchStop)
	if s == nil || err == nil {
		return
	}
	var failure *Error
	if errors.As(err, &failure) && (failure.Code == "provider_invalid_response" || failure.Code == "external_hosted_identity_unproven") && failure.RetryAfterSeconds <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cause == nil {
		s.cause = err
		s.cancel()
	}
}
func (s *ciBatchStop) failure() error { s.mu.Lock(); defer s.mu.Unlock(); return s.cause }
