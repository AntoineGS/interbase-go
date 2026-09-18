package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	interbase "interbase-go"
	"interbase-go/internal/nativegate"
)

const servicePollInterval = 25 * time.Millisecond

// Manager owns one native Services Manager attachment. The native API permits
// only one active service action per attachment, so Manager serializes all
// operations and rejects a second asynchronous action until the first Job is
// drained or closed.
type Manager struct {
	mu              sync.Mutex
	backend         nativeBackend
	target          string
	secrets         []string
	active          *Job
	closing         bool
	closed          bool
	closeInProgress bool
	closeAttempt    *closeAttempt
}

type closeAttempt struct {
	done   chan struct{}
	result error
}

// closeWaitHooks are used only by package tests to make the waiter/retry
// ordering deterministic. A closeAttempt result is published before its done
// channel is closed and is never changed after completion.
type closeWaitHooks struct {
	afterCapture func()
	beforeResult func()
}

// ServiceManager and Connection are descriptive aliases for Manager. They are
// provided to make the native API easy to discover for callers familiar with
// the InterBase and Python service APIs.
type ServiceManager = Manager
type Connection = Manager

// Open attaches to the InterBase Services Manager using the official native
// client library.
func Open(ctx context.Context, cfg Config) (*Manager, error) {
	if ctx == nil {
		return nil, errors.New("interbase services: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := buildServiceTarget(cfg)
	if err != nil {
		return nil, err
	}
	spb, err := buildServiceSPB(cfg)
	if err != nil {
		return nil, err
	}
	backend, err := openNativeServiceContext(ctx, target, spb)
	secrets := []string{cfg.Password, cfg.EncryptedPassword,
		cfg.SystemEncryptionPassword, cfg.TLS.ClientPassPhrase}
	if err != nil {
		serviceErr := newServiceError("attach service manager", err, secrets...)
		if backend != nil {
			// A failed native attach can still own a live handle. Quarantine the
			// returned Manager so callers can use Close to retry cleanup rather
			// than losing that native owner behind a nil error result.
			return &Manager{backend: backend, target: target, secrets: secrets,
				closing: true}, serviceErr
		}
		return nil, serviceErr
	}
	return &Manager{
		backend: backend,
		target:  target,
		secrets: secrets,
	}, nil
}

// Connect is an alias for Open.
func Connect(ctx context.Context, cfg Config) (*Manager, error) { return Open(ctx, cfg) }

// Target returns a diagnostic form of the native service-manager target. Any
// credential-bearing attachment options are redacted; the private target used
// for the native attach is never exposed.
func (m *Manager) Target() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return redactServiceSecrets(m.target, m.secrets...)
}

// Close detaches the native Services Manager. If an action is still running or
// its completion is uncertain, Close drains what the client can observe before
// detaching; it never attempts to cancel or claim completion of the native
// operation.
func (m *Manager) Close() error {
	return m.closeWithWaitHooks(closeWaitHooks{})
}

func (m *Manager) closeWithWaitHooks(hooks closeWaitHooks) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	if m.closeInProgress {
		attempt := m.closeAttempt
		m.mu.Unlock()
		if hooks.afterCapture != nil {
			hooks.afterCapture()
		}
		<-attempt.done
		if hooks.beforeResult != nil {
			hooks.beforeResult()
		}
		return attempt.result
	}
	m.closing = true
	m.closeInProgress = true
	attempt := &closeAttempt{done: make(chan struct{})}
	m.closeAttempt = attempt
	job := m.active
	m.mu.Unlock()

	var firstErr error
	if job != nil {
		firstErr = job.Close()
	}

	m.mu.Lock()
	backend := m.backend
	secrets := append([]string(nil), m.secrets...)
	m.mu.Unlock()

	if backend == nil {
		m.mu.Lock()
		m.closed = true
		m.closeInProgress = false
		attempt.result = firstErr
		m.closeAttempt = nil
		close(attempt.done)
		m.mu.Unlock()
		return firstErr
	}
	if err := backend.close(); err != nil {
		firstErr = errors.Join(firstErr, newServiceError("detach service manager", err, secrets...))
		// Keep the backend owned by this Manager. A failed detach may have
		// left the native service handle live, or may have consumed it while
		// returning an error; either state must be resolved by a later Close
		// attempt rather than being silently forgotten.
		m.mu.Lock()
		m.closing = true
		m.closed = false
		m.closeInProgress = false
		attempt.result = firstErr
		m.closeAttempt = nil
		close(attempt.done)
		m.mu.Unlock()
		return firstErr
	}

	m.mu.Lock()
	m.backend = nil
	m.closed = true
	m.closeInProgress = false
	attempt.result = firstErr
	m.closeAttempt = nil
	close(attempt.done)
	m.mu.Unlock()
	return firstErr
}

// Start validates and starts one typed asynchronous service action. Context is
// used to prevent starting work after cancellation. Once the native action has
// started, cancellation is reported by Job.Wait but is not sent to the native
// client as a cancellation request.
func (m *Manager) Start(ctx context.Context, request Request) (*Job, error) {
	if ctx == nil {
		return nil, errors.New("interbase services: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, errors.New("interbase services: nil service request")
	}
	requestBytes, err := request.build()
	if err != nil {
		return nil, err
	}

	if err := m.lockContext(ctx); err != nil {
		return nil, err
	}
	job, err := m.startLocked(ctx, requestBytes, request, "start service action")
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go job.run()
	return job, nil
}

func (m *Manager) startLocked(ctx context.Context, requestBytes []byte, request Request, operation string) (*Job, error) {
	if m.closed || m.closing || m.backend == nil {
		return nil, ErrClosed
	}
	if m.active != nil {
		return nil, m.activeErrorLocked()
	}
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	releaseNative, err := nativegate.Global.EnterContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := m.backend.start(requestBytes); err != nil {
		return nil, newServiceError(operation, err, m.secrets...)
	}
	job := newJob(m, request)
	m.active = job
	return job, nil
}

func (m *Manager) activeErrorLocked() error {
	if m.active != nil && m.active.completionUnknown() {
		return ErrCompletionUnknown
	}
	return ErrJobActive
}

// Logs starts retrieval of the server log.
func (m *Manager) Logs(ctx context.Context) (*Job, error) { return m.Start(ctx, LogRequest{}) }

// GetLog is a compatibility spelling for Logs.
func (m *Manager) GetLog(ctx context.Context) (*Job, error) { return m.Logs(ctx) }

// DatabaseStatistics starts a bounded streaming database statistics report.
func (m *Manager) DatabaseStatistics(ctx context.Context, request DatabaseStatisticsRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// GetStatistics is a compatibility spelling for DatabaseStatistics.
func (m *Manager) GetStatistics(ctx context.Context, request DatabaseStatisticsRequest) (*Job, error) {
	return m.DatabaseStatistics(ctx, request)
}

// Backup starts a logical database backup.
func (m *Manager) Backup(ctx context.Context, request BackupRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// Restore starts a logical database restore.
func (m *Manager) Restore(ctx context.Context, request RestoreRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// Dump starts a server-side database dump.
func (m *Manager) Dump(ctx context.Context, request DumpRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// ArchiveBackup starts an archive database or journal backup.
func (m *Manager) ArchiveBackup(ctx context.Context, request ArchiveBackupRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// ArchiveRestore starts an archive recovery operation.
func (m *Manager) ArchiveRestore(ctx context.Context, request ArchiveRestoreRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// BackupTablespace starts a tablespace backup.
func (m *Manager) BackupTablespace(ctx context.Context, request TablespaceBackupRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// RestoreTablespace starts a tablespace restore.
func (m *Manager) RestoreTablespace(ctx context.Context, request TablespaceRestoreRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// Validate starts database validation or repair.
func (m *Manager) Validate(ctx context.Context, request ValidationRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// Sweep starts a database sweep.
func (m *Manager) Sweep(ctx context.Context, request SweepRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// ListLimbo starts retrieval of limbo transaction information.
func (m *Manager) ListLimbo(ctx context.Context, request LimboTransactionsRequest) (*Job, error) {
	return m.Start(ctx, request)
}

// CreateDump is a convenience wrapper for Dump.
func (m *Manager) CreateDump(ctx context.Context, database, dumpFile string, overwrite bool) (*Job, error) {
	return m.Dump(ctx, DumpRequest{Database: database, DumpFile: dumpFile, Overwrite: overwrite})
}

// SetPageBuffers applies a database page-cache setting and waits for the
// native action to finish.
func (m *Manager) SetPageBuffers(ctx context.Context, request PageBuffersRequest) error {
	return m.runAndDrain(ctx, request)
}

// SetSweepInterval applies the automatic sweep threshold.
func (m *Manager) SetSweepInterval(ctx context.Context, request SweepIntervalRequest) error {
	return m.runAndDrain(ctx, request)
}

// SetReserveSpace applies the data-page reserve policy.
func (m *Manager) SetReserveSpace(ctx context.Context, request ReserveSpaceRequest) error {
	return m.runAndDrain(ctx, request)
}

// SetWriteMode applies forced or buffered writes.
func (m *Manager) SetWriteMode(ctx context.Context, request WriteModeRequest) error {
	return m.runAndDrain(ctx, request)
}

// SetAccessMode applies read-only or read-write access.
func (m *Manager) SetAccessMode(ctx context.Context, request AccessModeRequest) error {
	return m.runAndDrain(ctx, request)
}

// SetSQLDialect applies a database SQL dialect.
func (m *Manager) SetSQLDialect(ctx context.Context, request SQLDialectRequest) error {
	return m.runAndDrain(ctx, request)
}

// ActivateShadow activates database shadows.
func (m *Manager) ActivateShadow(ctx context.Context, request ActivateShadowRequest) error {
	return m.runAndDrain(ctx, request)
}

// Shutdown shuts down a database using the requested documented mode.
func (m *Manager) Shutdown(ctx context.Context, request ShutdownRequest) error {
	return m.runAndDrain(ctx, request)
}

// BringOnline brings a shut-down database online.
func (m *Manager) BringOnline(ctx context.Context, request OnlineRequest) error {
	return m.runAndDrain(ctx, request)
}

// CommitLimbo resolves one limbo transaction by committing it.
func (m *Manager) CommitLimbo(ctx context.Context, request ResolveLimboRequest) error {
	request.Commit = true
	return m.runAndDrain(ctx, request)
}

// RollbackLimbo resolves one limbo transaction by rolling it back.
func (m *Manager) RollbackLimbo(ctx context.Context, request ResolveLimboRequest) error {
	request.Commit = false
	return m.runAndDrain(ctx, request)
}

// ServerInfo returns the read-only server information exposed by the native
// Services API.
func (m *Manager) ServerInfo(ctx context.Context) (ServerInfo, error) {
	if err := checkContext(ctx); err != nil {
		return ServerInfo{}, err
	}
	if err := m.lockContext(ctx); err != nil {
		return ServerInfo{}, err
	}
	defer m.mu.Unlock()
	if err := m.ensureIdleLocked(); err != nil {
		return ServerInfo{}, err
	}

	result := ServerInfo{}
	var err error
	if result.ServiceManagerVersion, err = m.queryIntLocked(ctx, infoVersion); err != nil {
		return ServerInfo{}, newServiceError("read service manager version", err, m.secrets...)
	}
	if result.ServerVersion, err = m.queryStringLocked(ctx, infoServerVersion); err != nil {
		return ServerInfo{}, newServiceError("read server version", err, m.secrets...)
	}
	if result.Architecture, err = m.queryStringLocked(ctx, infoImplementation); err != nil {
		return ServerInfo{}, newServiceError("read server architecture", err, m.secrets...)
	}
	if result.HomeDirectory, err = m.queryStringLocked(ctx, infoHomeDirectory); err != nil {
		return ServerInfo{}, newServiceError("read home directory", err, m.secrets...)
	}
	if result.SecurityDatabasePath, err = m.queryStringLocked(ctx, infoSecurityPath); err != nil {
		return ServerInfo{}, newServiceError("read security database path", err, m.secrets...)
	}
	if result.LockFileDirectory, err = m.queryStringLocked(ctx, infoLockDirectory); err != nil {
		return ServerInfo{}, newServiceError("read lock-file directory", err, m.secrets...)
	}
	if result.MessageFileDirectory, err = m.queryStringLocked(ctx, infoMessageDirectory); err != nil {
		return ServerInfo{}, newServiceError("read message-file directory", err, m.secrets...)
	}
	var capabilityValue int
	if capabilityValue, err = m.queryIntLocked(ctx, infoCapabilities); err != nil {
		return ServerInfo{}, newServiceError("read server capabilities", err, m.secrets...)
	}
	result.Capabilities = uint32(capabilityValue)
	serverDatabase, err := m.queryServerDatabaseLocked(ctx)
	if err != nil {
		return ServerInfo{}, newServiceError("read server database information", err, m.secrets...)
	}
	result.ConnectionCount = serverDatabase.Attachments
	result.AttachedDatabases = serverDatabase.Databases
	return result, nil
}

// ServiceManagerVersion returns the Services Manager protocol version.
func (m *Manager) ServiceManagerVersion(ctx context.Context) (int, error) {
	return m.queryInt(ctx, infoVersion, "read service manager version")
}

// ServerVersion returns the InterBase server version string.
func (m *Manager) ServerVersion(ctx context.Context) (string, error) {
	return m.queryString(ctx, infoServerVersion, "read server version")
}

// Architecture returns the server implementation string.
func (m *Manager) Architecture(ctx context.Context) (string, error) {
	return m.queryString(ctx, infoImplementation, "read server architecture")
}

// HomeDirectory returns the server's InterBase installation directory.
func (m *Manager) HomeDirectory(ctx context.Context) (string, error) {
	return m.queryString(ctx, infoHomeDirectory, "read home directory")
}

// SecurityDatabasePath returns the server security database path.
func (m *Manager) SecurityDatabasePath(ctx context.Context) (string, error) {
	return m.queryString(ctx, infoSecurityPath, "read security database path")
}

// LockFileDirectory returns the server lock-file directory.
func (m *Manager) LockFileDirectory(ctx context.Context) (string, error) {
	return m.queryString(ctx, infoLockDirectory, "read lock-file directory")
}

// MessageFileDirectory returns the server message-file directory.
func (m *Manager) MessageFileDirectory(ctx context.Context) (string, error) {
	return m.queryString(ctx, infoMessageDirectory, "read message-file directory")
}

// Capabilities returns the server capability bitmask.
func (m *Manager) Capabilities(ctx context.Context) (uint32, error) {
	value, err := m.queryInt(ctx, infoCapabilities, "read server capabilities")
	return uint32(value), err
}

// ConnectionCount returns the number of server attachments.
func (m *Manager) ConnectionCount(ctx context.Context) (int, error) {
	info, err := m.serverDatabaseInfo(ctx)
	if err != nil {
		return 0, err
	}
	return info.Attachments, nil
}

// AttachedDatabaseNames returns the databases currently attached to the server.
func (m *Manager) AttachedDatabaseNames(ctx context.Context) ([]string, error) {
	info, err := m.serverDatabaseInfo(ctx)
	if err != nil {
		return nil, err
	}
	return append([]string(nil), info.Databases...), nil
}

// Users lists security-database users. Passwords are never returned.
func (m *Manager) Users(ctx context.Context, name string) ([]User, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	request := ListUsersRequest{Name: name}
	requestBytes, err := request.build()
	if err != nil {
		return nil, err
	}
	if err := m.lockContext(ctx); err != nil {
		return nil, newServiceError("list users", err, m.secrets...)
	}
	job, err := m.startLocked(ctx, requestBytes, request, "list users")
	if err != nil {
		m.mu.Unlock()
		return nil, newServiceError("list users", err, m.secrets...)
	}
	var raw []byte
	resultErr := m.waitForActionLocked(ctx)
	if resultErr == nil {
		raw, resultErr = m.queryInfoLocked(ctx, infoUsers)
	}
	var users []User
	if resultErr == nil {
		users, resultErr = decodeUsers(raw)
	}
	if resultErr == nil {
		resultErr = ctx.Err()
	}
	m.mu.Unlock()
	if resultErr != nil {
		resultErr = newServiceError("read users", resultErr, m.secrets...)
		job.finish(resultErr, false)
		return nil, resultErr
	}
	job.finish(nil, true)
	return users, nil
}

// UserExists reports whether a named user exists.
func (m *Manager) UserExists(ctx context.Context, name string) (bool, error) {
	users, err := m.Users(ctx, name)
	return len(users) != 0, err
}

// AddUser adds one security-database user.
func (m *Manager) AddUser(ctx context.Context, user User) error {
	return m.runAndDrain(ctx, AddUserRequest{User: user})
}

// ModifyUser modifies one security-database user.
func (m *Manager) ModifyUser(ctx context.Context, user User) error {
	return m.runAndDrain(ctx, ModifyUserRequest{User: user})
}

// DeleteUser removes one security-database user.
func (m *Manager) DeleteUser(ctx context.Context, name string) error {
	return m.runAndDrain(ctx, DeleteUserRequest{Name: name})
}

// Aliases lists database aliases.
func (m *Manager) Aliases(ctx context.Context) (map[string]string, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	request := ListAliasesRequest{}
	requestBytes, err := request.build()
	if err != nil {
		return nil, err
	}
	if err := m.lockContext(ctx); err != nil {
		return nil, newServiceError("list database aliases", err, m.secrets...)
	}
	job, err := m.startLocked(ctx, requestBytes, request, "list database aliases")
	if err != nil {
		m.mu.Unlock()
		return nil, newServiceError("list database aliases", err, m.secrets...)
	}
	var raw []byte
	resultErr := m.waitForActionLocked(ctx)
	if resultErr == nil {
		raw, resultErr = m.queryInfoLocked(ctx, infoAliases)
	}
	var aliases map[string]string
	if resultErr == nil {
		aliases, resultErr = decodeAliases(raw)
	}
	if resultErr == nil {
		resultErr = ctx.Err()
	}
	m.mu.Unlock()
	if resultErr != nil {
		resultErr = newServiceError("read database aliases", resultErr, m.secrets...)
		job.finish(resultErr, false)
		return nil, resultErr
	}
	job.finish(nil, true)
	return aliases, nil
}

// AddAlias adds a server database alias.
func (m *Manager) AddAlias(ctx context.Context, alias, database string) error {
	return m.runAndDrain(ctx, AddAliasRequest{Alias: alias, Database: database})
}

// DeleteAlias deletes a server database alias.
func (m *Manager) DeleteAlias(ctx context.Context, alias string) error {
	return m.runAndDrain(ctx, DeleteAliasRequest{Alias: alias})
}

// ListAliases is an alias for Aliases.
func (m *Manager) ListAliases(ctx context.Context) (map[string]string, error) { return m.Aliases(ctx) }

func (m *Manager) queryInt(ctx context.Context, item byte, operation string) (int, error) {
	if err := checkContext(ctx); err != nil {
		return 0, err
	}
	if err := m.lockContext(ctx); err != nil {
		return 0, err
	}
	defer m.mu.Unlock()
	if err := m.ensureIdleLocked(); err != nil {
		return 0, err
	}
	value, err := m.queryIntLocked(ctx, item)
	if err != nil {
		return 0, newServiceError(operation, err, m.secrets...)
	}
	return value, nil
}

func (m *Manager) queryString(ctx context.Context, item byte, operation string) (string, error) {
	if err := checkContext(ctx); err != nil {
		return "", err
	}
	if err := m.lockContext(ctx); err != nil {
		return "", err
	}
	defer m.mu.Unlock()
	if err := m.ensureIdleLocked(); err != nil {
		return "", err
	}
	value, err := m.queryStringLocked(ctx, item)
	if err != nil {
		return "", newServiceError(operation, err, m.secrets...)
	}
	return value, nil
}

func (m *Manager) serverDatabaseInfo(ctx context.Context) (ServerDatabaseInfo, error) {
	if err := checkContext(ctx); err != nil {
		return ServerDatabaseInfo{}, err
	}
	if err := m.lockContext(ctx); err != nil {
		return ServerDatabaseInfo{}, err
	}
	defer m.mu.Unlock()
	if err := m.ensureIdleLocked(); err != nil {
		return ServerDatabaseInfo{}, err
	}
	result, err := m.queryServerDatabaseLocked(ctx)
	if err != nil {
		return ServerDatabaseInfo{}, newServiceError("read server database information", err, m.secrets...)
	}
	return result, nil
}

func (m *Manager) ensureIdleLocked() error {
	if m.closed || m.closing || m.backend == nil {
		return ErrClosed
	}
	if m.active != nil {
		return m.activeErrorLocked()
	}
	return nil
}

func (m *Manager) queryInfoLocked(ctx context.Context, item byte) ([]byte, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	capacity := defaultServiceQueryBuffer
	for {
		releaseNative, err := nativegate.Global.EnterContext(ctx)
		if err != nil {
			return nil, err
		}
		raw, truncated, err := m.backend.query(item, capacity)
		releaseNative()
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !truncated && (len(raw) == 0 || raw[0] != infoTruncated) {
			return raw, nil
		}
		if capacity >= maxServiceQueryBuffer {
			return nil, ErrOutputLimit
		}
		capacity *= 4
		if capacity > maxServiceQueryBuffer {
			capacity = maxServiceQueryBuffer
		}
	}
}

// waitForActionLocked waits for a display action without consuming its output.
// The native client can return a partial information payload when the action
// has not finished yet, so information queries must be ordered after this
// status-only poll.
func (m *Manager) waitForActionLocked(ctx context.Context) error {
	for {
		raw, err := m.queryInfoLocked(ctx, infoRunning)
		if err != nil {
			return err
		}
		running, err := decodeRunning(raw)
		if err != nil {
			return err
		}
		if !running {
			return nil
		}

		timer := time.NewTimer(servicePollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// lockContext admits an operation without making a canceled caller wait for
// an unrelated native action to release the manager mutex.
func (m *Manager) lockContext(ctx context.Context) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	for {
		if m.mu.TryLock() {
			return nil
		}
		timer := time.NewTimer(servicePollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) queryIntLocked(ctx context.Context, item byte) (int, error) {
	raw, err := m.queryInfoLocked(ctx, item)
	if err != nil {
		return 0, err
	}
	value, err := decodeInfoUint16(raw, item)
	return int(value), err
}

func (m *Manager) queryStringLocked(ctx context.Context, item byte) (string, error) {
	raw, err := m.queryInfoLocked(ctx, item)
	if err != nil {
		return "", err
	}
	return decodeInfoString(raw, item)
}

func (m *Manager) queryServerDatabaseLocked(ctx context.Context) (ServerDatabaseInfo, error) {
	raw, err := m.queryInfoLocked(ctx, infoServerDatabase)
	if err != nil {
		return ServerDatabaseInfo{}, err
	}
	return decodeServerDatabaseInfo(raw)
}

func (m *Manager) runAndDrain(ctx context.Context, request Request) error {
	job, err := m.Start(ctx, request)
	if err != nil {
		return err
	}
	return job.Wait(ctx)
}

// Job streams output from one native service action. Job implements io.Reader
// and io.Closer; output is delivered in bounded native chunks and is never
// accumulated by Manager.
type Job struct {
	manager     *Manager
	request     Request
	chunks      chan []byte
	discard     chan struct{}
	discardOnce sync.Once
	done        chan struct{}
	readMu      sync.Mutex
	stateMu     sync.Mutex
	current     []byte
	err         error
	completed   bool
	uncertain   bool
	closed      bool
}

func newJob(manager *Manager, request Request) *Job {
	return &Job{
		manager: manager,
		request: request,
		chunks:  make(chan []byte, 4),
		discard: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (j *Job) run() {
	var operationErr error
	completed := false
	for {
		raw, truncated, err := j.manager.queryActive(infoToEOF, maxServiceQueryBuffer)
		if err != nil {
			operationErr = newServiceError("read service output", err, j.manager.secrets...)
			break
		}
		payload, ended, err := decodeOutput(raw, truncated)
		if err != nil {
			operationErr = err
			break
		}
		if len(payload) != 0 {
			j.enqueue(payload)
		}
		runningRaw, _, err := j.manager.queryActive(infoRunning, 16)
		if err != nil {
			operationErr = newServiceError("read service status", err, j.manager.secrets...)
			break
		}
		running, err := decodeRunning(runningRaw)
		if err != nil {
			operationErr = err
			break
		}
		if ended && !running {
			completed = true
			break
		}
		time.Sleep(servicePollInterval)
	}
	j.finish(operationErr, completed)
}

func (m *Manager) queryActive(item byte, capacity int) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.backend == nil {
		return nil, false, ErrClosed
	}
	return m.backend.query(item, capacity)
}

func (j *Job) enqueue(payload []byte) {
	copyPayload := append([]byte(nil), payload...)
	select {
	case j.chunks <- copyPayload:
	case <-j.discard:
	}
}

func (j *Job) finish(err error, completed bool) {
	j.stateMu.Lock()
	j.err = err
	j.completed = completed
	j.uncertain = !completed
	j.stateMu.Unlock()
	if completed {
		j.manager.finishJob(j)
	}
	close(j.chunks)
	close(j.done)
}

func (m *Manager) finishJob(job *Job) {
	m.mu.Lock()
	if m.active == job {
		m.active = nil
	}
	m.mu.Unlock()
}

// Read reads the next bounded service-output chunk. It returns the native
// operation error after all buffered output has been consumed.
func (j *Job) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	j.readMu.Lock()
	defer j.readMu.Unlock()
	j.stateMu.Lock()
	closed := j.closed
	j.stateMu.Unlock()
	if closed {
		return 0, ErrClosed
	}
	for len(j.current) == 0 {
		chunk, ok := <-j.chunks
		if !ok {
			return 0, j.resultError()
		}
		j.current = chunk
	}
	n := copy(p, j.current)
	j.current = j.current[n:]
	return n, nil
}

// Wait drains any unread output and waits for the native operation to finish.
// If ctx expires, Wait returns its error without canceling or claiming to have
// canceled the native operation; the Job remains active until completion.
func (j *Job) Wait(ctx context.Context) error {
	if ctx == nil {
		return errors.New("interbase services: nil context")
	}
	j.readMu.Lock()
	defer j.readMu.Unlock()
	j.current = nil
	for {
		select {
		case _, ok := <-j.chunks:
			if !ok {
				return j.terminalError()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Close discards unread output and drains the native operation. It does not
// send a native cancellation request. The manager is released for another
// action only when completion was proven; an uncertain operation leaves the
// manager quarantined until Manager.Close detaches it.
func (j *Job) Close() error {
	if j == nil {
		return nil
	}
	j.discardOnce.Do(func() { close(j.discard) })
	err := j.Wait(context.Background())
	j.stateMu.Lock()
	j.closed = true
	j.stateMu.Unlock()
	return err
}

func (j *Job) completionUnknown() bool {
	j.stateMu.Lock()
	defer j.stateMu.Unlock()
	return j.uncertain
}

// CompletionKnown reports whether the native operation reached a terminal
// response that the client could prove. It is false while running and after a
// query or decode error, because detaching is the only safe recovery from that
// uncertain state.
func (j *Job) CompletionKnown() bool {
	if j == nil {
		return false
	}
	j.stateMu.Lock()
	defer j.stateMu.Unlock()
	return j.completed
}

// Done returns a channel closed when the native operation has completed.
func (j *Job) Done() <-chan struct{} {
	if j == nil {
		return closedChannel()
	}
	return j.done
}

// Err returns the terminal native operation error, or nil while running.
func (j *Job) Err() error {
	if j == nil {
		return ErrClosed
	}
	select {
	case <-j.done:
		return j.terminalError()
	default:
		return nil
	}
}

func (j *Job) resultError() error {
	err := j.terminalError()
	if err == nil {
		return io.EOF
	}
	return err
}

func (j *Job) terminalError() error {
	j.stateMu.Lock()
	defer j.stateMu.Unlock()
	return j.err
}

func closedChannel() <-chan struct{} { channel := make(chan struct{}); close(channel); return channel }

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("interbase services: nil context")
	}
	return ctx.Err()
}

func decodeRunning(raw []byte) (bool, error) {
	if len(raw) == 1 && raw[0] == infoDataNotReady {
		return true, nil
	}
	if len(raw) != 6 || raw[0] != infoRunning || raw[5] != infoEnd {
		return false, fmt.Errorf("%w: running response", ErrMalformedResponse)
	}
	value := binaryLittleEndianUint32(raw[1:5])
	if value > 1 {
		return false, fmt.Errorf("%w: running value %d", ErrMalformedResponse, value)
	}
	return value != 0, nil
}

func decodeUsers(raw []byte) ([]User, error) {
	payload, err := unwrapServicePayload(raw, infoUsers)
	if err != nil {
		return nil, err
	}
	var users []User
	var current *User
	for position := 0; position < len(payload); {
		if payload[position] == infoFlagEnd || payload[position] == infoEnd {
			break
		}
		code := payload[position]
		position++
		switch code {
		case securityUserName:
			value, next, err := readInfoString(payload, position)
			if err != nil {
				return nil, err
			}
			if current != nil {
				users = append(users, *current)
			}
			current = &User{Name: value}
			position = next
		case securityPassword:
			_, position, err = readInfoString(payload, position)
			if err != nil {
				return nil, err
			}
		case securityFirstName, securityMiddleName, securityLastName:
			if current == nil {
				return nil, fmt.Errorf("%w: user field before username", ErrMalformedResponse)
			}
			value, next, err := readInfoString(payload, position)
			if err != nil {
				return nil, err
			}
			switch code {
			case securityFirstName:
				current.FirstName = value
			case securityMiddleName:
				current.MiddleName = value
			case securityLastName:
				current.LastName = value
			}
			position = next
		case securityUserID, securityGroupID:
			if current == nil || position+4 > len(payload) {
				return nil, fmt.Errorf("%w: user numeric field", ErrMalformedResponse)
			}
			value := binaryLittleEndianUint32(payload[position : position+4])
			if code == securityUserID {
				current.UserID = value
			} else {
				current.GroupID = value
			}
			position += 4
		default:
			return nil, fmt.Errorf("%w: unknown user item %d", ErrMalformedResponse, code)
		}
	}
	if current != nil {
		users = append(users, *current)
	}
	return users, nil
}

func decodeAliases(raw []byte) (map[string]string, error) {
	payload, err := unwrapServicePayload(raw, infoAliases)
	if err != nil {
		return nil, err
	}
	aliases := make(map[string]string)
	var alias, database string
	for position := 0; position < len(payload); {
		if payload[position] == infoFlagEnd || payload[position] == infoEnd {
			break
		}
		code := payload[position]
		position++
		value, next, err := readInfoString(payload, position)
		if err != nil {
			return nil, err
		}
		position = next
		switch code {
		case aliasName:
			alias = value
		case aliasPath:
			database = value
		default:
			return nil, fmt.Errorf("%w: unknown alias item %d", ErrMalformedResponse, code)
		}
		if alias != "" && database != "" {
			aliases[alias] = database
			alias, database = "", ""
		}
	}
	return aliases, nil
}

func unwrapServicePayload(raw []byte, expected byte) ([]byte, error) {
	if len(raw) == 0 || raw[0] != expected {
		return nil, fmt.Errorf("%w: service item %d", ErrMalformedResponse, expected)
	}
	if len(raw) >= 4 {
		payloadLength := int(binaryLittleEndianUint16(raw[1:3]))
		payloadEnd := 3 + payloadLength
		if payloadEnd == len(raw)-1 && (raw[payloadEnd] == infoEnd || raw[payloadEnd] == infoFlagEnd) {
			return raw[3:payloadEnd], nil
		}
	}
	return raw[1:], nil
}

func readInfoString(raw []byte, position int) (string, int, error) {
	if position+2 > len(raw) {
		return "", position, fmt.Errorf("%w: string length", ErrMalformedResponse)
	}
	length := int(binaryLittleEndianUint16(raw[position : position+2]))
	position += 2
	if length > len(raw)-position {
		return "", position, fmt.Errorf("%w: string payload", ErrMalformedResponse)
	}
	return string(raw[position : position+length]), position + length, nil
}

func binaryLittleEndianUint16(value []byte) uint16 {
	return uint16(value[0]) | uint16(value[1])<<8
}

func binaryLittleEndianUint32(value []byte) uint32 {
	return uint32(value[0]) | uint32(value[1])<<8 | uint32(value[2])<<16 | uint32(value[3])<<24
}

type serviceRedactedError struct {
	message string
	cause   error
}

func (e *serviceRedactedError) Error() string { return e.message }
func (e *serviceRedactedError) Unwrap() error { return e.cause }

func newServiceError(operation string, err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrClosed) || errors.Is(err, ErrJobActive) || errors.Is(err, ErrCompletionUnknown) || errors.Is(err, ErrOutputLimit) || errors.Is(err, ErrMalformedResponse) || errors.Is(err, ErrUnsupported) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	message := redactServiceSecrets(err.Error(), secrets...)
	parsed := parseServiceNativeError(message)
	if native, ok := parsed.(*interbase.Error); ok {
		if operation != "" {
			native.Operation = operation
		}
		return &serviceRedactedError{message: native.Error(), cause: native}
	}
	if operation != "" {
		message = fmt.Sprintf("interbase: %s: %s", operation, message)
	}
	return &serviceRedactedError{message: message, cause: errors.New(message)}
}

func parseServiceNativeError(message string) error {
	const marker = " failed (SQLCODE "
	start := strings.Index(message, marker)
	if start < 0 {
		return &interbase.Error{Message: message}
	}
	remainder := message[start+len(marker):]
	statusMarker := ", native status "
	statusStart := strings.Index(remainder, statusMarker)
	if statusStart < 0 {
		return &interbase.Error{Message: message}
	}
	close := strings.IndexByte(remainder[statusStart+len(statusMarker):], ')')
	if close < 0 {
		return &interbase.Error{Message: message}
	}
	close += statusStart + len(statusMarker)
	sqlCode, sqlErr := strconv.Atoi(strings.TrimSpace(remainder[:statusStart]))
	nativeCode, nativeErr := strconv.ParseInt(strings.TrimSpace(remainder[statusStart+len(statusMarker):close]), 10, 64)
	if sqlErr != nil || nativeErr != nil {
		return &interbase.Error{Message: message}
	}
	detail := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(remainder[close+1:]), ":"))
	return &interbase.Error{Operation: message[:start], SQLCode: sqlCode, NativeCode: nativeCode, Message: detail}
}

func redactServiceSecrets(message string, secrets ...string) string {
	if message == "" {
		return message
	}
	type span struct{ start, end int }
	var spans []span
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		for search := 0; search < len(message); {
			relative := strings.Index(message[search:], secret)
			if relative < 0 {
				break
			}
			start := search + relative
			spans = append(spans, span{start: start, end: start + len(secret)})
			search = start + 1
		}
	}
	if len(spans) == 0 {
		return message
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			if spans[j].start < spans[i].start || (spans[j].start == spans[i].start && spans[j].end > spans[i].end) {
				spans[i], spans[j] = spans[j], spans[i]
			}
		}
	}
	merged := spans[:1]
	for _, current := range spans[1:] {
		last := &merged[len(merged)-1]
		if current.start <= last.end {
			if current.end > last.end {
				last.end = current.end
			}
			continue
		}
		merged = append(merged, current)
	}
	var builder strings.Builder
	position := 0
	for _, current := range merged {
		builder.WriteString(message[position:current.start])
		builder.WriteString("[REDACTED]")
		position = current.end
	}
	builder.WriteString(message[position:])
	return builder.String()
}
