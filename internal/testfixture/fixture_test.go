package testfixture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	helperCreateCompletionMarker = "INTERBASE_GO_FIXTURE_CREATE_COMPLETE"
	helperDropCompletionMarker   = "INTERBASE_GO_FIXTURE_DROP_COMPLETE"
)

func TestMain(m *testing.M) {
	if os.Getenv("INTERBASE_FIXTURE_DESCENDANT") == "1" {
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
	if os.Getenv("INTERBASE_FIXTURE_PARENT_DEATH_HELPER") == "1" {
		parentDeathHelperProcess()
	}
	if mode := os.Getenv("INTERBASE_FIXTURE_HELPER"); mode != "" {
		helperProcess(mode)
	}
	os.Exit(m.Run())
}

func helperProcess(mode string) {
	input, _ := io.ReadAll(os.Stdin)
	script := string(input)
	create := strings.Contains(script, "CREATE DATABASE")
	drop := strings.Contains(script, "DROP DATABASE")
	path := helperDatabasePath(script)
	exitCode := 0
	dropFailed := false

	switch mode {
	case "success":
		if create {
			helperCreateDatabase(path, &exitCode)
			fmt.Fprintln(os.Stdout, helperCreateCompletionMarker)
		}
		if drop {
			fmt.Fprintln(os.Stdout, helperDropCompletionMarker)
			helperDropDatabase(path, &exitCode)
		}
	case "success-exit-one":
		if create {
			helperCreateDatabase(path, &exitCode)
			fmt.Fprintln(os.Stdout, helperCreateCompletionMarker)
		}
		if drop {
			fmt.Fprintln(os.Stdout, helperDropCompletionMarker)
			helperDropDatabase(path, &exitCode)
		}
		exitCode = 1
	case "exit-one-no-marker":
		if create {
			helperCreateDatabase(path, &exitCode)
		}
		if drop {
			helperDropDatabase(path, &exitCode)
		}
		exitCode = 1
	case "marker-and-sql-error":
		if create {
			helperCreateDatabase(path, &exitCode)
			fmt.Fprintln(os.Stdout, "Statement failed, SQLCODE = -901")
			fmt.Fprintln(os.Stdout, helperCreateCompletionMarker)
			exitCode = 1
		}
	case "marker-create-missing-file":
		if create {
			fmt.Fprintln(os.Stdout, helperCreateCompletionMarker)
		}
	case "drop-marker-keeps-file":
		if create {
			helperCreateDatabase(path, &exitCode)
			fmt.Fprintln(os.Stdout, helperCreateCompletionMarker)
		}
		if drop {
			fmt.Fprintln(os.Stdout, helperDropCompletionMarker)
			exitCode = 1
		}
	case "drop-fails-after-removing-file":
		if create {
			helperCreateDatabase(path, &exitCode)
			fmt.Fprintln(os.Stdout, helperCreateCompletionMarker)
		}
		if drop {
			state := os.Getenv("INTERBASE_FIXTURE_HELPER_STATE")
			if _, err := os.Stat(state); err == nil {
				fmt.Fprintln(os.Stderr, "unexpected retry after database was removed")
			} else if errors.Is(err, os.ErrNotExist) {
				if err := os.WriteFile(state, []byte("drop attempted"), 0o600); err != nil {
					fmt.Fprintln(os.Stderr, err)
				} else {
					_ = os.Remove(path)
					fmt.Fprintln(os.Stderr, "drop command failed after database removal")
				}
			} else {
				fmt.Fprintln(os.Stderr, err)
			}
			exitCode = 1
		}
	case "fail-create":
		if create {
			helperCreateDatabase(path, &exitCode)
			fmt.Fprintf(os.Stderr, "Statement failed, SQLCODE = -902\nuser=%s password=%s escaped=%s\n",
				os.Getenv("INTERBASE_FIXTURE_HELPER_USER"),
				os.Getenv("INTERBASE_FIXTURE_HELPER_RAW"),
				os.Getenv("INTERBASE_FIXTURE_HELPER_ESCAPED"))
		}
	case "sleep-create":
		if create {
			helperCreateDatabase(path, &exitCode)
			time.Sleep(10 * time.Second)
		}
	case "parent-death-isql":
		if create {
			helperCreateDatabase(path, &exitCode)
			readyFile := os.Getenv("INTERBASE_FIXTURE_PARENT_DEATH_READY")
			if exitCode == 0 && readyFile != "" {
				ready := fmt.Sprintf("%d\n%s\n", os.Getpid(), path)
				if err := os.WriteFile(readyFile, []byte(ready), 0o600); err != nil {
					fmt.Fprintln(os.Stderr, err)
					exitCode = 1
				}
			}
			if exitCode == 0 {
				fmt.Fprintln(os.Stdout, helperCreateCompletionMarker)
				for {
					time.Sleep(time.Hour)
				}
			}
		}
	case "excessive-output":
		if create {
			helperCreateDatabase(path, &exitCode)
			_, _ = os.Stdout.Write([]byte(strings.Repeat("x", maxOutputBytes+1)))
		}
	case "descendant-holds-pipes":
		if create {
			helperCreateDatabase(path, &exitCode)
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
		if create {
			helperCreateDatabase(path, &exitCode)
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
	case "marked-exit-one-descendant-holds-pipes":
		if create {
			helperCreateDatabase(path, &exitCode)
			fmt.Fprintln(os.Stdout, helperCreateCompletionMarker)
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
		if create {
			helperCreateDatabase(path, &exitCode)
			fmt.Fprintf(os.Stderr, "subprocess failed user=%s password=%s escaped=%s\n",
				os.Getenv("INTERBASE_FIXTURE_HELPER_USER"),
				os.Getenv("INTERBASE_FIXTURE_HELPER_RAW"),
				os.Getenv("INTERBASE_FIXTURE_HELPER_ESCAPED"))
			exitCode = 1
		}
	case "boundary-output":
		if create {
			helperCreateDatabase(path, &exitCode)
			boundary := os.Getenv("INTERBASE_FIXTURE_BOUNDARY")
			prefixLength := maxOutputBytes - len(boundary)/2
			if prefixLength < 0 {
				prefixLength = 0
			}
			_, _ = os.Stdout.Write([]byte(strings.Repeat("x", prefixLength) + boundary + "\n"))
		}
	case "fail-drop-once":
		if create {
			helperCreateDatabase(path, &exitCode)
			fmt.Fprintln(os.Stdout, helperCreateCompletionMarker)
		}
		if drop {
			state := os.Getenv("INTERBASE_FIXTURE_HELPER_STATE")
			file, err := os.OpenFile(state, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err == nil {
				_ = file.Close()
				fmt.Fprintln(os.Stderr, "Statement failed, SQLCODE = -901")
				dropFailed = true
			} else if errors.Is(err, os.ErrExist) {
				fmt.Fprintln(os.Stdout, helperDropCompletionMarker)
				helperDropDatabase(path, &exitCode)
			} else {
				fmt.Fprintln(os.Stderr, err)
				exitCode = 1
			}
		}
	}
	if drop && exitCode == 0 && !dropFailed && mode != "success" && mode != "success-exit-one" && mode != "drop-marker-keeps-file" && mode != "drop-fails-after-removing-file" && mode != "fail-drop-once" {
		fmt.Fprintln(os.Stdout, helperDropCompletionMarker)
		helperDropDatabase(path, &exitCode)
	}
	os.Exit(exitCode)
}

func parentDeathHelperProcess() {
	readyFile := os.Getenv("INTERBASE_FIXTURE_PARENT_DEATH_READY")
	_ = os.Unsetenv("INTERBASE_FIXTURE_PARENT_DEATH_HELPER")
	if err := os.Setenv("INTERBASE_FIXTURE_HELPER", "parent-death-isql"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	isql, err := filepath.Abs(os.Args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	db, err := Create(context.Background(), Config{
		ISQL:     isql,
		User:     "SYSDBA",
		Password: "masterkey",
		Dialect:  1,
	}, "")
	if db != nil {
		_ = db.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if readyFile == "" {
		fmt.Fprintln(os.Stderr, "missing parent-death readiness file")
	}
	os.Exit(1)
}

func helperCreateDatabase(path string, exitCode *int) {
	if path == "" {
		return
	}
	if err := os.WriteFile(path, []byte("owned fixture database"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		*exitCode = 1
	}
}

func helperDropDatabase(path string, exitCode *int) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, err)
		*exitCode = 1
	}
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
	if got := scriptDialect(cfg); got != 3 {
		t.Fatalf("default fixture dialect = %d, want 3", got)
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
		{name: "server newline", cfg: Config{ISQL: "/tmp/isql", User: "SYSDBA", Password: "masterkey", Server: "localhost/3050\n"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateConfig(test.cfg); err == nil {
				t.Fatal("validateConfig accepted a control character")
			}
		})
	}
}

func TestServerPrefixIsPropagatedToFixtureScriptsAndConnectionString(t *testing.T) {
	cfg := Config{ISQL: "/tmp/isql", User: "SYSDBA", Password: "masterkey", Server: "localhost/3050"}
	path := "/tmp/example.ib"

	create := createScript(path, cfg, "CREATE TABLE T (ID INTEGER);")
	if !strings.Contains(create, "CREATE DATABASE 'localhost/3050:/tmp/example.ib'") {
		t.Fatalf("create script does not use server-prefixed attachment:\n%s", create)
	}
	drop := dropScript(path, cfg)
	if !strings.Contains(drop, "CONNECT 'localhost/3050:/tmp/example.ib'") {
		t.Fatalf("drop script does not use server-prefixed attachment:\n%s", drop)
	}

	db := &Database{Path: path, config: cfg}
	if got := db.ConnectionString(); got != "localhost/3050:/tmp/example.ib" {
		t.Fatalf("ConnectionString() = %q, want server-prefixed path", got)
	}
}

func TestConfigDialectDefaultsAndValidation(t *testing.T) {
	field, ok := reflect.TypeOf(Config{}).FieldByName("Dialect")
	if !ok {
		t.Fatal("testfixture.Config.Dialect is missing")
	}
	if field.Type != reflect.TypeOf(int(0)) {
		t.Fatalf("testfixture.Config.Dialect type = %v, want int", field.Type)
	}

	base := Config{ISQL: "/tmp/isql", User: "SYSDBA", Password: "masterkey"}
	for _, test := range []struct {
		name  string
		value int
		want  int
		valid bool
	}{
		{name: "default", value: 0, want: 3, valid: true},
		{name: "dialect one", value: 1, want: 1, valid: true},
		{name: "dialect three", value: 3, want: 3, valid: true},
		{name: "dialect two", value: 2, valid: false},
		{name: "negative", value: -1, valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			reflect.ValueOf(&cfg).Elem().FieldByName("Dialect").SetInt(int64(test.value))
			err := validateConfig(cfg)
			if (err == nil) != test.valid {
				t.Fatalf("validateConfig(Dialect=%d) error = %v, valid = %t", test.value, err, test.valid)
			}
			if test.valid {
				got, normalizeErr := normalizeDialect(test.value)
				if normalizeErr != nil {
					t.Fatalf("normalizeDialect(Dialect=%d) returned error: %v", test.value, normalizeErr)
				}
				if got != test.want {
					t.Fatalf("normalizeDialect(Dialect=%d) = %d, want %d", test.value, got, test.want)
				}
			}
		})
	}
}

func TestDialectIsPropagatedToISQLScripts(t *testing.T) {
	cfg := Config{ISQL: "/tmp/isql", User: "SYSDBA", Password: "masterkey"}
	field := reflect.ValueOf(&cfg).Elem().FieldByName("Dialect")
	if !field.IsValid() {
		t.Fatal("testfixture.Config.Dialect is missing")
	}
	field.SetInt(3)

	create := createScript("/tmp/example.ib", cfg, "CREATE TABLE T (ID INTEGER);")
	if !strings.Contains(create, "SET SQL DIALECT 3;") {
		t.Fatalf("create script does not select Dialect 3:\n%s", create)
	}
	if strings.Contains(create, "SET SQL DIALECT 1;") {
		t.Fatalf("create script selected Dialect 1 for Dialect 3:\n%s", create)
	}

	drop := dropScript("/tmp/example.ib", cfg)
	if !strings.Contains(drop, "SET SQL DIALECT 3;") {
		t.Fatalf("drop script does not select Dialect 3:\n%s", drop)
	}
	if strings.Contains(drop, "SET SQL DIALECT 1;") {
		t.Fatalf("drop script selected Dialect 1 for Dialect 3:\n%s", drop)
	}
}

func TestCreateRejectsInvalidDialectBeforeSubprocessExecution(t *testing.T) {
	cfg := helperConfig(t, "success")
	field := reflect.ValueOf(&cfg).Elem().FieldByName("Dialect")
	if !field.IsValid() {
		t.Fatal("testfixture.Config.Dialect is missing")
	}
	field.SetInt(2)

	db, err := Create(context.Background(), cfg, "")
	if db != nil {
		_ = db.Close()
		t.Fatal("Create returned a fixture for an invalid dialect")
	}
	if err == nil {
		t.Fatal("Create accepted an invalid dialect")
	}
}

func TestSQLScriptsEscapeLiterals(t *testing.T) {
	cfg := Config{ISQL: "/tmp/isql", User: "O'Reilly", Password: "p'ass"}
	path := "/tmp/fixture's/database's.ib"
	schema := "CREATE TABLE T (LABEL VARCHAR(20));"

	create := createScript(path, cfg, schema)
	for _, want := range []string{
		"SET SQL DIALECT 3;",
		"CREATE DATABASE '/tmp/fixture''s/database''s.ib'",
		"USER 'O''Reilly'",
		"PASSWORD 'p''ass'",
		"DEFAULT CHARACTER SET UTF8",
		schema,
		"COMMIT;",
		"SELECT 'INTERBASE_GO_FIXTURE_CREATE_COMPLETE' FROM RDB$DATABASE",
	} {
		if !strings.Contains(create, want) {
			t.Fatalf("create script does not contain %q:\n%s", want, create)
		}
	}

	drop := dropScript(path, cfg)
	for _, want := range []string{
		"SET SQL DIALECT 3;",
		"CONNECT '/tmp/fixture''s/database''s.ib'",
		"USER 'O''Reilly'",
		"PASSWORD 'p''ass'",
		"SELECT 'INTERBASE_GO_FIXTURE_DROP_COMPLETE' FROM RDB$DATABASE",
		"DROP DATABASE",
	} {
		if !strings.Contains(drop, want) {
			t.Fatalf("drop script does not contain %q:\n%s", want, drop)
		}
	}
}

func TestCompletionMarkerRequiresStandaloneOutputLine(t *testing.T) {
	marker := helperCreateCompletionMarker
	for _, test := range []struct {
		name   string
		output string
		want   bool
	}{
		{name: "result row with whitespace", output: "  " + marker + " \r\n", want: true},
		{name: "sql echo", output: "SELECT '" + marker + "' FROM RDB$DATABASE;\n", want: false},
		{name: "embedded in error", output: "error: " + marker + "\n", want: false},
		{name: "embedded in text", output: "prefix-" + marker + "-suffix\n", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := hasCompletionMarker(test.output, marker); got != test.want {
				t.Fatalf("hasCompletionMarker(%q) = %t, want %t", test.output, got, test.want)
			}
		})
	}
}

func TestCreateAcceptsExitOneWithCompletionMarker(t *testing.T) {
	cfg := helperConfig(t, "success-exit-one")
	db, err := Create(context.Background(), cfg, "")
	if err != nil {
		t.Fatalf("Create rejected a marked exit-one completion: %v", err)
	}
	if db == nil {
		t.Fatal("Create returned a nil database")
	}
	if _, err := os.Stat(db.Path); err != nil {
		t.Fatalf("created database is missing: %v", err)
	}

	t.Setenv("INTERBASE_FIXTURE_HELPER", "success")
	if err := db.Close(); err != nil {
		t.Fatalf("close marked exit-one fixture: %v", err)
	}
}

func TestCreateRejectsMarkedExitOneWithDescendantHoldingOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	t.Setenv("INTERBASE_FIXTURE_DESCENDANT_PID", pidFile)
	t.Cleanup(func() {
		contents, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(string(contents))
		if err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	started := time.Now()
	db, err := Create(context.Background(), helperConfig(t, "marked-exit-one-descendant-holds-pipes"), "")
	elapsed := time.Since(started)
	if db == nil {
		t.Fatal("Create returned a nil database with an incomplete output stream")
	}
	if err == nil {
		t.Fatal("Create accepted a marked exit-one command before output reached EOF")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Create waited %v instead of the finite output-drain delay", elapsed)
	}
	if !strings.Contains(err.Error(), "output") {
		t.Fatalf("Create error = %v, want output-completion diagnostics", err)
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
		t.Fatalf("close incomplete-output fixture: %v", closeErr)
	}
}

func TestCreateRejectsExitOneWithoutCompletionMarker(t *testing.T) {
	cfg := helperConfig(t, "exit-one-no-marker")
	db, err := Create(context.Background(), cfg, "")
	if db == nil {
		t.Fatal("Create returned a nil database with a partial setup error")
	}
	if err == nil {
		t.Fatal("Create accepted exit status one without a completion marker")
	}
	if !strings.Contains(err.Error(), "completion marker") {
		t.Fatalf("Create error = %v, want missing-marker diagnostics", err)
	}

	t.Setenv("INTERBASE_FIXTURE_HELPER", "success")
	if err := db.Close(); err != nil {
		t.Fatalf("close partial exit-one fixture: %v", err)
	}
}

func TestCreateRejectsCompletionMarkerWhenDatabaseFileIsMissing(t *testing.T) {
	cfg := helperConfig(t, "marker-create-missing-file")
	db, err := Create(context.Background(), cfg, "")
	if db == nil {
		t.Fatal("Create returned a nil database with a missing database file")
	}
	if err == nil {
		t.Fatal("Create accepted a completion marker without a database file")
	}
	if _, statErr := os.Stat(filepath.Dir(db.Path)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fixture directory was not removed after missing database proof: %v", statErr)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close after missing database proof: %v", err)
	}
}

func TestCreateRejectsSQLDiagnosticsAfterCompletionMarker(t *testing.T) {
	cfg := helperConfig(t, "marker-and-sql-error")
	db, err := Create(context.Background(), cfg, "")
	if db == nil {
		t.Fatal("Create returned a nil database with SQL diagnostics")
	}
	if err == nil {
		t.Fatal("Create accepted a completion marker after a failed SQL statement")
	}
	if !strings.Contains(err.Error(), "SQLCODE = -901") {
		t.Fatalf("Create error lost SQL diagnostics: %v", err)
	}
	if strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("Create reported subprocess status before SQL diagnostics: %v", err)
	}

	t.Setenv("INTERBASE_FIXTURE_HELPER", "success")
	if err := db.Close(); err != nil {
		t.Fatalf("close SQL-error fixture: %v", err)
	}
}

func TestCloseAcceptsExitOneWithCompletionMarker(t *testing.T) {
	db, err := Create(context.Background(), helperConfig(t, "success"), "")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	directory := filepath.Dir(db.Path)

	t.Setenv("INTERBASE_FIXTURE_HELPER", "success-exit-one")
	if err := db.Close(); err != nil {
		t.Fatalf("Close rejected a marked exit-one completion: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture directory still exists after marked drop: %v", err)
	}
}

func TestCloseRejectsCompletionMarkerWhenDatabaseFileRemains(t *testing.T) {
	db, err := Create(context.Background(), helperConfig(t, "success"), "")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	directory := filepath.Dir(db.Path)

	t.Setenv("INTERBASE_FIXTURE_HELPER", "drop-marker-keeps-file")
	if err := db.Close(); err == nil {
		t.Fatal("Close accepted a drop marker while the database file remained")
	}
	if _, err := os.Stat(db.Path); err != nil {
		t.Fatalf("database file disappeared during failed drop proof: %v", err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("fixture directory was not retained after failed drop proof: %v", err)
	}

	t.Setenv("INTERBASE_FIXTURE_HELPER", "success")
	if err := db.Close(); err != nil {
		t.Fatalf("Close did not retry after failed drop proof: %v", err)
	}
}

func TestCloseDoesNotReconnectAfterFailedDropRemovedDatabase(t *testing.T) {
	state := filepath.Join(t.TempDir(), "drop-attempt")
	t.Setenv("INTERBASE_FIXTURE_HELPER_STATE", state)
	db, err := Create(context.Background(), helperConfig(t, "success"), "")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	directory := filepath.Dir(db.Path)

	t.Setenv("INTERBASE_FIXTURE_HELPER", "drop-fails-after-removing-file")
	if err := db.Close(); err == nil {
		t.Fatal("Close hid a failed drop command")
	}
	if _, err := os.Stat(db.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database file still exists after partial drop: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned directory was not removed after partial drop: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close retried a completed partial drop: %v", err)
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
		Dialect:  1,
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

func TestCreateISQLDiesWhenOwningGoHelperIsKilled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("parent-death signals are Linux-specific")
	}

	readyFile := filepath.Join(t.TempDir(), "parent-death.ready")
	helper := exec.Command(os.Args[0], "-test.run", "^TestFixtureParentDeathHelper$")
	helper.Env = append(os.Environ(),
		"INTERBASE_FIXTURE_PARENT_DEATH_HELPER=1",
		"INTERBASE_FIXTURE_PARENT_DEATH_READY="+readyFile,
		"INTERBASE_FIXTURE_HELPER=",
	)
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	helper.Stdout = io.Discard
	helper.Stderr = io.Discard
	if err := helper.Start(); err != nil {
		t.Fatalf("start parent-death helper: %v", err)
	}

	var ready parentDeathReady
	helperWaited := false
	t.Cleanup(func() {
		if !helperWaited && helper.Process != nil {
			_ = helper.Process.Kill()
			_ = helper.Wait()
		}
		if ready.pid > 0 {
			if running, _ := processRunning(ready.pid); running {
				_ = syscall.Kill(-ready.pid, syscall.SIGKILL)
			}
		}
		if ready.path != "" {
			_ = os.Remove(ready.path)
			_ = os.Remove(filepath.Dir(ready.path))
		}
	})

	var err error
	ready, err = waitForParentDeathReady(readyFile, 5*time.Second)
	if err != nil {
		t.Fatalf("wait for parent-death helper readiness: %v", err)
	}
	running, err := processRunning(ready.pid)
	if err != nil {
		t.Fatalf("inspect isql test double %d: %v", ready.pid, err)
	}
	if !running {
		t.Fatalf("isql test double %d exited before its Go owner was killed", ready.pid)
	}

	helperGroup, err := syscall.Getpgid(helper.Process.Pid)
	if err != nil {
		t.Fatalf("get parent-death helper process group: %v", err)
	}
	isqlGroup, err := syscall.Getpgid(ready.pid)
	if err != nil {
		t.Fatalf("get isql test-double process group: %v", err)
	}
	if helperGroup == isqlGroup {
		t.Fatalf("isql test double remained in the Go helper process group %d", helperGroup)
	}

	if err := syscall.Kill(-helper.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("kill parent-death helper process group: %v", err)
	}
	if err := helper.Wait(); err == nil {
		t.Fatal("parent-death helper exited successfully after SIGTERM")
	}
	helperWaited = true

	if err := waitForProcessExit(ready.pid, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(ready.path); err != nil {
		t.Fatalf("owned database file after helper death: %v", err)
	}
	directory := filepath.Dir(ready.path)
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("owned fixture directory after helper death: %v", err)
	}

	cfg := helperConfig(t, "success")
	db := &Database{
		Path:           ready.path,
		config:         cfg,
		directory:      directory,
		ownedPath:      ready.path,
		databaseExists: true,
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture after helper death: %v", err)
	}
	if _, err := os.Stat(ready.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned database file remains after cleanup: %v", err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned fixture directory remains after cleanup: %v", err)
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
	return Config{ISQL: isql, User: "SYSDBA", Password: "masterkey", Dialect: 1}
}

type parentDeathReady struct {
	pid  int
	path string
}

func waitForParentDeathReady(path string, timeout time.Duration) (parentDeathReady, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		contents, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
			if len(lines) == 2 {
				pid, parseErr := strconv.Atoi(strings.TrimSpace(lines[0]))
				if parseErr == nil && pid > 0 && filepath.IsAbs(lines[1]) {
					return parentDeathReady{pid: pid, path: lines[1]}, nil
				}
				lastErr = fmt.Errorf("invalid readiness contents %q", contents)
			} else {
				lastErr = fmt.Errorf("incomplete readiness contents %q", contents)
			}
		} else {
			lastErr = err
		}
		if !time.Now().Before(deadline) {
			return parentDeathReady{}, fmt.Errorf("readiness did not become valid before timeout: %w", lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForProcessExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		running, err := processRunning(pid)
		if err != nil {
			return fmt.Errorf("inspect process %d while waiting for exit: %w", pid, err)
		}
		if !running {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("process %d remained alive for %s after its Go owner was killed", pid, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processRunning(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false, err
	}

	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	closeParen := strings.LastIndexByte(string(contents), ')')
	if closeParen < 0 || closeParen+2 >= len(contents) {
		return false, fmt.Errorf("malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(string(contents[closeParen+2:]))
	if len(fields) == 0 {
		return false, fmt.Errorf("malformed /proc/%d/stat state", pid)
	}
	return fields[0] != "Z", nil
}
