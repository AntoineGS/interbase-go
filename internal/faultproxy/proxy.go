// Package faultproxy provides a loopback-only TCP proxy for fault-injection tests.
package faultproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const dialTimeout = 5 * time.Second

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// Proxy relays TCP traffic to one target while allowing tests to pause server
// responses or tear down every active connection deterministically.
type Proxy struct {
	listener net.Listener
	target   string
	dial     dialContextFunc
	ctx      context.Context
	cancel   context.CancelFunc

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	cancels     map[net.Conn]context.CancelFunc
	paused      bool
	release     chan struct{}
	blocked     chan struct{}
	blockOnce   sync.Once
	reset       chan struct{}
	closed      bool

	closeOnce sync.Once
	wg        sync.WaitGroup
}

// New starts a loopback-only proxy that relays connections to target.
func New(target string) (*Proxy, error) {
	return newWithDialer(target, (&net.Dialer{}).DialContext)
}

func newWithDialer(target string, dial dialContextFunc) (*Proxy, error) {
	if target == "" {
		return nil, errors.New("fault proxy: target is required")
	}
	if dial == nil {
		return nil, errors.New("fault proxy: dialer is required")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("fault proxy: listen: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	proxy := &Proxy{
		listener:    listener,
		target:      target,
		dial:        dial,
		ctx:         ctx,
		cancel:      cancel,
		connections: make(map[net.Conn]struct{}),
		cancels:     make(map[net.Conn]context.CancelFunc),
		blocked:     make(chan struct{}),
		reset:       make(chan struct{}),
	}
	proxy.wg.Add(1)
	go proxy.accept()
	return proxy, nil
}

// Addr returns the loopback listener address.
func (p *Proxy) Addr() string { return p.listener.Addr().String() }

// PauseResponses holds target-to-client bytes after the first such byte reaches
// the proxy. WaitResponseBlocked provides an ordering barrier for the hold.
func (p *Proxy) PauseResponses() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.paused {
		return
	}
	p.paused = true
	p.release = make(chan struct{})
	p.blocked = make(chan struct{})
	p.blockOnce = sync.Once{}
}

// WaitResponseBlocked waits until a target response has reached a paused proxy.
func (p *Proxy) WaitResponseBlocked(ctx context.Context) error {
	p.mu.Lock()
	if !p.paused {
		p.mu.Unlock()
		return errors.New("fault proxy: responses are not paused")
	}
	blocked := p.blocked
	release := p.release
	reset := p.reset
	p.mu.Unlock()
	select {
	case <-blocked:
		return nil
	case <-release:
		return errors.New("fault proxy: responses were released before being blocked")
	case <-reset:
		return errors.New("fault proxy: connection reset while waiting for response")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseResponses resumes a paused target-to-client direction.
func (p *Proxy) ReleaseResponses() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.paused {
		return
	}
	p.paused = false
	close(p.release)
}

// ResetConnections closes every active client and target socket, making an
// in-flight native operation observe a real network failure.
func (p *Proxy) ResetConnections() {
	p.mu.Lock()
	p.paused = false
	close(p.reset)
	p.reset = make(chan struct{})
	connections := make([]net.Conn, 0, len(p.connections))
	cancels := make([]context.CancelFunc, 0, len(p.cancels))
	for _, cancel := range p.cancels {
		cancels = append(cancels, cancel)
	}
	for connection := range p.connections {
		connections = append(connections, connection)
	}
	for _, cancel := range cancels {
		cancel()
	}
	p.mu.Unlock()
	for _, connection := range connections {
		_ = connection.Close()
	}
}

// Close stops accepting and tears down only connections owned by this proxy.
func (p *Proxy) Close() error {
	var closeErr error
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.cancel()
		if p.paused {
			p.paused = false
			close(p.release)
		}
		p.mu.Unlock()
		closeErr = p.listener.Close()
		p.ResetConnections()
		p.wg.Wait()
	})
	return closeErr
}

func (p *Proxy) accept() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(p.ctx)
		if !p.registerClient(client, ctx, cancel) {
			cancel()
			continue
		}
		p.wg.Add(1)
		go p.serve(ctx, cancel, client)
	}
}

func (p *Proxy) serve(ctx context.Context, cancel context.CancelFunc, client net.Conn) {
	defer p.wg.Done()
	defer cancel()
	defer func() {
		p.remove(client)
		_ = client.Close()
	}()

	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	target, err := p.dial(dialCtx, "tcp", p.target)
	dialCancel()
	if err != nil {
		return
	}
	if !p.add(target, ctx) {
		return
	}
	defer func() {
		p.remove(target)
		_ = target.Close()
	}()

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(target, client)
		closeWrite(target)
		close(done)
	}()
	_, _ = p.copyResponses(client, target)
	closeWrite(client)
	_ = client.Close()
	_ = target.Close()
	<-done
}

func (p *Proxy) copyResponses(client net.Conn, target net.Conn) (int64, error) {
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		read, readErr := target.Read(buffer)
		if read > 0 {
			if err := p.waitIfPaused(); err != nil {
				return total, err
			}
			written, writeErr := client.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func (p *Proxy) waitIfPaused() error {
	p.mu.Lock()
	if !p.paused {
		p.mu.Unlock()
		return nil
	}
	release := p.release
	blocked := p.blocked
	reset := p.reset
	p.blockOnce.Do(func() { close(blocked) })
	p.mu.Unlock()
	select {
	case <-release:
		return nil
	case <-reset:
		return errors.New("fault proxy: connection reset while response was paused")
	}
}

func (p *Proxy) registerClient(client net.Conn, ctx context.Context, cancel context.CancelFunc) bool {
	p.mu.Lock()
	if p.closed || ctx.Err() != nil {
		p.mu.Unlock()
		_ = client.Close()
		return false
	}
	p.connections[client] = struct{}{}
	p.cancels[client] = cancel
	p.mu.Unlock()
	return true
}

func (p *Proxy) add(connection net.Conn, ctx context.Context) bool {
	p.mu.Lock()
	if p.closed || ctx.Err() != nil {
		p.mu.Unlock()
		_ = connection.Close()
		return false
	}
	p.connections[connection] = struct{}{}
	p.mu.Unlock()
	return true
}

func (p *Proxy) remove(connection net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.connections, connection)
	delete(p.cancels, connection)
}

func closeWrite(connection net.Conn) {
	halfCloser, ok := connection.(interface{ CloseWrite() error })
	if !ok {
		return
	}
	_ = halfCloser.CloseWrite()
}
