package interbase

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestBuildAttachmentMatchesPythonTLSShape(t *testing.T) {
	cfg := Config{
		Host:     "db.example/3050",
		Database: "/var/lib/interbase/example.ib",
		TLS: TLSConfig{
			Enabled:              true,
			ServerPublicFile:     "/etc/interbase/server.pem",
			ClientCertFile:       "/etc/interbase/client.pem",
			ClientPassPhrase:     "client-secret",
			ClientPassPhraseFile: "/etc/interbase/client.pass",
			ServerPublicPath:     "/etc/interbase/public",
		},
	}

	got, err := buildAttachment(cfg)
	if err != nil {
		t.Fatalf("buildAttachment returned error: %v", err)
	}
	want := "db.example/3050?ssl=true?serverPublicFile=/etc/interbase/server.pem?clientCertFile=/etc/interbase/client.pem?clientPassPhrase=client-secret?clientPassPhraseFile=/etc/interbase/client.pass?serverPublicPath=/etc/interbase/public??:/var/lib/interbase/example.ib"
	if got != want {
		t.Fatalf("attachment = %q, want %q", got, want)
	}
}

func TestBuildAttachmentRejectsAmbiguousHostAndTLSSettings(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "host trailing colon",
			cfg:  Config{Host: "db.example:", Database: "/db.ib"},
		},
		{
			name: "host contains query delimiter",
			cfg:  Config{Host: "db.example?ssl=true", Database: "/db.ib"},
		},
		{
			name: "disabled TLS has options",
			cfg: Config{
				Host:     "db.example",
				Database: "/db.ib",
				TLS:      TLSConfig{ServerPublicFile: "/etc/interbase/server.pem"},
			},
		},
		{
			name: "TLS without host",
			cfg: Config{
				Database: "/db.ib",
				TLS:      TLSConfig{Enabled: true},
			},
		},
		{
			name: "TLS value contains query delimiter",
			cfg: Config{
				Host:     "db.example",
				Database: "/db.ib",
				TLS:      TLSConfig{Enabled: true, ServerPublicFile: "/etc/interbase/public?bad"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildAttachment(tc.cfg); err == nil {
				t.Fatal("buildAttachment accepted an ambiguous configuration")
			}
		})
	}
}

func TestNewConnectorCopiesMutableParityConfiguration(t *testing.T) {
	cfg := Config{
		Database: "/db.ib",
		Host:     "db.example",
		User:     "alice",
		Password: "password",
		TLS: TLSConfig{
			Enabled:          true,
			ClientPassPhrase: "phrase",
		},
		TransactionOptions: TransactionOptions{
			NoWait: true,
			TableReservations: []TableReservation{
				{
					Table:       "EXAMPLE",
					SharingMode: TableSharingProtected,
					AccessMode:  TableAccessWrite,
				},
			},
		},
	}
	opened, err := NewConnector(cfg)
	if err != nil {
		t.Fatalf("NewConnector returned error: %v", err)
	}

	cfg.TLS.ClientPassPhrase = "changed"
	cfg.TransactionOptions.TableReservations[0].Table = "CHANGED"
	cfg.TransactionOptions.TableReservations = append(cfg.TransactionOptions.TableReservations,
		TableReservation{Table: "SECOND", SharingMode: TableSharingShared, AccessMode: TableAccessRead})

	got := opened.(*connector).cfg
	if got.TLS.ClientPassPhrase != "phrase" {
		t.Fatalf("connector retained mutated TLS pass phrase %q", got.TLS.ClientPassPhrase)
	}
	if len(got.TransactionOptions.TableReservations) != 1 ||
		got.TransactionOptions.TableReservations[0].Table != "EXAMPLE" {
		t.Fatalf("connector retained mutable transaction options: %#v", got.TransactionOptions)
	}
}

func TestBuildTPBMatchesPythonOrderAndOptions(t *testing.T) {
	tests := []struct {
		name    string
		options driver.TxOptions
		config  TransactionOptions
		want    []byte
	}{
		{
			name:    "default writable read committed",
			options: driver.TxOptions{},
			want:    []byte{3, 9, 6, 15, 17},
		},
		{
			name: "read-only read committed no-wait no-record",
			options: driver.TxOptions{
				Isolation: driver.IsolationLevel(sql.LevelReadCommitted),
				ReadOnly:  true,
			},
			config: TransactionOptions{
				NoWait:          true,
				NoRecordVersion: true,
			},
			want: []byte{3, 8, 7, 15, 18},
		},
		{
			name:    "repeatable read is snapshot",
			options: driver.TxOptions{Isolation: driver.IsolationLevel(sql.LevelRepeatableRead)},
			want:    []byte{3, 9, 6, 2},
		},
		{
			name:    "snapshot",
			options: driver.TxOptions{Isolation: driver.IsolationLevel(sql.LevelSnapshot)},
			want:    []byte{3, 9, 6, 2},
		},
		{
			name:    "serializable",
			options: driver.TxOptions{Isolation: driver.IsolationLevel(sql.LevelSerializable)},
			want:    []byte{3, 9, 6, 1},
		},
		{
			name: "table reservation",
			config: TransactionOptions{
				TableReservations: []TableReservation{
					{Table: "EXAMPLE", SharingMode: TableSharingProtected, AccessMode: TableAccessWrite},
				},
			},
			want: []byte{3, 9, 6, 15, 17, 11, 8, 'E', 'X', 'A', 'M', 'P', 'L', 'E', 0, 4},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildTPB(tc.options, tc.config)
			if err != nil {
				t.Fatalf("buildTPB returned error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("TPB = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildTPBRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		name    string
		options driver.TxOptions
		config  TransactionOptions
	}{
		{
			name:    "unsupported isolation",
			options: driver.TxOptions{Isolation: driver.IsolationLevel(sql.LevelReadUncommitted)},
		},
		{
			name:    "no record version with snapshot",
			options: driver.TxOptions{Isolation: driver.IsolationLevel(sql.LevelSnapshot)},
			config:  TransactionOptions{NoRecordVersion: true},
		},
		{
			name:   "invalid sharing mode",
			config: TransactionOptions{TableReservations: []TableReservation{{Table: "EXAMPLE", SharingMode: 99, AccessMode: TableAccessRead}}},
		},
		{
			name:   "invalid access mode",
			config: TransactionOptions{TableReservations: []TableReservation{{Table: "EXAMPLE", SharingMode: TableSharingShared, AccessMode: 99}}},
		},
		{
			name:   "non ASCII table name",
			config: TransactionOptions{TableReservations: []TableReservation{{Table: "café", SharingMode: TableSharingShared, AccessMode: TableAccessRead}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildTPB(tc.options, tc.config); err == nil {
				t.Fatal("buildTPB accepted invalid options")
			}
		})
	}
}

func TestBuildTPBRejectsMalformedQuotedTableNames(t *testing.T) {
	for _, table := range []string{`"`, `"MixedCase`, `MixedCase"`, `MIX"ED`} {
		t.Run(table, func(t *testing.T) {
			_, err := buildTPB(driver.TxOptions{}, TransactionOptions{
				TableReservations: []TableReservation{{
					Table:       table,
					SharingMode: TableSharingShared,
					AccessMode:  TableAccessRead,
				}},
			})
			if err == nil {
				t.Fatalf("buildTPB accepted malformed quoted table name %q", table)
			}
		})
	}
}

func TestQuotedTableNormalizationIsIdempotentAndPreservesCase(t *testing.T) {
	options := TransactionOptions{TableReservations: []TableReservation{{
		Table:       `"MixedCase"`,
		SharingMode: TableSharingProtected,
		AccessMode:  TableAccessWrite,
	}}}
	first, err := normalizeTransactionOptions(options)
	if err != nil {
		t.Fatalf("first normalization returned error: %v", err)
	}
	second, err := normalizeTransactionOptions(first)
	if err != nil {
		t.Fatalf("second normalization returned error: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("normalization is not idempotent: first=%#v second=%#v", first, second)
	}
	if got := first.TableReservations[0].Table; got != `"MixedCase"` {
		t.Fatalf("normalized quoted table = %q, want %q", got, `"MixedCase"`)
	}

	gotConnector, err := NewConnector(Config{
		Database:           "/db.ib",
		User:               "alice",
		TransactionOptions: options,
	})
	if err != nil {
		t.Fatalf("NewConnector returned error: %v", err)
	}
	stored := gotConnector.(*connector).cfg.TransactionOptions
	if got := stored.TableReservations[0].Table; got != `"MixedCase"` {
		t.Fatalf("connector normalized quoted table = %q, want %q", got, `"MixedCase"`)
	}
	got, err := buildTPB(driver.TxOptions{}, stored)
	if err != nil {
		t.Fatalf("buildTPB from connector options returned error: %v", err)
	}
	want := []byte{3, 9, 6, 15, 17, 11, 10, 'M', 'i', 'x', 'e', 'd', 'C', 'a', 's', 'e', 0, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("connector TPB = %v, want %v", got, want)
	}
}

func TestNativeErrorPreservesStatusThroughRedaction(t *testing.T) {
	err := parseNativeError("attach database failed (SQLCODE -902, native status 335544472): password=secret")
	var nativeErr *Error
	if !errors.As(err, &nativeErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if nativeErr.Operation != "attach database" || nativeErr.SQLCode != -902 || nativeErr.NativeCode != 335544472 {
		t.Fatalf("native error metadata = %#v", nativeErr)
	}

	sanitized := sanitizeError("connect", err, "secret")
	if !errors.As(sanitized, &nativeErr) {
		t.Fatalf("sanitized error type = %T, want *Error", sanitized)
	}
	if nativeErr.Operation != "connect" || strings.Contains(sanitized.Error(), "secret") {
		t.Fatalf("sanitized native error = %v", sanitized)
	}
}

func TestSanitizeErrorRedactsEveryConfiguredConnectionValue(t *testing.T) {
	cfg := Config{
		Database:                 "database-secret",
		Host:                     "host-secret",
		User:                     "user-secret",
		Password:                 "password-secret",
		Role:                     "role-secret",
		EncryptedPassword:        "encrypted-password-secret",
		SystemEncryptionPassword: "system-password-secret",
		TLS: TLSConfig{
			ServerPublicFile:     "server-public-file-secret",
			ClientCertFile:       "client-cert-file-secret",
			ClientPassPhrase:     "client-pass-phrase-secret",
			ClientPassPhraseFile: "client-pass-file-secret",
			ServerPublicPath:     "server-public-path-secret",
		},
	}
	message := strings.Join(configSecrets(cfg), " ")
	sanitized := sanitizeError("connect", &Error{Message: message}, configSecrets(cfg)...)
	for _, secret := range configSecrets(cfg) {
		if strings.Contains(sanitized.Error(), secret) {
			t.Fatalf("sanitized error leaked configured value %q: %v", secret, sanitized)
		}
	}
}

func TestSanitizeErrorPreservesJoinedErrorTreeAndMetadata(t *testing.T) {
	secret := "connection-secret"
	primary := errors.New("primary failure: " + secret)
	cleanup := &Error{
		Operation:  "cleanup connection",
		SQLCode:    -901,
		NativeCode: 335544999,
		Message:    "cleanup failure: " + secret,
	}
	original := errors.Join(
		fmt.Errorf("primary wrapper: %w", primary),
		fmt.Errorf("cleanup wrapper: %w", cleanup),
	)

	sanitized := sanitizeError("connect", original, secret)
	if !errors.Is(sanitized, primary) {
		t.Fatalf("sanitized error lost primary errors.Is identity: %v", sanitized)
	}
	var cleanupError *Error
	if !errors.As(sanitized, &cleanupError) {
		t.Fatalf("sanitized error lost cleanup *Error metadata: %v", sanitized)
	}
	if cleanupError.Operation != cleanup.Operation || cleanupError.SQLCode != cleanup.SQLCode ||
		cleanupError.NativeCode != cleanup.NativeCode {
		t.Fatalf("cleanup metadata = %#v, want %#v", cleanupError, cleanup)
	}
	if !strings.Contains(sanitized.Error(), "primary wrapper") ||
		!strings.Contains(sanitized.Error(), "cleanup wrapper") {
		t.Fatalf("sanitized error lost joined messages: %v", sanitized)
	}
	assertErrorTreeContainsNoSecret(t, sanitized, secret)
}

func TestSanitizeErrorRedactsOverlappingSecretsAsOriginalRanges(t *testing.T) {
	message := "user=alice-private-suffix; overlapping=abcd"
	sanitized := sanitizeError("connect", errors.New(message),
		"alice", "alice-private-suffix", "abc", "bcd")
	for _, fragment := range []string{"alice", "private-suffix", "abc", "bcd", "abcd"} {
		if strings.Contains(sanitized.Error(), fragment) {
			t.Fatalf("sanitized error leaked overlapping fragment %q: %v", fragment, sanitized)
		}
	}
}

func assertErrorTreeContainsNoSecret(t *testing.T, err error, secret string) {
	t.Helper()
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error tree leaked secret %q in %T: %v", secret, err, err)
	}
	switch unwrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range unwrapped.Unwrap() {
			assertErrorTreeContainsNoSecret(t, child, secret)
		}
	case interface{ Unwrap() error }:
		if child := unwrapped.Unwrap(); child != nil {
			assertErrorTreeContainsNoSecret(t, child, secret)
		}
	}
}
