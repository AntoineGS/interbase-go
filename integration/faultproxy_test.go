//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/internal/faultproxy"
	"interbase-go/internal/testfixture"
)

const faultTestsEnvironment = "INTERBASE_FAULT_TESTS"

func TestFaultProxyReplacesResetIdlePoolConnection(t *testing.T) {
	requireFaultTests(t)
	fixture, cfg := createFaultFixture(t)
	proxy := newFaultProxy(t, cfg.Server)
	db := openFaultDatabase(t, proxy.Addr(), fixture.Path, cfg)
	assertFaultCountry(t, db)
	proxy.ResetConnections()
	if err := queryFaultCountry(db); err == nil {
		t.Fatal("query on reset pooled connection succeeded")
	}
	assertFaultCountry(t, db)
}

func TestFaultProxyLostResponsesHaveOneObservableEffect(t *testing.T) {
	requireFaultTests(t)
	fixture, cfg := createFaultFixture(t)
	proxy := newFaultProxy(t, cfg.Server)
	for _, operation := range []string{"execute", "fetch", "commit", "rollback"} {
		t.Run(operation, func(t *testing.T) {
			marker := faultMarker(t, operation)
			beforeGenerator := faultGeneratorValue(t, fixture.Path, cfg)
			result := runFaultWorker(t, proxy, fixture.Path, cfg, operation, marker, faultReset)
			if result.Err != nil {
				t.Fatalf("%s worker failed instead of reporting its native lost-response error: %v\n%s", operation, result.Err, result.Output)
			}
			afterGenerator := faultGeneratorValue(t, fixture.Path, cfg)
			count := faultEffectCount(t, fixture.Path, cfg, marker)
			switch operation {
			case "execute":
				if count != 0 || afterGenerator-beforeGenerator != 1 {
					t.Fatalf("execute rows=%d generator delta=%d, want rollback rows=0 plus one server execution", count, afterGenerator-beforeGenerator)
				}
			case "commit":
				if count != 1 || afterGenerator-beforeGenerator != 1 {
					t.Fatalf("commit rows=%d generator delta=%d, want exactly one committed server execution", count, afterGenerator-beforeGenerator)
				}
			case "rollback":
				if count != 0 || afterGenerator-beforeGenerator != 1 {
					t.Fatalf("rollback rows=%d generator delta=%d, want rows=0 plus one staged execution", count, afterGenerator-beforeGenerator)
				}
			case "fetch":
				if count != 0 {
					t.Fatalf("%s effect count = %d, want 0", operation, count)
				}
				if afterGenerator != beforeGenerator {
					t.Fatalf("fetch changed generator from %d to %d", beforeGenerator, afterGenerator)
				}
			}
			assertFaultCountry(t, openDirectFaultDatabase(t, fixture.Path, cfg))
		})
	}
}

func TestFaultProxyBlackholeKillsOnlyBoundWorker(t *testing.T) {
	requireFaultTests(t)
	fixture, cfg := createFaultFixture(t)
	proxy := newFaultProxy(t, cfg.Server)
	result := runFaultWorker(t, proxy, fixture.Path, cfg, "fetch", faultMarker(t, "blackhole"), faultBlackhole)
	if result.Err == nil {
		t.Fatal("blackholed worker exited successfully")
	}
	if !result.KilledAtDeadline {
		t.Fatalf("blackholed worker exited before its external deadline: %v\n%s", result.Err, result.Output)
	}
	assertFaultCountry(t, openDirectFaultDatabase(t, fixture.Path, cfg))
}

func TestFaultProxyBlackholedConnectUsesNativeConnectTimeout(t *testing.T) {
	requireFaultTests(t)
	fixture, cfg := createFaultFixture(t)
	proxy := newFaultProxy(t, cfg.Server)
	result := runFaultWorker(t, proxy, fixture.Path, cfg, "connect_timeout", faultMarker(t, "connect"), faultNativeTimeout)
	if result.Err != nil {
		t.Fatalf("connect-timeout worker failed: %v\n%s", result.Err, result.Output)
	}
	if result.Elapsed < time.Second || result.Elapsed > 6*time.Second {
		t.Fatalf("native connect timeout elapsed = %v, want 1s through 6s", result.Elapsed)
	}
	assertFaultCountry(t, openDirectFaultDatabase(t, fixture.Path, cfg))
}

func TestFaultOwnedContainerRestartReplacesConnection(t *testing.T) {
	requireFaultTests(t)
	container, token := os.Getenv("INTERBASE_FAULT_OWNED_CONTAINER"), os.Getenv("INTERBASE_FAULT_OWNED_TOKEN")
	if container == "" || token == "" {
		t.Skip("owned-container restart is available through test-faults-docker.sh")
	}
	if output, err := exec.Command("docker", "inspect", "--format", "{{index .Config.Labels \"interbase-go.fault-token\"}}", container).CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != token {
		t.Fatalf("refusing restart of unverified container %q: %v %s", container, err, output)
	}
	fixture, cfg := createFaultFixture(t)
	db := openDirectFaultDatabase(t, fixture.Path, cfg)
	assertFaultCountry(t, db)
	if output, err := exec.Command("docker", "restart", "--", container).CombinedOutput(); err != nil {
		t.Fatalf("restart verified owned container: %v: %s", err, output)
	}
	if err := queryFaultCountry(db); err == nil {
		t.Fatal("query on connection surviving server restart succeeded")
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := queryFaultCountry(db); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("fresh connection did not recover after owned server restart")
}

func TestFaultWorker(t *testing.T) {
	operation := os.Getenv("INTERBASE_FAULT_WORKER_OPERATION")
	if operation == "" {
		t.Skip("fault worker helper")
	}
	if os.Getenv(faultTestsEnvironment) != "1" {
		t.Fatal("fault worker requires explicit opt-in")
	}
	control := workerOwnershipHandshake(t, operation)
	defer control.Close()

	config := interbase.Config{Host: os.Getenv("INTERBASE_FAULT_HOST"), Database: os.Getenv("INTERBASE_FAULT_DATABASE"), User: os.Getenv("INTERBASE_FAULT_USER"), Password: os.Getenv("INTERBASE_FAULT_PASSWORD"), Dialect: 1}
	if operation == "connect_timeout" {
		config.ConnectTimeout = time.Second
	}
	connector, err := interbase.NewConnector(config)
	if err != nil {
		t.Fatalf("worker connector: %v", err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if operation == "connect_timeout" {
		workerOperationBarrier(t, control, operation)
		started := time.Now()
		err = db.PingContext(context.Background())
		elapsed := time.Since(started)
		if err == nil {
			t.Fatal("blackholed connect unexpectedly succeeded")
		}
		if _, writeErr := fmt.Fprintf(control, "elapsed %s\n", elapsed); writeErr != nil {
			t.Fatalf("report native timeout: %v", writeErr)
		}
		return
	}

	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("worker dedicated connection: %v", err)
	}
	defer conn.Close()
	marker := os.Getenv("INTERBASE_FAULT_MARKER")
	var operationErr error
	switch operation {
	case "execute":
		tx, beginErr := conn.BeginTx(context.Background(), nil)
		if beginErr != nil {
			t.Fatalf("begin execute: %v", beginErr)
		}
		defer tx.Rollback()
		stmt, prepareErr := tx.PrepareContext(context.Background(), "INSERT INTO GO_FAULT_LOG (SEQ, MARKER, OPERATION) VALUES (GEN_ID(GO_FAULT_GENERATOR, 1), ?, ?)")
		if prepareErr != nil {
			t.Fatalf("prepare execute: %v", prepareErr)
		}
		defer stmt.Close()
		workerOperationBarrier(t, control, operation)
		_, operationErr = stmt.ExecContext(context.Background(), marker, operation)
	case "fetch":
		stmt, prepareErr := conn.PrepareContext(context.Background(), "SELECT PAYLOAD FROM GO_FAULT_ROWS")
		if prepareErr != nil {
			t.Fatalf("prepare fetch: %v", prepareErr)
		}
		defer stmt.Close()
		rows, queryErr := stmt.QueryContext(context.Background())
		if queryErr != nil {
			t.Fatalf("start fetch: %v", queryErr)
		}
		defer rows.Close()
		workerOperationBarrier(t, control, operation)
		for rows.Next() {
			var payload string
			if scanErr := rows.Scan(&payload); scanErr != nil {
				operationErr = scanErr
				break
			}
		}
		if operationErr == nil {
			operationErr = rows.Err()
		}
		if operationErr == nil {
			t.Fatal("large fetch reached EOF without a network error")
		}
	case "commit", "rollback":
		tx, beginErr := conn.BeginTx(context.Background(), nil)
		if beginErr != nil {
			t.Fatalf("begin %s: %v", operation, beginErr)
		}
		defer tx.Rollback()
		stmt, prepareErr := tx.PrepareContext(context.Background(), "INSERT INTO GO_FAULT_LOG (SEQ, MARKER, OPERATION) VALUES (GEN_ID(GO_FAULT_GENERATOR, 1), ?, ?)")
		if prepareErr != nil {
			t.Fatalf("prepare %s: %v", operation, prepareErr)
		}
		defer stmt.Close()
		if _, stageErr := stmt.ExecContext(context.Background(), marker, operation); stageErr != nil {
			t.Fatalf("stage %s: %v", operation, stageErr)
		}
		workerOperationBarrier(t, control, operation)
		if operation == "commit" {
			operationErr = tx.Commit()
		} else {
			operationErr = tx.Rollback()
		}
	default:
		t.Fatalf("unknown fault operation %q", operation)
	}
	if operationErr == nil {
		t.Fatalf("%s unexpectedly succeeded after fault injection", operation)
	}
}

func TestFaultWorkerRejectsUnapprovedInvocationBeforeNativeSetup(t *testing.T) {
	for _, test := range []struct {
		name        string
		environment []string
		want        string
	}{
		{"missing opt-in", []string{"INTERBASE_FAULT_WORKER_OPERATION=execute"}, "requires explicit opt-in"},
		{"missing parent token", []string{faultTestsEnvironment + "=1", "INTERBASE_FAULT_WORKER_OPERATION=execute"}, "lacks parent ownership token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestFaultWorker$")
			command.Env = withoutFaultWorkerEnvironment(os.Environ())
			command.Env = append(command.Env, test.environment...)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("unapproved worker succeeded: %s", output)
			}
			if !strings.Contains(string(output), test.want) {
				t.Fatalf("worker output = %q, want %q", output, test.want)
			}
		})
	}
}

func withoutFaultWorkerEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, variable := range environment {
		if strings.HasPrefix(variable, "INTERBASE_FAULT_") {
			continue
		}
		if strings.HasPrefix(variable, faultTestsEnvironment+"=") {
			continue
		}
		filtered = append(filtered, variable)
	}
	return filtered
}

func workerOwnershipHandshake(t *testing.T, operation string) net.Conn {
	t.Helper()
	token := os.Getenv("INTERBASE_FAULT_PARENT_TOKEN")
	if token == "" {
		t.Fatal("fault worker lacks parent ownership token")
	}
	connection, err := net.Dial("tcp", os.Getenv("INTERBASE_FAULT_CONTROL_ADDR"))
	if err != nil {
		t.Fatalf("worker control dial: %v", err)
	}
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(connection, "owner %s\n", token); err != nil {
		connection.Close()
		t.Fatalf("worker ownership signal: %v", err)
	}
	line := make([]byte, len("owned\n"))
	if _, err := io.ReadFull(connection, line); err != nil || string(line) != "owned\n" {
		connection.Close()
		t.Fatalf("worker ownership authorization = %q, %v", line, err)
	}
	return connection
}

func workerOperationBarrier(t *testing.T, connection net.Conn, operation string) {
	t.Helper()
	if _, err := fmt.Fprintf(connection, "ready %s\n", operation); err != nil {
		t.Fatalf("worker operation ready: %v", err)
	}
	line := make([]byte, len("go\n"))
	if _, err := io.ReadFull(connection, line); err != nil || string(line) != "go\n" {
		t.Fatalf("worker operation authorization = %q, %v", line, err)
	}
}

type faultMode uint8

const (
	faultReset faultMode = iota
	faultBlackhole
	faultNativeTimeout
)

type faultWorkerResult struct {
	Err              error
	Output           string
	Elapsed          time.Duration
	KilledAtDeadline bool
}

func runFaultWorker(t *testing.T, proxy *faultproxy.Proxy, database string, cfg testfixture.Config, operation, marker string, mode faultMode) faultWorkerResult {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen worker control: %v", err)
	}
	defer listener.Close()
	token := fmt.Sprintf("fault-parent-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	command := exec.Command(os.Args[0], "-test.run=^TestFaultWorker$")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	command.Env = append(os.Environ(), faultTestsEnvironment+"=1", "INTERBASE_FAULT_WORKER_OPERATION="+operation, "INTERBASE_FAULT_HOST=127.0.0.1/"+strconv.Itoa(proxyPort(t, proxy)), "INTERBASE_FAULT_DATABASE="+database, "INTERBASE_FAULT_USER="+cfg.User, "INTERBASE_FAULT_PASSWORD="+cfg.Password, "INTERBASE_FAULT_MARKER="+marker, "INTERBASE_FAULT_PARENT_TOKEN="+token, "INTERBASE_FAULT_CONTROL_ADDR="+listener.Addr().String())
	if err := command.Start(); err != nil {
		t.Fatalf("start %s worker: %v", operation, err)
	}
	var waitErr error
	finished := make(chan struct{})
	go func() { waitErr = command.Wait(); close(finished) }()
	stopWorker := func() {
		select {
		case <-finished:
			return
		default:
			_ = command.Process.Kill()
			<-finished
		}
	}
	defer stopWorker()
	worker := acceptFaultWorker(t, listener, ctx)
	defer worker.Close()
	expectFaultLine(t, worker, "owner "+token+"\n")
	if err := writeFaultControl(worker, "owned\n"); err != nil {
		t.Fatalf("authorize worker: %v", err)
	}
	expectFaultLine(t, worker, "ready "+operation+"\n")
	proxy.PauseResponses()
	if err := writeFaultControl(worker, "go\n"); err != nil {
		t.Fatalf("release worker: %v", err)
	}
	if err := proxy.WaitResponseBlocked(ctx); err != nil {
		t.Fatalf("wait for exact %s response: %v", operation, err)
	}
	result := faultWorkerResult{}
	switch mode {
	case faultReset:
		proxy.ResetConnections()
		select {
		case <-finished:
			result.Err = waitErr
		case <-ctx.Done():
			t.Fatalf("%s worker did not exit after reset", operation)
		}
	case faultNativeTimeout:
		select {
		case <-finished:
			result.Err = waitErr
			if result.Err != nil {
				t.Fatalf("native connect-timeout worker failed: %v\n%s", result.Err, output.String())
			}
			result.Elapsed = readFaultElapsed(t, worker)
		case <-ctx.Done():
			stopWorker()
			result.Err = waitErr
			t.Fatalf("native connect timeout did not end the blocked attachment\n%s", output.String())
		}
	case faultBlackhole:
		select {
		case <-finished:
			result.Err = waitErr
			t.Fatalf("blackholed worker exited before deadline: %v\n%s", result.Err, result.Output)
		case <-ctx.Done():
			result.KilledAtDeadline = true
			stopWorker()
			result.Err = waitErr
		}
	}
	result.Output = output.String()
	return result
}

func acceptFaultWorker(t *testing.T, listener net.Listener, ctx context.Context) net.Conn {
	t.Helper()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	connection, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept worker control: %v", err)
	}
	return connection
}
func expectFaultLine(t *testing.T, connection net.Conn, want string) {
	t.Helper()
	got := make([]byte, len(want))
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(connection, got); err != nil || string(got) != want {
		t.Fatalf("worker control = %q, %v; want %q", got, err, want)
	}
}
func writeFaultControl(connection net.Conn, value string) error {
	if err := connection.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	_, err := io.WriteString(connection, value)
	return err
}
func readFaultElapsed(t *testing.T, connection net.Conn) time.Duration {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := readFaultControlLine(connection)
	if err != nil {
		t.Fatalf("read native timeout elapsed: %v", err)
	}
	value, ok := strings.CutPrefix(line, "elapsed ")
	if !ok {
		t.Fatalf("native timeout report = %q", line)
	}
	elapsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		t.Fatalf("parse native timeout elapsed %q: %v", value, err)
	}
	return elapsed
}

func readFaultControlLine(connection net.Conn) (string, error) {
	var line []byte
	one := []byte{0}
	for len(line) < 128 {
		if _, err := io.ReadFull(connection, one); err != nil {
			return "", err
		}
		line = append(line, one[0])
		if one[0] == '\n' {
			return string(line), nil
		}
	}
	return "", errors.New("fault control line exceeds 128 bytes")
}

func requireFaultTests(t *testing.T) {
	t.Helper()
	if os.Getenv(faultTestsEnvironment) != "1" {
		t.Skipf("set %s=1 to run owned destructive fault tests", faultTestsEnvironment)
	}
}
func createFaultFixture(t *testing.T) (*testfixture.Database, testfixture.Config) {
	t.Helper()
	cfg, err := testfixture.FromEnv()
	if err != nil {
		t.Fatalf("fault fixture config: %v", err)
	}
	cfg.Dialect = 1
	ctx, cancel := context.WithTimeout(context.Background(), fixtureSetupTimeout)
	defer cancel()
	fixture, err := testfixture.Create(ctx, cfg, faultSchema())
	if err != nil {
		t.Fatalf("create fault fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := fixture.Close(); err != nil {
			t.Errorf("close fault fixture: %v", err)
		}
	})
	return fixture, cfg
}
func faultSchema() string {
	var schema strings.Builder
	schema.WriteString(fixtureSchema)
	schema.WriteString("\nCREATE GENERATOR GO_FAULT_GENERATOR;\nCREATE TABLE GO_FAULT_LOG (SEQ INTEGER, MARKER VARCHAR(80), OPERATION VARCHAR(20));\nCREATE TABLE GO_FAULT_ROWS (PAYLOAD VARCHAR(4096));\n")
	payload := strings.Repeat("x", 4096)
	for range 128 {
		fmt.Fprintf(&schema, "INSERT INTO GO_FAULT_ROWS (PAYLOAD) VALUES ('%s');\n", payload)
	}
	schema.WriteString("COMMIT;\n")
	return schema.String()
}
func newFaultProxy(t *testing.T, server string) *faultproxy.Proxy {
	t.Helper()
	proxy, err := faultproxy.New(strings.Replace(server, "/", ":", 1))
	if err != nil {
		t.Fatalf("start fault proxy: %v", err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Errorf("close fault proxy: %v", err)
		}
	})
	return proxy
}
func proxyPort(t *testing.T, proxy *faultproxy.Proxy) int {
	t.Helper()
	_, port, err := net.SplitHostPort(proxy.Addr())
	if err != nil {
		t.Fatalf("split proxy address: %v", err)
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse proxy port: %v", err)
	}
	return value
}
func openFaultDatabase(t *testing.T, address, database string, cfg testfixture.Config) *sql.DB {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split fault proxy address: %v", err)
	}
	connector, err := interbase.NewConnector(interbase.Config{Host: "127.0.0.1/" + port, Database: database, User: cfg.User, Password: cfg.Password, Dialect: 1})
	if err != nil {
		t.Fatalf("fault connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func openDirectFaultDatabase(t *testing.T, database string, cfg testfixture.Config) *sql.DB {
	t.Helper()
	connector, err := interbase.NewConnector(interbase.Config{Host: cfg.Server, Database: database, User: cfg.User, Password: cfg.Password, Dialect: 1})
	if err != nil {
		t.Fatalf("direct fault connector: %v", err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func queryFaultCountry(db *sql.DB) error {
	var country string
	return db.QueryRowContext(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country)
}
func assertFaultCountry(t *testing.T, db *sql.DB) {
	t.Helper()
	var country string
	if err := db.QueryRowContext(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("fresh fault connection query: %v", err)
	}
	if country != "USA" {
		t.Fatalf("fresh fault connection country = %q, want USA", country)
	}
}
func faultEffectCount(t *testing.T, database string, cfg testfixture.Config, marker string) int {
	t.Helper()
	db := openDirectFaultDatabase(t, database, cfg)
	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM GO_FAULT_LOG WHERE MARKER = ?", marker).Scan(&count); err != nil {
		t.Fatalf("count fault effects: %v", err)
	}
	return count
}
func faultGeneratorValue(t *testing.T, database string, cfg testfixture.Config) int64 {
	t.Helper()
	db := openDirectFaultDatabase(t, database, cfg)
	var value int64
	if err := db.QueryRowContext(context.Background(), "SELECT GEN_ID(GO_FAULT_GENERATOR, 0) FROM RDB$DATABASE").Scan(&value); err != nil {
		t.Fatalf("read fault generator: %v", err)
	}
	return value
}
func faultMarker(t *testing.T, operation string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", operation, time.Now().UnixNano())
}
