//go:build integration

package integration_test

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/events"
	"interbase-go/services"
)

// TestNativeTLSVerification requires an explicitly opted-in disposable server,
// not the ordinary integration fixture. Run the binary INSIDE its network-none
// container: localhost:3065 must serve a localhost-only certificate, and 3050
// must be a working plaintext listener for the no-fallback control.
//
// Removing ssl=true, dropping the CA option, disabling certificate/hostname
// verification, or retrying plaintext must fail this matrix. Every negative is
// bracketed by successful native TLS operations so downtime cannot pass it.
func TestNativeTLSVerification(t *testing.T) {
	if os.Getenv("INTERBASE_TLS_TEST") != "1" {
		t.Skip("requires explicit disposable TLS fixture; see integration/README.md")
	}
	requireEnv := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("TLS fixture requires %s", name)
		}
		return value
	}
	ca := requireEnv("INTERBASE_TLS_CA")
	wrongCA := requireEnv("INTERBASE_TLS_WRONG_CA")
	trusted := readTLSCertificate(t, ca)
	untrusted := readTLSCertificate(t, wrongCA)
	if !trusted.IsCA || !untrusted.IsCA || trusted.Equal(untrusted) {
		t.Fatal("fixture requires distinct, valid CA certificates")
	}
	server := readTLSCertificate(t, requireEnv("INTERBASE_TLS_SERVER_CERT"))
	if err := server.VerifyHostname("localhost"); err != nil {
		t.Fatalf("fixture server certificate must identify localhost: %v", err)
	}
	if err := server.VerifyHostname("wrong.parity.invalid"); err == nil {
		t.Fatal("fixture server certificate must NOT identify wrong.parity.invalid")
	}
	roots := x509.NewCertPool()
	roots.AddCert(trusted)
	if _, err := server.Verify(x509.VerifyOptions{Roots: roots, DNSName: "localhost"}); err != nil {
		t.Fatalf("fixture certificate must verify with the correct trust anchor: %v", err)
	}
	wrongRoots := x509.NewCertPool()
	wrongRoots.AddCert(untrusted)
	if _, err := server.Verify(x509.VerifyOptions{Roots: wrongRoots, DNSName: "localhost"}); err == nil {
		t.Fatal("fixture certificate must NOT verify with the wrong trust anchor")
	}
	cfg := interbase.Config{
		Host:     "localhost/3065",
		Database: requireEnv("INTERBASE_TLS_DATABASE"),
		User:     requireEnv("INTERBASE_TLS_USER"),
		Password: requireEnv("INTERBASE_TLS_PASSWORD"),
		TLS:      interbase.TLSConfig{Enabled: true, ServerPublicFile: ca},
	}
	// The runner maps this deliberately wrong DNS name to the same loopback
	// address as localhost. Refused connections must not count as verification.
	conn, err := net.DialTimeout("tcp", "wrong.parity.invalid:3065", 5*time.Second)
	if err != nil {
		t.Fatalf("wrong-host TLS endpoint is not reachable: %v", err)
	}
	wrongAddress := conn.RemoteAddr().String()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err = net.DialTimeout("tcp", "localhost:3065", 5*time.Second)
	if err != nil {
		t.Fatalf("TLS control endpoint is not reachable: %v", err)
	}
	correctAddress := conn.RemoteAddr().String()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if wrongAddress != correctAddress {
		t.Fatal("wrong hostname must resolve to the same TLS listener as localhost")
	}

	for _, api := range []struct {
		name  string
		probe func(*testing.T, interbase.Config) error
	}{
		{"root_sql", probeTLSSQL},
		{"root_direct", probeTLSDirect},
		{"events", probeTLSEvents},
		{"services", probeTLSServices},
	} {
		t.Run(api.name, func(t *testing.T) {
			for _, scenario := range []string{"correct_trust", "wrong_trust", "wrong_hostname", "mandatory_tls_no_fallback"} {
				t.Run(scenario, func(t *testing.T) {
					if err := api.probe(t, cfg); err != nil {
						t.Fatalf("positive TLS control: %v", err)
					}
					candidate := cfg
					switch scenario {
					case "correct_trust":
						return
					case "wrong_trust":
						candidate.TLS.ServerPublicFile = wrongCA
					case "wrong_hostname":
						candidate.Host = "wrong.parity.invalid/3065"
					case "mandatory_tls_no_fallback":
						candidate.Host = "localhost/3050"
						plain := candidate
						plain.TLS = interbase.TLSConfig{}
						if err := api.probe(t, plain); err != nil {
							t.Fatalf("plaintext listener positive control: %v", err)
						}
					}
					err := api.probe(t, candidate)
					if err == nil {
						t.Errorf("%s was accepted; certificate verification or mandatory TLS is broken", scenario)
						if err := api.probe(t, cfg); err != nil {
							t.Fatalf("positive TLS recovery control: %v", err)
						}
						return
					}
					var native *interbase.Error
					if !errors.As(err, &native) || native.SQLCode != -902 || native.NativeCode != 335544721 {
						t.Fatalf("expected native network/TLS rejection, not validation/authentication/timeout: %v", err)
					}
					// The current native adapter exposes SQLCODE/network status but
					// flattens detailed SSL text. Controls above/below distinguish
					// verification failure from an unavailable fixture; vendor isql
					// can supply the detailed certificate/handshake diagnostics.
					// Do not print configuration/credentials, even on unexpected failures.
					t.Logf("native rejection: %s", strings.ReplaceAll(err.Error(), cfg.Password, "[REDACTED]"))
					if err := api.probe(t, cfg); err != nil {
						t.Fatalf("positive TLS recovery control: %v", err)
					}
				})
			}
		})
	}
}

func readTLSCertificate(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read public TLS fixture certificate: %v", err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("TLS fixture must provide a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse TLS fixture certificate: %v", err)
	}
	if now := time.Now(); now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		t.Fatal("TLS fixture certificate is not currently valid")
	}
	return cert
}

// Probe errors represent attachment failures ONLY. Failures after successful
// attachment must fail the test rather than masquerade as TLS rejection.
func probeTLSSQL(t *testing.T, cfg interbase.Config) error {
	t.Helper()
	connector, err := interbase.NewConnector(cfg)
	if err != nil {
		t.Fatalf("invalid TLS test configuration: %v", err)
	}
	db := sql.OpenDB(connector)
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close SQL TLS probe: %v", err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	var value int
	if err := db.QueryRowContext(ctx, "SELECT 42 FROM RDB$DATABASE").Scan(&value); err != nil {
		t.Fatalf("query after SQL attachment succeeded: %v", err)
	}
	if value != 42 {
		t.Fatalf("TLS query returned %d, want 42", value)
	}
	return nil
}

func probeTLSDirect(t *testing.T, cfg interbase.Config) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	attachment, err := interbase.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := attachment.Close(); err != nil {
			t.Errorf("close direct TLS probe: %v", err)
		}
	}()
	if _, err := attachment.Diagnostics(ctx); err != nil {
		t.Fatalf("diagnostics after direct attachment succeeded: %v", err)
	}
	return nil
}

func probeTLSEvents(t *testing.T, cfg interbase.Config) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	subscription, err := events.Subscribe(ctx, cfg, "GO_TLS_PROBE")
	if err != nil {
		return err
	}
	// Subscribe waits for the native event baseline, exercising more than the
	// connection string builder. Notification payload encryption is not asserted.
	if err := subscription.Close(); err != nil {
		t.Fatalf("close subscribed TLS probe: %v", err)
	}
	return nil
}

func probeTLSServices(t *testing.T, cfg interbase.Config) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manager, err := services.Open(ctx, services.Config{
		Host: cfg.Host, User: cfg.User, Password: cfg.Password, TLS: cfg.TLS,
	})
	if err != nil {
		if manager != nil {
			return errors.Join(err, manager.Close())
		}
		return err
	}
	defer func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close services TLS probe: %v", err)
		}
	}()
	version, err := manager.ServerVersion(ctx)
	if err != nil || version == "" {
		t.Fatalf("server version after services attachment succeeded: %q, %v", version, err)
	}
	return nil
}
