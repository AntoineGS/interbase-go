package faultproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestPauseResponsesBlocksUntilReleased(t *testing.T) {
	target := newEchoServer(t)
	proxy, err := New(target.Addr().String())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	proxy.PauseResponses()
	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.WaitResponseBlocked(ctx); err != nil {
		t.Fatalf("wait for blocked response: %v", err)
	}

	readDone := make(chan error, 1)
	buffer := make([]byte, len("request"))
	go func() {
		_, err := io.ReadFull(client, buffer)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		t.Fatalf("response escaped paused proxy: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	proxy.ReleaseResponses()
	if err := <-readDone; err != nil {
		t.Fatalf("read released response: %v", err)
	}
	if got, want := string(buffer), "request"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
}

func TestResetConnectionsUnblocksPausedClient(t *testing.T) {
	target := newEchoServer(t)
	proxy, err := New(target.Addr().String())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	proxy.PauseResponses()
	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := proxy.WaitResponseBlocked(ctx); err != nil {
		t.Fatalf("wait for blocked response: %v", err)
	}

	proxy.ResetConnections()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	_, err = client.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("read succeeded after proxy reset")
	}
}

func TestCloseUnblocksWaitingForResponseBlocked(t *testing.T) {
	target := newEchoServer(t)
	proxy, err := New(target.Addr().String())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	proxy.PauseResponses()

	baseContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &waitStartContext{Context: baseContext, started: make(chan struct{})}
	waitDone := make(chan error, 1)
	go func() { waitDone <- proxy.WaitResponseBlocked(ctx) }()
	<-ctx.started

	proxy.Close()
	select {
	case err := <-waitDone:
		if err == nil {
			t.Fatal("WaitResponseBlocked succeeded after proxy Close")
		}
	case <-time.After(time.Second):
		cancel()
		t.Error("WaitResponseBlocked remained blocked after proxy Close")
	}
}

func TestCloseStopsListenerAndConnections(t *testing.T) {
	target := newEchoServer(t)
	proxy, err := New(target.Addr().String())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := proxy.Close(); err != nil {
		t.Fatalf("close proxy: %v", err)
	}
	if _, err := net.DialTimeout("tcp", proxy.Addr(), 100*time.Millisecond); err == nil {
		t.Fatal("dial succeeded after proxy Close")
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client remained open after proxy Close")
	}
}

func TestUpstreamCloseUnblocksWaitingClient(t *testing.T) {
	target := newClosingServer(t)
	proxy, err := New(target.Addr().String())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	readDone := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		readDone <- err
	}()

	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("client read succeeded after upstream close")
		}
	case <-time.After(time.Second):
		t.Error("client read remained blocked after upstream close")
	}
}

func TestCloseCancelsPendingDial(t *testing.T) {
	dialStarted := make(chan struct{})
	dialCanceled := make(chan struct{})
	proxy, err := newWithDialer("pending", func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(dialStarted)
		<-ctx.Done()
		close(dialCanceled)
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("newWithDialer: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("proxy did not start the pending target dial")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- proxy.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close remained blocked while target dial was pending")
	}

	select {
	case <-dialCanceled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the pending target dial")
	}
}

func TestResetConnectionsCancelsPendingDial(t *testing.T) {
	dialStarted := make(chan struct{})
	dialCanceled := make(chan struct{})
	proxy, err := newWithDialer("pending", func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(dialStarted)
		<-ctx.Done()
		close(dialCanceled)
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("newWithDialer: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("proxy did not start the pending target dial")
	}

	resetDone := make(chan struct{})
	go func() {
		proxy.ResetConnections()
		close(resetDone)
	}()
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("ResetConnections remained blocked while target dial was pending")
	}
	select {
	case <-dialCanceled:
	case <-time.After(time.Second):
		t.Fatal("ResetConnections did not cancel the pending target dial")
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("client read succeeded after ResetConnections")
		}
	case <-time.After(time.Second):
		t.Fatal("ResetConnections did not close the pending-dial client")
	}
}

func TestPendingDialHasBoundedDeadline(t *testing.T) {
	deadlineSeen := make(chan time.Time, 1)
	proxy, err := newWithDialer("pending", func(ctx context.Context, _, _ string) (net.Conn, error) {
		deadline, ok := ctx.Deadline()
		if ok {
			deadlineSeen <- deadline
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("newWithDialer: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	select {
	case deadline := <-deadlineSeen:
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > dialTimeout {
			t.Fatalf("dial deadline remaining = %s, want between 0 and %s", remaining, dialTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not start the target dial")
	}
}

func TestClientHalfCloseReachesUpstream(t *testing.T) {
	target, serverDone := newHalfCloseServer(t)
	proxy, err := New(target.Addr().String())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	client, err := net.Dial("tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	tcpClient, ok := client.(*net.TCPConn)
	if !ok {
		t.Fatalf("client type = %T, want *net.TCPConn", client)
	}
	if err := tcpClient.CloseWrite(); err != nil {
		t.Fatalf("close client write side: %v", err)
	}

	response, err := io.ReadAll(client)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if got, want := string(response), "response"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("half-close server: %v", err)
	}
}

func TestConcurrentResetAndClose(t *testing.T) {
	target := newEchoServer(t)
	proxy, err := New(target.Addr().String())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	clients := make([]net.Conn, 8)
	for index := range clients {
		client, err := net.Dial("tcp", proxy.Addr())
		if err != nil {
			t.Fatalf("dial proxy %d: %v", index, err)
		}
		clients[index] = client
		t.Cleanup(func() { _ = client.Close() })
		if _, err := client.Write([]byte("x")); err != nil {
			t.Fatalf("write request %d: %v", index, err)
		}
		if _, err := io.ReadFull(client, make([]byte, 1)); err != nil {
			t.Fatalf("read response %d: %v", index, err)
		}
	}

	start := make(chan struct{})
	closeDone := make(chan error, 1)
	var operations sync.WaitGroup
	operations.Add(2)
	go func() {
		defer operations.Done()
		<-start
		for range 100 {
			proxy.ResetConnections()
		}
	}()
	go func() {
		defer operations.Done()
		<-start
		closeDone <- proxy.Close()
	}()
	close(start)
	operations.Wait()
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func newEchoServer(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	return listener
}

func newClosingServer(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen closing server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		_ = connection.Close()
	}()
	return listener
}

func newHalfCloseServer(t *testing.T) (net.Listener, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen half-close server: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		request, err := io.ReadAll(connection)
		if err == nil && string(request) != "request" {
			err = errors.New("unexpected request")
		}
		if err == nil {
			_, err = connection.Write([]byte("response"))
		}
		done <- err
	}()
	return listener, done
}

type waitStartContext struct {
	context.Context
	started chan struct{}
	once    sync.Once
}

func (c *waitStartContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.started) })
	return c.Context.Done()
}
