// Package events provides explicitly owned native InterBase database events.
//
// A Subscription owns a separate database attachment. It does not borrow a
// database/sql connection or an interbase.Attachment, and it does not provide
// exactly-once delivery or per-event payloads. Event counts are accumulated
// until Next returns them or the subscription is closed.
package events

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	interbase "interbase-go"
)

// Config aliases the root package's attachment configuration so event
// subscriptions use the same validation and credential fields as SQL and
// direct attachments.
type Config = interbase.Config

var (
	// ErrClosed is returned when an operation is attempted after Close starts.
	ErrClosed = errors.New("interbase events: subscription is closed")
	// ErrCountOverflow indicates that an accumulated event count could not be
	// represented by uint64 without wrapping.
	ErrCountOverflow = errors.New("interbase events: event occurrence count overflow")
)

const (
	maxEventNames            = 1024
	maxEventNameBytes        = 127
	maxEventBufferBytes      = math.MaxInt16
	maxEventNamesPerBlock    = 15
	nextPollInterval         = 25 * time.Millisecond
	maxConnectTimeoutSeconds = uint64(^uint32(0))
)

// Subscription accumulates notifications for a fixed set of event names.
// Next calls are serialized; Close may run concurrently with Next.
type Subscription struct {
	mu           sync.Mutex
	cond         *sync.Cond
	nextSlot     chan struct{}
	backend      nativeBackend
	names        []string
	cfg          Config
	active       int
	closed       bool
	closeErr     error
	closeAttempt *closeAttempt
}

type closeAttempt struct {
	done chan struct{}
	err  error
}

// Subscribe attaches to cfg, queues the native event requests, and waits until
// their initial baselines are installed. Notification timing and grouping are
// controlled by the InterBase server and native client; delivery is not a
// row-level or exactly-once contract.
func Subscribe(ctx context.Context, cfg Config, names ...string) (*Subscription, error) {
	if ctx == nil {
		return nil, errors.New("interbase events: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateEventNames(names); err != nil {
		return nil, err
	}
	if _, err := interbase.NewConnector(cfg); err != nil {
		return nil, err
	}
	connectTimeout, err := normalizeConnectTimeout(cfg.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	cfg.ConnectTimeout = connectTimeout
	attachment := buildAttachment(cfg)
	charset := cfg.Charset
	if charset == "" {
		charset = "UTF8"
	} else {
		charset = strings.ToUpper(charset)
	}
	dialect := cfg.Dialect
	if dialect == 0 {
		dialect = 3
	}
	native, err := openNative(ctx, cfg, attachment, charset, dialect, names)
	if err != nil {
		if isContextError(err) {
			return nil, err
		}
		return nil, wrapNativeError("subscribe", err, cfg)
	}
	if err := waitForNativeReady(ctx, native); err != nil {
		cleanupErr := cleanupNative(native)
		if native.pointer != nil && cleanupErr != nil {
			_ = native.quarantine()
		}
		primary := err
		if !isContextError(err) {
			primary = wrapNativeError("wait for subscription readiness", err, cfg)
		}
		return nil, errors.Join(primary,
			wrapNativeError("cleanup subscription after readiness failure", cleanupErr, cfg))
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := cleanupNative(native)
		if native.pointer != nil && cleanupErr != nil {
			_ = native.quarantine()
		}
		return nil, errors.Join(err,
			wrapNativeError("cleanup subscription after context cancellation", cleanupErr, cfg))
	}
	return newSubscription(cfg, names, native), nil
}

func waitForNativeReady(ctx context.Context, backend nativeBackend) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := backend.ready(ctx, nextWait(ctx))
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
	}
}

func normalizeConnectTimeout(timeout time.Duration) (time.Duration, error) {
	if timeout < 0 {
		return 0, errors.New("interbase: connect timeout cannot be negative")
	}
	if timeout == 0 {
		return 0, nil
	}

	seconds := uint64(timeout / time.Second)
	if timeout%time.Second != 0 {
		seconds++
	}
	if seconds > maxConnectTimeoutSeconds {
		return 0, errors.New("interbase: connect timeout exceeds the native limit")
	}
	return time.Duration(seconds) * time.Second, nil
}

func cleanupNative(backend *nativeSubscription) error {
	if backend == nil {
		return nil
	}
	var cleanupErr error
	if err := backend.stop(); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := backend.destroy(); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	return cleanupErr
}

func newSubscription(cfg Config, names []string, backend nativeBackend) *Subscription {
	subscription := &Subscription{
		backend:  backend,
		names:    append([]string(nil), names...),
		cfg:      cfg,
		nextSlot: make(chan struct{}, 1),
	}
	subscription.cond = sync.NewCond(&subscription.mu)
	return subscription
}

// Next waits until at least one notification is available and returns all
// accumulated counts, including zero values for names that were not part of
// that notification. A canceled context only cancels this wait; it does not
// close the subscription or discard pending counts.
func (s *Subscription) Next(ctx context.Context) (map[string]uint64, error) {
	if s == nil {
		return nil, ErrClosed
	}
	if ctx == nil {
		return nil, errors.New("interbase events: nil context")
	}
	if err := s.acquireNext(ctx); err != nil {
		return nil, err
	}
	defer s.releaseNext()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		backend, err := s.acquire()
		if err != nil {
			return nil, err
		}
		ready, waitErr := backend.wait(ctx, nextWait(ctx))
		if waitErr != nil {
			s.release()
			if isContextError(waitErr) {
				return nil, waitErr
			}
			if s.isClosed() {
				return nil, ErrClosed
			}
			return nil, s.terminateAfterError("wait for events", waitErr)
		}
		if !ready {
			s.release()
			continue
		}
		if err := ctx.Err(); err != nil {
			s.release()
			return nil, err
		}
		if s.isClosed() {
			s.release()
			return nil, ErrClosed
		}
		counts, hasCounts, takeErr := backend.take(ctx, len(s.names))
		s.release()
		if takeErr != nil {
			if isContextError(takeErr) {
				return nil, takeErr
			}
			if s.isClosed() {
				return nil, ErrClosed
			}
			return nil, s.terminateAfterError("read event counts", takeErr)
		}
		if !hasCounts {
			continue
		}
		result := make(map[string]uint64, len(s.names))
		for index, name := range s.names {
			result[name] = counts[index]
		}
		return result, nil
	}
}

func (s *Subscription) acquireNext(ctx context.Context) error {
	if s.nextSlot == nil {
		return ErrClosed
	}
	select {
	case s.nextSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Subscription) releaseNext() {
	<-s.nextSlot
}

func (s *Subscription) terminateAfterError(operation string, err error) error {
	primary := wrapNativeError(operation, err, s.cfg)
	return errors.Join(primary, s.Close())
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (s *Subscription) acquire() (nativeBackend, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.backend == nil {
		return nil, ErrClosed
	}
	s.active++
	return s.backend, nil
}

func (s *Subscription) release() {
	s.mu.Lock()
	s.active--
	if s.active == 0 {
		s.cond.Broadcast()
	}
	s.mu.Unlock()
}

func (s *Subscription) isClosed() bool {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	return closed
}

// Close cancels native event requests, waits for in-flight Next calls and
// callbacks to drain, then detaches the owned database. It is idempotent after
// successful cleanup; a failed cleanup retains ownership so a later call can
// retry it.
func (s *Subscription) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closeAttempt != nil {
		attempt := s.closeAttempt
		s.mu.Unlock()
		<-attempt.done
		return attempt.err
	}
	if s.closed && s.backend == nil {
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	attempt := &closeAttempt{done: make(chan struct{})}
	s.closeAttempt = attempt
	s.closed = true
	backend := s.backend
	s.mu.Unlock()

	var firstErr error
	if backend != nil {
		if err := backend.stop(); err != nil {
			firstErr = errors.Join(firstErr, wrapNativeError("cancel events", err, s.cfg))
		}
	}

	s.mu.Lock()
	for s.active != 0 {
		s.cond.Wait()
	}
	s.mu.Unlock()

	if backend != nil {
		if err := backend.destroy(); err != nil {
			firstErr = errors.Join(firstErr, wrapNativeError("close events", err, s.cfg))
		}
	}
	s.mu.Lock()
	s.closeErr = firstErr
	if firstErr == nil {
		s.backend = nil
	}
	// Keep the native backend owned by this Subscription when cleanup fails.
	// Clearing the completed attempt permits an explicit Close retry while
	// callers already waiting on this attempt retain its immutable result.
	attempt.err = firstErr
	s.closeAttempt = nil
	close(attempt.done)
	s.mu.Unlock()
	return firstErr
}

func nextWait(ctx context.Context) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < nextPollInterval {
			if remaining < 0 {
				return 0
			}
			return remaining
		}
	}
	return nextPollInterval
}

func validateEventNames(names []string) error {
	if len(names) == 0 {
		return errors.New("interbase events: at least one event name is required")
	}
	if len(names) > maxEventNames {
		return fmt.Errorf("interbase events: too many event names: %d exceeds %d", len(names), maxEventNames)
	}
	seen := make(map[string]struct{}, len(names))
	blockBytes := 1 // version byte; each event adds its name byte and four counters
	for index, name := range names {
		if name == "" {
			return fmt.Errorf("interbase events: event name %d is empty", index)
		}
		if !utf8.ValidString(name) {
			return fmt.Errorf("interbase events: event name %d is not valid UTF-8", index)
		}
		if strings.IndexByte(name, 0) >= 0 {
			return fmt.Errorf("interbase events: event name %d contains a NUL byte", index)
		}
		if len(name) > maxEventNameBytes {
			return fmt.Errorf("interbase events: event name %d is too long", index)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("interbase events: duplicate event name %q", name)
		}
		seen[name] = struct{}{}
		if index%maxEventNamesPerBlock == 0 {
			blockBytes = 1
		}
		if len(name) > maxEventBufferBytes-blockBytes-5 {
			return errors.New("interbase events: encoded event names exceed native buffer limit")
		}
		blockBytes += len(name) + 5
	}
	return nil
}

func buildAttachment(cfg Config) string {
	if cfg.Host == "" {
		return cfg.Database
	}
	attachment := cfg.Host
	if cfg.TLS.Enabled {
		attachment += "?ssl=true"
		for _, option := range []struct {
			name  string
			value string
		}{
			{name: "serverPublicFile", value: cfg.TLS.ServerPublicFile},
			{name: "clientCertFile", value: cfg.TLS.ClientCertFile},
			{name: "clientPassPhrase", value: cfg.TLS.ClientPassPhrase},
			{name: "clientPassPhraseFile", value: cfg.TLS.ClientPassPhraseFile},
			{name: "serverPublicPath", value: cfg.TLS.ServerPublicPath},
		} {
			if option.value != "" {
				attachment += "?" + option.name + "=" + option.value
			}
		}
		attachment += "??"
	}
	if attachment != "" {
		attachment += ":"
	}
	return attachment + cfg.Database
}

func wrapNativeError(operation string, err error, cfg Config) error {
	if err == nil {
		return nil
	}
	message := redactSecrets(err.Error(), configSecrets(cfg)...)
	if strings.Contains(message, "event occurrence count overflow") {
		return fmt.Errorf("%w: %s", ErrCountOverflow, message)
	}
	const marker = " failed (SQLCODE "
	markerStart := strings.Index(message, marker)
	if markerStart < 0 {
		return fmt.Errorf("interbase events: %s: %s", operation, message)
	}
	remainder := message[markerStart+len(marker):]
	statusMarker := ", native status "
	statusStart := strings.Index(remainder, statusMarker)
	if statusStart < 0 {
		return fmt.Errorf("interbase events: %s: %s", operation, message)
	}
	closeIndex := strings.IndexByte(remainder[statusStart+len(statusMarker):], ')')
	if closeIndex < 0 {
		return fmt.Errorf("interbase events: %s: %s", operation, message)
	}
	closeIndex += statusStart + len(statusMarker)
	sqlCode, sqlErr := strconv.Atoi(strings.TrimSpace(remainder[:statusStart]))
	nativeCode, nativeErr := strconv.ParseInt(
		strings.TrimSpace(remainder[statusStart+len(statusMarker):closeIndex]), 10, 64)
	if sqlErr != nil || nativeErr != nil {
		return fmt.Errorf("interbase events: %s: %s", operation, message)
	}
	detail := strings.TrimSpace(remainder[closeIndex+1:])
	detail = strings.TrimSpace(strings.TrimPrefix(detail, ":"))
	return &interbase.Error{
		Operation:  operation,
		SQLCode:    sqlCode,
		NativeCode: nativeCode,
		Message:    detail,
	}
}

func configSecrets(cfg Config) []string {
	return []string{
		cfg.Database,
		cfg.Host,
		cfg.User,
		cfg.Password,
		cfg.Role,
		cfg.EncryptedPassword,
		cfg.SystemEncryptionPassword,
		cfg.TLS.ServerPublicFile,
		cfg.TLS.ClientCertFile,
		cfg.TLS.ClientPassPhrase,
		cfg.TLS.ClientPassPhraseFile,
		cfg.TLS.ServerPublicPath,
	}
}

func redactSecrets(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}
