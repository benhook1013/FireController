package client

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
)

// TransportPool shares transport and one random client connection label. It
// retains no binding, credential or authenticated client. Its owner closes it
// when that owner's session ends. The label grants no access.
type TransportPool struct {
	mu        sync.Mutex
	transport *http.Transport
	sessionID string
	closed    bool
}

func NewTransportPool() *TransportPool { return &TransportPool{} }

// NewWithTransportPool performs the same fresh credential validation and
// authenticated instance verification as New. Closing this client does not
// close the borrowed pool. A nil pool retains New's owned-transport behavior.
func NewWithTransportPool(ctx context.Context, binding profile.Binding, pool *TransportPool) (*Client, error) {
	return newClient(ctx, binding, pool)
}

func newTransport() *http.Transport {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second, ExpectContinueTimeout: time.Second}
	transport.DialContext = (&net.Dialer{Timeout: requestTimeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = requestTimeout
	transport.TLSHandshakeTimeout = requestTimeout
	transport.MaxConnsPerHost = 4
	transport.MaxIdleConns = 4
	transport.DisableCompression = true
	return transport
}

func (p *TransportPool) borrow() (*http.Transport, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, "", contextError(context.Canceled)
	}
	if p.transport == nil {
		var err error
		p.sessionID, err = newConnectionID()
		if err != nil {
			return nil, "", err
		}
		p.transport = newTransport()
	}
	return p.transport, p.sessionID, nil
}

func (p *TransportPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.transport != nil {
		p.transport.CloseIdleConnections()
	}
}
