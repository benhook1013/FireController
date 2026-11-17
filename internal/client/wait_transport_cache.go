package client

import (
	"net/http"
	"sync"
	"time"
)

// Only idle transports are bounded. A miss clones immediately instead of
// queuing behind another wait or using the ordinary control-read pool.
const maxIdleWaitTransports = 4

type idleWaitTransport struct {
	timeout   time.Duration
	transport *http.Transport
}

type waitTransportCache struct {
	mu     sync.Mutex
	idle   []idleWaitTransport // oldest release first
	closed bool
}

func (p *waitTransportCache) lease(base *http.Transport, timeout time.Duration) (*http.Transport, func()) {
	var transport *http.Transport
	if p != nil {
		p.mu.Lock()
		if !p.closed {
			for i := len(p.idle) - 1; i >= 0; i-- {
				if p.idle[i].timeout == timeout {
					transport = p.idle[i].transport
					copy(p.idle[i:], p.idle[i+1:])
					p.idle[len(p.idle)-1] = idleWaitTransport{}
					p.idle = p.idle[:len(p.idle)-1]
					break
				}
			}
		}
		p.mu.Unlock()
	}
	if transport == nil {
		transport = base.Clone()
		transport.ResponseHeaderTimeout = timeout
	}
	var once sync.Once
	return transport, func() { once.Do(func() { p.release(idleWaitTransport{timeout, transport}) }) }
}

func (p *waitTransportCache) release(entry idleWaitTransport) {
	if p == nil {
		entry.transport.CloseIdleConnections()
		return
	}
	var evicted *http.Transport
	p.mu.Lock()
	if p.closed {
		evicted = entry.transport
	} else {
		if len(p.idle) == maxIdleWaitTransports {
			evicted = p.idle[0].transport
			p.idle = append(p.idle[:0], p.idle[1:]...)
		}
		p.idle = append(p.idle, entry)
	}
	p.mu.Unlock()
	if evicted != nil {
		evicted.CloseIdleConnections()
	}
}

func (p *waitTransportCache) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, entry := range idle {
		entry.transport.CloseIdleConnections()
	}
}
