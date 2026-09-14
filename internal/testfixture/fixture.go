// Package testfixture provisions disposable InterBase databases for tests.
package testfixture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	defaultISQL     = "/opt/interbase/bin/isql"
	defaultUser     = "SYSDBA"
	defaultPassword = "masterkey"
	createMarker    = "INTERBASE_GO_FIXTURE_CREATE_COMPLETE"
	dropMarker      = "INTERBASE_GO_FIXTURE_DROP_COMPLETE"

	commandTimeout = 30 * time.Second
	cleanupTimeout = 30 * time.Second
	waitDelay      = 500 * time.Millisecond
	maxOutputBytes = 64 * 1024
)

var sqlFailurePattern = regexp.MustCompile(`(?i)(statement\s+failed|dynamic\s+sql\s+error|sqlcode\s*=\s*-\d+|sqlstate\s*=\s*[0-9a-z]{5})`)

var errUnsafeTempRoot = errors.New("interbase fixture: temporary root is not a local absolute path")
var errOutputLimit = errors.New("isql output exceeded capture limit")
var errOutputIncomplete = errors.New("isql output did not reach EOF before wait delay")
var errOwnedDescendants = errors.New("isql owned descendants remained alive after command exit")

// Config contains the test-only isql and credential settings.
//
// Database paths are deliberately not part of Config. Create always generates
// an owned temporary path instead of accepting a caller-supplied database.
type Config struct {
	ISQL     string
	User     string
	Password string
}

// FromEnv reads the optional test-only fixture overrides.
//
// INTERBASE_DATABASE and the ordinary application credential variables are
// intentionally ignored. An explicitly set empty password is preserved.
func FromEnv() (Config, error) {
	return configFromLookup(os.LookupEnv)
}

func configFromLookup(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("interbase fixture: nil environment lookup")
	}

	cfg := Config{
		ISQL:     defaultISQL,
		User:     defaultUser,
		Password: defaultPassword,
	}
	if value, ok := lookup("INTERBASE_TEST_ISQL"); ok {
		cfg.ISQL = value
	}
	if value, ok := lookup("INTERBASE_TEST_USER"); ok {
		cfg.User = value
	}
	if value, ok := lookup("INTERBASE_TEST_PASSWORD"); ok {
		cfg.Password = value
	}
	if err := validateConfig(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func validateConfig(cfg Config) error {
	if cfg.ISQL == "" {
		return errors.New("interbase fixture: isql path is required")
	}
	if cfg.User == "" {
		return errors.New("interbase fixture: user is required")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "isql path", value: cfg.ISQL},
		{name: "user", value: cfg.User},
		{name: "password", value: cfg.Password},
	} {
		if hasControlCharacter(field.value) {
			return fmt.Errorf("interbase fixture: %s contains a control character", field.name)
		}
	}
	return nil
}

func hasControlCharacter(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func validateLocalPath(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, ':') || hasControlCharacter(path) {
		return errUnsafeTempRoot
	}
	return nil
}

// Database is an owned disposable InterBase database.
//
// Path is the generated database path used by the connector. It must not be
// changed before Close; the ownership check refuses to drop another path.
type Database struct {
	Path string

	mu             sync.Mutex
	config         Config
	directory      string
	ownedPath      string
	databaseExists bool
	dropCompleted  bool
	closed         bool
}

// Create creates a unique local database and loads schema into it.
//
// If provisioning starts and fails, the returned Database is still non-nil so
// callers can register cleanup before handling the error.
func Create(ctx context.Context, cfg Config, schema string) (*Database, error) {
	if ctx == nil {
		return nil, errors.New("interbase fixture: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	root := filepath.Clean(os.TempDir())
	if err := validateLocalPath(root); err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(root, "interbase-go-fixture-")
	if err != nil {
		return nil, fmt.Errorf("interbase fixture: create temporary directory: %w", err)
	}
	path := filepath.Join(directory, "database.ib")
	if err := validateLocalPath(directory); err != nil {
		_ = os.Remove(directory)
		return nil, err
	}
	if err := validateLocalPath(path); err != nil {
		_ = os.Remove(directory)
		return nil, err
	}
	db := &Database{
		Path:      path,
		config:    cfg,
		directory: directory,
		ownedPath: path,
	}

	commandContext, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if err := runISQL(commandContext, cfg, createScript(path, cfg, schema), "create database", createMarker); err != nil {
		cleanupErr := db.handleFailedCreate()
		if cleanupErr != nil {
			return db, errors.Join(err, cleanupErr)
		}
		return db, err
	}
	exists, err := databasePathExists(db.ownedPath)
	if err != nil {
		proofErr := fmt.Errorf("interbase fixture: create database: inspect completion path %q: %w", db.ownedPath, err)
		cleanupErr := db.handleFailedCreate()
		if cleanupErr != nil {
			return db, errors.Join(proofErr, cleanupErr)
		}
		return db, proofErr
	}
	if !exists {
		proofErr := fmt.Errorf("interbase fixture: create database: completion marker reported but database path %q is absent", db.ownedPath)
		cleanupErr := db.handleFailedCreate()
		if cleanupErr != nil {
			return db, errors.Join(proofErr, cleanupErr)
		}
		return db, proofErr
	}
	db.databaseExists = true
	return db, nil
}

func databasePathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (db *Database) handleFailedCreate() error {
	if _, err := os.Stat(db.ownedPath); err == nil {
		db.databaseExists = true
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		db.databaseExists = true
		return fmt.Errorf("interbase fixture: inspect failed database path %q: %w", db.ownedPath, err)
	}

	if err := os.Remove(db.directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			db.closed = true
			return nil
		}
		return fmt.Errorf("interbase fixture: no database was created; retained directory %q: %w", db.directory, err)
	}
	db.closed = true
	return nil
}

// Close drops the owned database and removes its now-empty temporary
// directory. Failed cleanup leaves any remaining artifact in place and can be retried.
func (db *Database) Close() error {
	if db == nil {
		return errors.New("interbase fixture: nil database")
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return nil
	}
	if db.Path != db.ownedPath {
		return errors.New("interbase fixture: database path was changed; refusing cleanup")
	}
	if db.ownedPath == "" || db.directory == "" {
		return errors.New("interbase fixture: database has no owned path")
	}

	var dropErr error
	if db.databaseExists && !db.dropCompleted {
		exists, err := databasePathExists(db.ownedPath)
		if err != nil {
			return fmt.Errorf("interbase fixture: cleanup failed; retained at %q: inspect database path: %w", db.ownedPath, err)
		}
		if !exists {
			db.dropCompleted = true
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			dropErr = runISQL(ctx, db.config, dropScript(db.ownedPath, db.config), "drop database", dropMarker)
			cancel()

			exists, inspectErr := databasePathExists(db.ownedPath)
			if inspectErr != nil {
				if dropErr != nil {
					return errors.Join(
						fmt.Errorf("interbase fixture: cleanup failed; retained at %q: %w", db.ownedPath, dropErr),
						fmt.Errorf("interbase fixture: cleanup failed; retained at %q: inspect database path: %w", db.ownedPath, inspectErr),
					)
				}
				return fmt.Errorf("interbase fixture: cleanup failed; retained at %q: inspect database path: %w", db.ownedPath, inspectErr)
			}
			if exists {
				if dropErr != nil {
					return fmt.Errorf("interbase fixture: cleanup failed; retained at %q: %w", db.ownedPath, dropErr)
				}
				return fmt.Errorf("interbase fixture: cleanup failed; retained at %q: drop completion marker reported but database path remains", db.ownedPath)
			}
			db.dropCompleted = true
		}
	}

	if err := os.Remove(db.directory); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("interbase fixture: database dropped but retained directory %q: %w", db.directory, err)
		}
	}
	db.closed = true
	if dropErr != nil {
		return fmt.Errorf("interbase fixture: cleanup command reported failure after database removal: %w", dropErr)
	}
	return nil
}

func runISQL(ctx context.Context, cfg Config, script, operation, marker string) error {
	commandContext, cancel := context.WithCancel(ctx)
	defer cancel()

	command := exec.CommandContext(commandContext, cfg.ISQL, "-q", "-s", "1", "-names", "UTF8")
	command.Env = fixtureEnvironment()
	command.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	command.Cancel = func() error {
		return terminateProcessGroup(command)
	}
	command.WaitDelay = waitDelay
	output := &boundedOutput{
		limit:  maxOutputBytes,
		cancel: cancel,
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("interbase fixture: %s: create output pipe: %w", operation, err)
	}
	outputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(output, reader)
		outputDone <- copyErr
	}()
	command.Stdout = writer
	command.Stderr = writer
	command.Stdin = strings.NewReader(script)
	// Linux ties Pdeathsig to the OS thread that creates the child, rather than
	// to the lifetime of the Go process. Keep that thread alive through Start,
	// Wait, and the output/process-group cleanup below.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	startErr := command.Start()
	_ = writer.Close()
	if startErr != nil {
		_ = reader.Close()
		<-outputDone
		return formatCommandError(operation, startErr, output.String(), cfg)
	}
	commandErr := command.Wait()
	outputErr := waitForISQLOutput(command, reader, outputDone)

	if output.Exceeded() {
		_ = terminateProcessGroup(command)
		return fmt.Errorf("interbase fixture: %s: %w", operation, errOutputLimit)
	}
	if outputErr != nil {
		_ = terminateProcessGroup(command)
		if contextErr := ctx.Err(); contextErr != nil {
			return fmt.Errorf("interbase fixture: %s: %w", operation, errors.Join(outputErr, contextErr))
		}
		return fmt.Errorf("interbase fixture: %s: %w", operation, outputErr)
	}
	text := redact(output.String(), cfg.User, cfg.Password)

	if sqlFailurePattern.MatchString(output.String()) {
		_ = terminateProcessGroup(command)
		if text == "" {
			text = "isql reported an SQL failure"
		}
		return fmt.Errorf("interbase fixture: %s: %s", operation, text)
	}
	if contextErr := ctx.Err(); contextErr != nil {
		_ = terminateProcessGroup(command)
		return formatCommandError(operation, contextErr, text, cfg)
	}
	if commandErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(commandErr, &exitErr) || exitErr.ExitCode() != 1 {
			_ = terminateProcessGroup(command)
			return formatCommandError(operation, commandErr, text, cfg)
		}
	}
	if !hasCompletionMarker(output.String(), marker) {
		_ = terminateProcessGroup(command)
		detail := fmt.Sprintf("completion marker %q was not found", marker)
		if commandErr != nil {
			detail += ": " + redact(commandErr.Error(), cfg.User, cfg.Password)
		}
		if text != "" {
			detail += ": " + text
		}
		return fmt.Errorf("interbase fixture: %s: %s", operation, detail)
	}
	return nil
}

func waitForISQLOutput(command *exec.Cmd, reader *os.File, outputDone <-chan error) error {
	timer := time.NewTimer(waitDelay)
	defer timer.Stop()

	select {
	case copyErr := <-outputDone:
		_ = reader.Close()
		if copyErr != nil {
			killErr := terminateAndWaitForProcessGroup(command)
			if errors.Is(copyErr, errOutputLimit) {
				if killErr != nil {
					return errors.Join(copyErr, killErr)
				}
				return copyErr
			}
			captureErr := fmt.Errorf("%w: %v", errOutputIncomplete, copyErr)
			if killErr != nil {
				return errors.Join(captureErr, killErr)
			}
			return captureErr
		}
		if processGroupAlive(command) {
			killErr := terminateAndWaitForProcessGroup(command)
			if killErr != nil {
				return errors.Join(errOwnedDescendants, killErr)
			}
			return errOwnedDescendants
		}
		return nil
	case <-timer.C:
		killErr := terminateAndWaitForProcessGroup(command)
		_ = reader.Close()
		copyErr := waitForOutputCopy(outputDone)
		if errors.Is(copyErr, errOutputLimit) {
			if killErr != nil {
				return errors.Join(copyErr, killErr)
			}
			return copyErr
		}
		if killErr != nil {
			return errors.Join(errOutputIncomplete, killErr)
		}
		return errOutputIncomplete
	}
}

func waitForOutputCopy(outputDone <-chan error) error {
	timer := time.NewTimer(waitDelay)
	defer timer.Stop()
	select {
	case copyErr := <-outputDone:
		return copyErr
	case <-timer.C:
		return nil
	}
}

func terminateAndWaitForProcessGroup(command *exec.Cmd) error {
	if err := terminateProcessGroup(command); err != nil {
		return err
	}
	if !processGroupAlive(command) {
		return nil
	}

	timer := time.NewTimer(waitDelay)
	defer timer.Stop()
	for processGroupAlive(command) {
		select {
		case <-timer.C:
			return errOwnedDescendants
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}

func processGroupAlive(command *exec.Cmd) bool {
	if command.Process == nil {
		return false
	}
	err := syscall.Kill(-command.Process.Pid, 0)
	return err == nil || err == syscall.EPERM
}

func hasCompletionMarker(output, marker string) bool {
	if marker == "" {
		return false
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == marker {
			return true
		}
	}
	return false
}

type boundedOutput struct {
	mu       sync.Mutex
	buffer   bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	if output.exceeded {
		output.mu.Unlock()
		return 0, errOutputLimit
	}
	remaining := output.limit - output.buffer.Len()
	if len(data) <= remaining {
		n, err := output.buffer.Write(data)
		output.mu.Unlock()
		return n, err
	}
	if remaining > 0 {
		_, _ = output.buffer.Write(data[:remaining])
	}
	output.exceeded = true
	cancel := output.cancel
	output.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return remaining, errOutputLimit
}

func (output *boundedOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	text := output.buffer.String()
	if output.exceeded {
		text += "\n[output truncated]"
	}
	return text
}

func (output *boundedOutput) Exceeded() bool {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.exceeded
}

func terminateProcessGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}
	return err
}

func formatCommandError(operation string, err error, output string, cfg Config) error {
	detail := output
	commandMessage := redact(err.Error(), cfg.User, cfg.Password)
	if detail == "" {
		detail = commandMessage
	} else if commandMessage != "" {
		detail = commandMessage + ": " + detail
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("interbase fixture: %s: %w: %s", operation, err, detail)
	}
	return fmt.Errorf("interbase fixture: %s: %s", operation, detail)
}

func fixtureEnvironment() []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment))
	for _, variable := range environment {
		name, _, _ := strings.Cut(variable, "=")
		switch name {
		case "INTERBASE_DATABASE", "INTERBASE_USER", "INTERBASE_PASSWORD":
			continue
		default:
			filtered = append(filtered, variable)
		}
	}
	return filtered
}

func redact(output string, secrets ...string) string {
	candidates := make([]string, 0, len(secrets)*3)
	seen := make(map[string]struct{}, len(secrets)*3)
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		escaped := strings.ReplaceAll(secret, "'", "''")
		for _, candidate := range []string{
			"'" + escaped + "'",
			escaped,
			secret,
		} {
			if _, ok := seen[candidate]; ok {
				continue
			}
			seen[candidate] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if len(candidates[i]) != len(candidates[j]) {
			return len(candidates[i]) > len(candidates[j])
		}
		return candidates[i] < candidates[j]
	})

	var redacted strings.Builder
	redacted.Grow(len(output))
	for offset := 0; offset < len(output); {
		match := ""
		for _, candidate := range candidates {
			if strings.HasPrefix(output[offset:], candidate) {
				match = candidate
				break
			}
		}
		if match == "" {
			redacted.WriteByte(output[offset])
			offset++
			continue
		}
		redacted.WriteString("[redacted]")
		offset += len(match)
	}
	return redacted.String()
}

func createScript(path string, cfg Config, schema string) string {
	var script strings.Builder
	fmt.Fprintf(&script, "CREATE DATABASE %s USER %s PASSWORD %s DEFAULT CHARACTER SET UTF8;\n",
		quoteSQLString(path), quoteSQLString(cfg.User), quoteSQLString(cfg.Password))
	script.WriteString(schema)
	if schema != "" && !strings.HasSuffix(schema, "\n") {
		script.WriteByte('\n')
	}
	script.WriteString("COMMIT;\n")
	fmt.Fprintf(&script, "SELECT %s FROM RDB$DATABASE;\n", quoteSQLString(createMarker))
	return script.String()
}

func dropScript(path string, cfg Config) string {
	return fmt.Sprintf("CONNECT %s USER %s PASSWORD %s;\nSELECT %s FROM RDB$DATABASE;\nDROP DATABASE;\n",
		quoteSQLString(path), quoteSQLString(cfg.User), quoteSQLString(cfg.Password), quoteSQLString(dropMarker))
}

func quoteSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
