package events

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestNormalizeConnectTimeoutRoundsUpToWholeNativeSeconds(t *testing.T) {
	maxSeconds := uint64(^uint32(0))
	maxTimeout := time.Duration(maxSeconds) * time.Second
	tests := []struct {
		name  string
		input time.Duration
		want  time.Duration
	}{
		{name: "zero preserves native default", input: 0, want: 0},
		{name: "exact second", input: 3 * time.Second, want: 3 * time.Second},
		{name: "fraction rounds upward", input: 1500 * time.Millisecond, want: 2 * time.Second},
		{name: "maximum native timeout", input: maxTimeout, want: maxTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeConnectTimeout(test.input)
			if err != nil {
				t.Fatalf("normalizeConnectTimeout(%v) returned error: %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("normalizeConnectTimeout(%v) = %v, want %v", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeConnectTimeoutRejectsInvalidValues(t *testing.T) {
	maxTimeout := time.Duration(uint64(^uint32(0))) * time.Second
	for _, test := range []struct {
		name  string
		input time.Duration
	}{
		{name: "negative", input: -time.Nanosecond},
		{name: "exceeds native uint32 seconds", input: maxTimeout + time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := normalizeConnectTimeout(test.input); got != 0 || err == nil {
				t.Fatalf("normalizeConnectTimeout(%v) = (%v, %v), want validation error", test.input, got, err)
			}
		})
	}
}

func TestSubscribeConnectTimeoutBoundsOwnedUnresponsiveListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer listener.Close()

	address := listener.Addr().String()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		accepted <- connection
	}()

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("net.SplitHostPort() error = %v", err)
	}
	started := time.Now()
	_, subscribeErr := Subscribe(context.Background(), Config{
		Database:       "/tmp/interbase-go-events-connect-timeout.ib",
		Host:           host + "/" + port,
		User:           "SYSDBA",
		Password:       "masterkey",
		ConnectTimeout: time.Second,
	}, "event_a")
	elapsed := time.Since(started)
	select {
	case connection := <-accepted:
		if err := connection.Close(); err != nil {
			t.Fatalf("close unresponsive listener connection: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("native attach did not reach the test listener")
	}
	if subscribeErr == nil {
		t.Fatal("Subscribe() unexpectedly succeeded against an unresponsive listener")
	}
	if elapsed < 750*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("Subscribe() elapsed %v, want approximately the configured 1s timeout", elapsed)
	}
}
