package mcpadapter

import (
	"context"
	"sync"

	"github.com/benhook1013/FireController/internal/client"
	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Each actual SDK session owns one finite-request connection pool. Tool calls
// still construct and authenticate a fresh client; streams own their clones.
type sessionTransports struct {
	mu    sync.Mutex
	pools map[*mcp.ServerSession]*client.TransportPool
}

func (s *sessionTransports) newClient(ctx context.Context, binding profile.Binding, session *mcp.ServerSession) (*client.Client, error) {
	if ctx.Err() != nil || session == nil {
		return client.New(ctx, binding)
	}
	s.mu.Lock()
	pool := s.pools[session]
	if pool == nil {
		pool = client.NewTransportPool()
		s.pools[session] = pool
		go func() {
			_ = session.Wait()
			s.mu.Lock()
			delete(s.pools, session)
			s.mu.Unlock()
			pool.Close()
		}()
	}
	s.mu.Unlock()
	return client.NewWithTransportPool(ctx, binding, pool)
}
