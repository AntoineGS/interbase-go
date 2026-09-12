package testfixture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("INTERBASE_FIXTURE_DESCENDANT") == "1" {
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
	if mode := os.Getenv("INTERBASE_FIXTURE_HELPER"); mode != "" {
		helperProcess(mode)
	}
	os.Exit(m.Run())
}

func helperProcess(mode string) {
	input, _ := io.ReadAll(os.Stdin)
	script := string(input)
	exitCode := 0
	dropFailed := false
	if strings.Contains(script, "CREATE DATABASE") {
		if path := helperDatabasePath(script); path != "" {
			if err := os.WriteFile(path, []byte("owned fixture placeholder"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				exitCode = 1
			}
		}
	}

	switch mode {
	case "fail-create":
		if strings.Contains(script, "CREATE DATABASE") {
			fmt.Fprintf(os.Stderr, "Statement failed, SQLCODE = -902\nuser=%s password=%s escaped=%s\n",
				os.Getenv("INTERBASE_FIXTURE_HELPER_USER"),
				os.Getenv("INTERBASE_FIXTURE_HELPER_RAW"),
				os.Getenv("INTERBASE_FIXTURE_HELPER_ESCAPED"))
		}
	case "sleep-create":
		if strings.Contains(script, "CREATE DATABASE") {
			time.Sleep(10 * time.Second)
		}
	case "excessive-output":
		if strings.Contains(script, "CREATE DATABASE") {
			_, _ = os.Stdout.Write([]byte(strings.Repeat("x", maxOutputBytes+1)))
		}
	case "descendant-holds-pipes":
		if strings.Contains(script, "CREATE DATABASE") {
			child := exec.Command(os.Args[0])
			child.Env = append(os.Environ(), "INTERBASE_FIXTURE_DESCENDANT=1")
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				exitCode = 1
			} else if pidFile := os.Getenv("INTERBASE_FIXTURE_DESCENDANT_PID"); pidFile != "" {
				if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
					fmt.Fprintln(os.Stderr, err)
					exitCode = 1
				}
			}
		}
	case "fail-create-descendant-holds-pipes":
		if strings.Contains(script, "CREATE DATABASE") {
			child := exec.Command(os.Args[0])
			child.Env = append(os.Environ(), "INTERBASE_FIXTURE_DESCENDANT=1")
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if err := child.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				exitCode = 1
			} else if pidFile := os.Getenv("INTERBASE_FIXTURE_DESCENDANT_PID"); pidFile != "" {
				if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
					fmt.Fprintln(os.Stderr, err)
					exitCode = 1
				}
			}
			exitCode = 1
		}
	case "fail-create-process":
		if strings.Contains(script, "CREATE DATABASE") {
			fmt.Fprintf(os.Stderr, "subprocess failed user=%s password=%s escaped=%s\n",
				os.Getenv("INTERBASE_FIXTURE_HELPER_USER"),
				os.Getenv("INTERBASE_FIXTURE_HELPER_RAW"),
				os.Getenv("INTERBASE_FIXTURE_HELPER_ESCAPED"))
			exitCode = 1
		}
	case "boundary-output":
		if strings.Contains(script, "CREATE DATABASE") {
			boundary := os.Getenv("INTERBASE_FIXTURE_BOUNDARY")
			prefixLength := maxOutputBytes - len(boundary)/2
			if prefixLength < 0 {
				prefixLength = 0
			}
			_, _ = os.Stdout.Write([]byte(strings.Repeat("x", prefixLength) + boundary + "\n"))
		}
	case "fail-drop-once":
		if strings.Contains(script, "DROP DATABASE") {
			state := os.Getenv("INTERBASE_FIXTURE_HELPER_STATE")
			file, err := os.OpenFile(state, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err == nil {
				_ = file.Close()
				fmt.Fprintln(os.Stderr, "Statement failed, SQLCODE = -901")
				dropFailed = true
			}
		}
	}
	if strings.Contains(script, "DROP DATABASE") && exitCode == 0 && !dropFailed {
		_ = os.Remove(helperDatabasePath(script))
	}
	os.Exit(exitCode)
}

func helperDatabasePath(script string) string {
	for _, command := range []string{"CREATE DATABASE ", "CONNECT "} {
		start := strings.Index(script, command)
		if start < 0 {
			continue
		}
		rest := script[start+len(command):]
		if len(rest) == 0 || rest[0] != '\'' {
			continue
		}
		var path strings.Builder
		for i := 1; i < len(rest); i++ {
			switch {
			case rest[i] != '\'':
				path.WriteByte(rest[i])
			case i+1 < len(rest) && rest[i+1] == '\'':
				path.WriteByte('\'')
				i++
			default:
				return path.String()
			}
		}
	}
	return ""
}

func TestDefaults(t *testing.T) {
	cfg, err := configFromLookup(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "SYSDBA" || cfg.Password != "masterkey" || cfg.ISQL != "/opt/interbase/bin/isql" {
		t.Fatalf("unexpected test defaults: %+v", cfg)
	}
}

func TestExplicitEmptyPasswordIsPreserved(t *testing.T) {
	cfg, err := configFromLookup(func(name string) (string, bool) {
		if name == "INTERBASE_TEST_PASSWORD" {
			return "", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Password != "" {
		t.Fatalf("password = %q, want explicit empty password", cfg.Password)
	}
}

func TestConfigIgnoresOperationalEnvironment(t *testing.T) {
	operational := map[string]string{
		"INTERBASE_DATABASE": "/srv/interbase/operational.ib",
		"INTERBASE_USER":     "live-user",
		"INTERBASE_PASSWORD": "live-password",
	}
	seen := make(map[string]bool)
	cfg, err := configFromLookup(func(name string) (string, bool) {
		seen[name] = true
		value, ok := operational[name]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "SYSDBA" || cfg.Password != "masterkey" || cfg.ISQL != "/opt/interbase/bin/isql" {
		t.Fatalf("operational settings affected fixture config: %+v", cfg)
	}
	if seen["INTERBASE_DATABASE"] || seen["INTERBASE_USER"] || seen["INTERBASE_PASSWORD"] {
		t.Fatalf("fixture config looked up operational environment: %v", seen)
	}
}

func TestISQLEnvironmentExcludesOperationalSettings(t *testing.T) {
	for _, name := range []string{"INTERBASE_DATABASE", "INTERBASE_USER", "INTERBASE_PASSWORD"} {
		t.Setenv(name, "must-not-reach-isql")
	}

	for _, variable := range fixtureEnvironment() {
		name, _, _ := strings.Cut(variable, "=")
		switch name {
		case "INTERBASE_DATABASE", "INTERBASE_USER", "INTERBASE_PASSWORD":
			t.Fatalf("operational environment variable passed to isql: %s", name)
		}
	}
}

func TestCreateRejectsRelativeTemporaryRoot(t *testing.T) {
	root := "fixture-relative-temp-root"
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(root); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove test temporary root: %v", err)
		}
	})
	t.Setenv("TMPDIR", root)

	db, err := Create(context.Background(), helperConfig(t, "fail-create"), "")
	if db != nil {
		t.Fatal("Create returned an owned fixture for a relative temporary root")
	}
	if !errors.Is(err, errUnsafeTempRoot) {
		t.Fatalf("Create error = %v, want errUnsafeTempRoot", err)
	}
}

func TestCreateRejectsColonTemporaryRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fixture:colon-root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", root)

	db, err := Create(context.Background(), helperConfig(t, "fail-create"), "")
	if db != nil {
		t.Fatal("Create returned an owned fixture for a colon-containing temporary root")
	}
	if !errors.Is(err, errUnsafeTempRoot) {
		t.Fatalf("Create error = %v, want errUnsafeTempRoot", err)
	}
}

func TestCreateRejectsControlCharacterTemporaryRoot(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "fixture\nroot"))

	db, err := Create(context.Background(), helperConfig(t, "fail-create"), "")
	if db != nil {
		t.Fatal("Create returned an owned fixture for a control-character temporary root")
	}
	if !errors.Is(err, errUnsafeTempRoot) {
		t.Fatalf("Create error = %v, want errUnsafeTempRoot", err)
	}
}

func TestConfigRejectsControlCharacters(t *testing.T) {
	for _, test := range []struct {
		name string
		cfg  Config
	}{
		{name: "isql newline", cfg: Config{ISQL: "/tmp/isql\n", User: "SYSDBA", Password: "masterkey"}},
		{name: "user nul", cfg: Config{ISQL: "/tmp/isql", User: "SYS\x00DBA", Password: "masterkey"}},
		{name: "password carriage return", cfg: Config{ISQL: "/tmp/isql", User: "SYSDBA", Password: "bad\rsecret"}},
		{name: "password tab", cfg: Config{ISQL: "/tmp/isql", User: "SYSDBA", Password: "bad\tsecret"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateConfig(test.cfg); err == nil {
				t.Fatal("validateConfig accepted a control character")
			}
		})
	}
}

func TestSQLScriptsEscapeLiterals(t *testing.T) {
	cfg := Config{ISQL: "/tmp/isql", User: "O'Reilly", Password: "p'ass"}
	path := "/tmp/fixture's/database's.ib"
	schema := "CREATE TABLE T (LABEL VARCHAR(20));"

	create := createScript(path, cfg, schema)
	for _, want := range []string{
		"CREATE DATABASE '/tmp/fixture''s/database''s.ib'",
		"USER 'O''Reilly'",
		"PASSWORD 'p''ass'",
		"DEFAULT CHARACTER SET UTF8",
		schema,
	} {
		if !strings.Contains(create, want) {
			t.Fatalf("create script does not contain %q:\n%s", want, create)
		}
	}

	drop := dropScript(path, cfg)
	for _, want := range []string{
		"CONNECT '/tmp/fixture''s/database''s.ib'",
		"USER 'O''Reilly'",
		"PASSWORD 'p''ass'",
		"DROP DATABASE",
	} {
		if !strings.Contains(drop, want) {
			t.Fatalf("drop script does not contain %q:\n%s", want, drop)
		}
	}
}

func TestCreateReportsSQLFailureAndRedactsCredentials(t *testing.T) {
	password := "secret'password"
	cfg := helperConfig(t, "fail-create")
	cfg.User = "fixture-user"
	cfg.Password = password
	t.Setenv("INTERBASE_FIXTURE_HELPER_USER", cfg.User)
	t.Setenv("INTERBASE_FIXTURE_HELPER_RAW", password)
	t.Setenv("INTERBASE_FIXTURE_HELPER_ESCAPED", strings.ReplaceAll(password, "'", "''"))

	db, err := Create(context.Background(), cfg, "")
	if db == nil {
		t.Fatal("Create returned a nil fixture with a partial setup error")
	}
	if err == nil {
		t.Fatal("Create accepted an exit-zero SQL failure")
	}
	if !strings.Contains(err.Error(), "SQLCODE") {
		t.Fatalf("Create error lost SQL diagnostics: %v", err)
	}
	if _, statErr := os.Stat(db.Path); statErr != nil {
		t.Fatalf("partial fixture placeholder is missing: %v", statErr)
	}
	for _, secret := range []string{cfg.User, password, strings.ReplaceAll(password, "'", "''")} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Create error leaked credential %q: %v", secret, err)
		}
	}

	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close partial fixture: %v", closeErr)
	}
}

func TestCreateReportsFailedSubprocessAndRedactsCredentials(t *testing.T) {
	password := "process-secret'password"
	cfg := helperConfig(t, "fail-create-process")
	cfg.User = "process-user"
	cfg.Password = password
	t.Setenv("INTERBASE_FIXTURE_HELPER_USER", cfg.User)
	t.Setenv("INTERBASE_FIXTURE_HELPER_RAW", password)
	t.Setenv("INTERBASE_FIXTURE_HELPER_ESCAPED", strings.ReplaceAll(password, "'", "''"))

	db, err := Create(context.Background(), cfg, "")
	if db == nil {
		t.Fatal("Create returned a nil fixture with a partial subprocess error")
	}
	if err == nil {
		t.Fatal("Create accepted a failed isql subprocess")
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("Create error lost subprocess diagnostics: %v", err)
	}
	for _, secret := range []string{cfg.User, password, strings.ReplaceAll(password, "'", "''")} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Create error leaked credential %q: %v", secret, err)
		}
	}

	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close partial fixture: %v", closeErr)
	}
}

func TestRedactPrefersLongestOverlappingCredential(t *testing.T) {
	got := redact("user=abc password=abc-secret escaped=abc-secret", "abc", "abc-secret")
	want := "user=[redacted] password=[redacted] escaped=[redacted]"
	if got != want {
		t.Fatalf("redact = %q, want %q", got, want)
	}
}

func TestRedactPrefersEscapedCredentialOverOverlappingUsername(t *testing.T) {
	got := redact("user=svc password=svc'pass escaped=svc''pass", "svc", "svc'pass")
	want := "user=[redacted] password=[redacted] escaped=[redacted]"
	if got != want {
		t.Fatalf("redact = %q, want %q", got, want)
	}
}

func TestCreateHonorsContextDeadline(t *testing.T) {
	cfg := helperConfig(t, "sleep-create")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	db, err := Create(ctx, cfg, "")
	if db == nil {
		t.Fatal("Create returned a nil fixture with a partial setup timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Create error = %v, want context.DeadlineExceeded", err)
	}

	t.Setenv("INTERBASE_FIXTURE_HELPER", "success")
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close timed-out fixture: %v", closeErr)
	}
}

func TestCreateRemovesEmptyDirectoryWhenISQLIsMissing(t *testing.T) {
	cfg := Config{
		ISQL:     filepath.Join(t.TempDir(), "missing-isql"),
		User:     "SYSDBA",
		Password: "masterkey",
	}
	db, err := Create(context.Background(), cfg, "")
	if db == nil {
		t.Fatal("Create returned a nil fixture after a missing isql executable")
	}
	if err == nil {
		t.Fatal("Create unexpectedly succeeded with a missing isql executable")
	}
	if _, statErr := os.Stat(filepath.Dir(db.Path)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("empty fixture directory was not removed: %v", statErr)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("Close after empty-directory cleanup: %v", closeErr)
	}
}

func TestCreateRejectsExcessiveISQLOutput(t *testing.T) {
	cfg := helperConfig(t, "excessive-output")
	db, err := Create(context.Background(), cfg, "")
	if db == nil {
		t.Fatal("Create returned a nil fixture with an output-limit error")
	}
	if !errors.Is(err, errOutputLimit) {
		t.Fatalf("Create error = %v, want errOutputLimit", err)
	}

	t.Setenv("INTERBASE_FIXTURE_HELPER", "success")
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close output-limited fixture: %v", closeErr)
	}
}

func TestCreateTerminatesDescendantHoldingOutputPipes(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	t.Setenv("INTERBASE_FIXTURE_DESCENDANT_PID", pidFile)
	cfg := helperConfig(t, "descendant-holds-pipes")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	db, err := Create(ctx, cfg, "")
	elapsed := time.Since(started)
	if db == nil {
		t.Fatal("Create returned a nil fixture with a descendant timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Create error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Create waited %v for a descendant holding output pipes", elapsed)
	}

	contents, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("read descendant pid: %v", readErr)
	}
	pid, atoiErr := strconv.Atoi(string(contents))
	if atoiErr != nil {
		t.Fatalf("parse descendant pid %q: %v", contents, atoiErr)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		killErr := syscall.Kill(pid, 0)
		if errors.Is(killErr, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant process %d is still alive: %v", pid, killErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Setenv("INTERBASE_FIXTURE_HELPER", "success")
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close descendant fixture: %v", closeErr)
	}
}

func TestCreateKillsDescendantWhenWrapperFailsBeforeContextDeadline(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	t.Setenv("INTERBASE_FIXTURE_DESCENDANT_PID", pidFile)
	cfg := helperConfig(t, "fail-create-descendant-holds-pipes")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	started := time.Now()
	db, err := Create(ctx, cfg, "")
	elapsed := time.Since(started)
	if db == nil {
		t.Fatal("Create returned a nil fixture with a failed wrapper")
	}
	if err == nil {
		t.Fatal("Create accepted a failed wrapper")
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("Create waited %v instead of the finite wait delay", elapsed)
	}

	contents, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatalf("read descendant pid: %v", readErr)
	}
	pid, atoiErr := strconv.Atoi(string(contents))
	if atoiErr != nil {
		t.Fatalf("parse descendant pid %q: %v", contents, atoiErr)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for {
		killErr := syscall.Kill(pid, 0)
		if errors.Is(killErr, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant process %d is still alive: %v", pid, killErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Setenv("INTERBASE_FIXTURE_HELPER", "success")
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close failed-wrapper fixture: %v", closeErr)
	}
}

func TestCreateOmitsAllOutputWhenCaptureLimitIsExceeded(t *testing.T) {
	password := "boundary-secret'password"
	escaped := strings.ReplaceAll(password, "'", "''")
	for _, test := range []struct {
		name     string
		boundary string
	}{
		{name: "raw", boundary: password},
		{name: "sql escaped", boundary: escaped},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := helperConfig(t, "boundary-output")
			t.Setenv("INTERBASE_FIXTURE_BOUNDARY", test.boundary)

			db, err := Create(context.Background(), cfg, "")
			if db == nil {
				t.Fatal("Create returned a nil fixture with an output-limit error")
			}
			if !errors.Is(err, errOutputLimit) {
				t.Fatalf("Create error = %v, want errOutputLimit", err)
			}
			if err.Error() != "interbase fixture: create database: isql output exceeded capture limit" {
				t.Fatalf("Create error = %q, want generic output-limit error", err)
			}
			for _, secret := range []string{password, escaped, test.boundary[:len(test.boundary)/2]} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("Create error leaked boundary credential %q", secret)
				}
			}

			t.Setenv("INTERBASE_FIXTURE_HELPER", "success")
			if closeErr := db.Close(); closeErr != nil {
				t.Fatalf("close output-limited fixture: %v", closeErr)
			}
		})
	}
}

func TestCloseRefusesChangedPathAndIsIdempotent(t *testing.T) {
	cfg := helperConfig(t, "success")
	db, err := Create(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	path := db.Path
	directory := filepath.Dir(path)

	db.Path = filepath.Join(directory, "not-owned.ib")
	if err := db.Close(); err == nil {
		t.Fatal("Close cleaned up after public Path was changed")
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("fixture directory after refused cleanup: %v", err)
	}

	db.Path = path
	if err := db.Close(); err != nil {
		t.Fatalf("Close returned error after restoring owned Path: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture directory still exists after cleanup: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close returned error: %v", err)
	}
}

func TestCloseCanRetryAfterSubprocessFailure(t *testing.T) {
	state := filepath.Join(t.TempDir(), "drop-attempt")
	t.Setenv("INTERBASE_FIXTURE_HELPER_STATE", state)
	cfg := helperConfig(t, "fail-drop-once")
	db, err := Create(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	directory := filepath.Dir(db.Path)

	if err := db.Close(); err == nil {
		t.Fatal("Close unexpectedly succeeded on the first drop attempt")
	}
	if _, err := os.Stat(db.Path); err != nil {
		t.Fatalf("partial fixture placeholder was not preserved after failed drop: %v", err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("fixture directory was not preserved after failed cleanup: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close did not retry successfully: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture directory still exists after retry: %v", err)
	}
}

func helperConfig(t *testing.T, mode string) Config {
	t.Helper()
	isql, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("INTERBASE_FIXTURE_HELPER", mode)
	return Config{ISQL: isql, User: "SYSDBA", Password: "masterkey"}
}
