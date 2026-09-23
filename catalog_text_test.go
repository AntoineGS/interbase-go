package interbase

import (
	"strings"
	"testing"
)

func TestCatalogTextCharsetNormalizationAndConnector(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		invalid bool
	}{
		{"", "", false},
		{"  ", "", false},
		{" win1250 ", "WIN1250", false},
		{"win1252", "WIN1252", false},
		{"iso8859_1", "ISO8859_1", false},
		{"ascii", "ASCII", false},
		{"NONE", "", true},
		{"OCTETS", "", true},
		{"UTF8", "", true},
		{"UNICODE_FSS", "", true},
		{"WIN1250\x00", "", true},
		{"unknown", "", true},
	}

	for _, test := range tests {
		t.Run(test.in, func(t *testing.T) {
			got, err := normalizeCatalogTextCharset(test.in)
			if test.invalid {
				if err == nil {
					t.Fatal("normalizeCatalogTextCharset returned no error for invalid value")
				}
				if !strings.Contains(strings.ToLower(err.Error()), "catalog text charset") {
					t.Fatalf("error %q does not identify catalog text charset", err)
				}
				_, connectorErr := NewConnector(Config{
					Database:           "test.ib",
					User:               "tester",
					CatalogTextCharset: test.in,
				})
				if connectorErr == nil || !strings.Contains(strings.ToLower(connectorErr.Error()), "catalog text charset") {
					t.Fatalf("NewConnector invalid charset error = %v, want catalog text charset error", connectorErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeCatalogTextCharset(%q): %v", test.in, err)
			}
			if got != test.want {
				t.Fatalf("normalizeCatalogTextCharset(%q) = %q, want %q", test.in, got, test.want)
			}

			opened, err := NewConnector(Config{
				Database:           "test.ib",
				User:               "tester",
				CatalogTextCharset: test.in,
			})
			if err != nil {
				t.Fatalf("NewConnector: %v", err)
			}
			if got := opened.(*connector).cfg.CatalogTextCharset; got != test.want {
				t.Fatalf("connector CatalogTextCharset = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCatalogTextCharsetNativeIDs(t *testing.T) {
	tests := []struct {
		charset string
		want    int16
	}{
		{"", 0},
		{"ASCII", 2},
		{"ISO8859_1", 21},
		{"WIN1250", 51},
		{"WIN1252", 53},
	}
	for _, test := range tests {
		t.Run(test.charset, func(t *testing.T) {
			got, err := catalogTextCharsetID(test.charset)
			if err != nil {
				t.Fatalf("catalogTextCharsetID(%q): %v", test.charset, err)
			}
			if got != test.want {
				t.Fatalf("catalogTextCharsetID(%q) = %d, want %d", test.charset, got, test.want)
			}
		})
	}
}

func TestCatalogTextCharsetDoesNotChangeAttachmentCharset(t *testing.T) {
	opened, err := NewConnector(Config{
		Database:           "test.ib",
		User:               "tester",
		Charset:            "UTF8",
		CatalogTextCharset: "WIN1250",
	})
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	got := opened.(*connector).cfg
	if got.Charset != "UTF8" || got.CatalogTextCharset != "WIN1250" {
		t.Fatalf("connector charsets = attachment %q, catalog %q; want UTF8, WIN1250", got.Charset, got.CatalogTextCharset)
	}

	opened, err = NewConnector(Config{
		Database:           "test.ib",
		User:               "tester",
		Charset:            "WIN1252",
		CatalogTextCharset: "ASCII",
	})
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	got = opened.(*connector).cfg
	if got.Charset != "WIN1252" || got.CatalogTextCharset != "ASCII" {
		t.Fatalf("connector charsets = attachment %q, catalog %q; want WIN1252, ASCII", got.Charset, got.CatalogTextCharset)
	}
}
