package interbase

import (
	"database/sql/driver"
	"math"
	"strconv"
	"testing"
	"time"
)

const (
	maxHardeningFuzzString = 4 << 10
	maxHardeningFuzzBytes  = 4 << 10
	maxHardeningFuzzArray  = 128
)

func FuzzConfigAndAttachmentParsing(f *testing.F) {
	f.Add("/tmp/example.ib", "", "SYSDBA", "masterkey", "", false)
	f.Add("/tmp/example.ib", "localhost/3050", "alice", "secret", "UTF8", false)
	f.Add("/tmp/example.ib", "localhost", "alice", "secret", "WIN1252", true)
	f.Add("", "", "", "", "not-a-charset", false)
	f.Add("/tmp/with\x00nul.ib", "localhost", "alice", "secret", "UTF8", false)

	f.Fuzz(func(t *testing.T, database, host, user, password, charset string, tlsEnabled bool) {
		for _, value := range []string{database, host, user, password, charset} {
			if len(value) > maxHardeningFuzzString {
				t.Skip()
			}
		}

		cfg := Config{
			Database: database,
			Host:     host,
			User:     user,
			Password: password,
			Charset:  charset,
		}
		if tlsEnabled {
			cfg.TLS = TLSConfig{
				Enabled:          true,
				ServerPublicFile: "server-public.pem",
			}
		}

		attachment, attachmentErr := buildAttachment(cfg)
		connectorValue, connectorErr := NewConnector(cfg)
		if attachmentErr == nil && len(attachment) > math.MaxInt16 {
			t.Fatalf("successful attachment exceeded native length bound: %d", len(attachment))
		}
		if connectorErr != nil {
			if connectorValue != nil {
				t.Fatalf("NewConnector returned both connector and error: %v", connectorErr)
			}
			if attachmentErr == nil {
				// NewConnector performs additional credential and transaction
				// validation, so an attachment that parses is not sufficient for
				// a complete connector configuration.
				return
			}
			return
		}
		if connectorValue == nil {
			t.Fatal("NewConnector returned a nil connector without an error")
		}
		connector, ok := connectorValue.(*connector)
		if !ok || connector == nil {
			t.Fatalf("NewConnector returned unexpected connector type %T", connectorValue)
		}
		normalizedAttachment, err := buildAttachment(connector.cfg)
		if err != nil {
			t.Fatalf("normalized connector configuration no longer builds an attachment: %v", err)
		}
		if attachmentErr != nil {
			t.Fatalf("connector accepted a configuration rejected by buildAttachment: %v", attachmentErr)
		}
		if normalizedAttachment != attachment {
			t.Fatalf("normalized attachment = %q, initial attachment = %q", normalizedAttachment, attachment)
		}
	})
}

func FuzzSQLParameterHandling(f *testing.F) {
	f.Add("SELECT ? FROM RDB$DATABASE", int64(1), "plain", []byte{0, 1, 2}, true)
	f.Add("-- comment\n SELECT ? FROM RDB$DATABASE", int64(-42), "", []byte(nil), false)
	f.Add("SET SUBSCRIPTION SUB_CUSTOMER_CHANGE ACTIVE", int64(1), "ignored", []byte("bytes"), true)
	f.Add("SELECT \x00", int64(1), "ignored", []byte(nil), false)

	f.Fuzz(func(t *testing.T, query string, integer int64, text string, bytesValue []byte, boolean bool) {
		if len(query) > maxHardeningFuzzString || len(text) > maxHardeningFuzzString ||
			len(bytesValue) > maxHardeningFuzzBytes {
			t.Skip()
		}

		values := []driver.NamedValue{
			{Ordinal: 1, Value: integer},
			{Ordinal: 2, Value: text},
			{Ordinal: 3, Value: bytesValue},
			{Ordinal: 4, Value: boolean},
		}
		arguments, err := convertQuery(query, values)
		if err != nil {
			return
		}
		if len(arguments) != len(values) {
			t.Fatalf("converted %d arguments from %d values", len(arguments), len(values))
		}
		for index, argument := range arguments {
			switch argument.kind {
			case argumentNull, argumentString, argumentInt64, argumentFloat64,
				argumentBool, argumentTimestamp, argumentBytes:
			default:
				t.Fatalf("argument %d has an invalid database/sql kind %d", index, argument.kind)
			}
		}
	})
}

func FuzzNamedValueOrdinalHandling(f *testing.F) {
	f.Add(uint8(0), "value")
	f.Add(uint8(1), "value")
	f.Add(uint8(2), "value")
	f.Add(uint8(255), "value")

	f.Fuzz(func(t *testing.T, ordinal uint8, value string) {
		if len(value) > maxHardeningFuzzString {
			t.Skip()
		}
		values := []driver.NamedValue{{Ordinal: int(ordinal), Value: value}}
		arguments, err := convertNamedValues(values)
		if err != nil {
			return
		}
		if len(arguments) != 1 || arguments[0].kind != argumentString || arguments[0].stringValue != value {
			t.Fatalf("converted ordinal %d value = %#v, want one string argument", ordinal, arguments)
		}
	})
}

func FuzzTypedValueConversions(f *testing.F) {
	f.Add(uint8(0), "text", []byte("bytes"), int64(-42), float64(1.25), true)
	f.Add(uint8(1), "", []byte(nil), int64(0), float64(0), false)
	f.Add(uint8(2), "timestamp", []byte{1, 2, 3}, int64(1700000000), float64(-2.5), true)
	f.Add(uint8(3), "array", []byte{0xff}, int64(math.MinInt64), math.MaxFloat64, false)

	f.Fuzz(func(t *testing.T, kind uint8, text string, bytesValue []byte, integer int64, floating float64, boolean bool) {
		if len(text) > maxHardeningFuzzString || len(bytesValue) > maxHardeningFuzzBytes {
			t.Skip()
		}

		var input any
		wantKind := argumentNull
		switch kind % 7 {
		case 0:
			input = nil
		case 1:
			input, wantKind = text, argumentString
		case 2:
			input, wantKind = bytesValue, argumentBytes
		case 3:
			input, wantKind = integer, argumentInt64
		case 4:
			input, wantKind = floating, argumentFloat64
		case 5:
			input, wantKind = boolean, argumentBool
		case 6:
			input, wantKind = time.Unix(integer, 0).UTC(), argumentTimestamp
		}

		argument, err := convertArgument(input)
		if err != nil {
			return
		}
		if argument.kind != wantKind {
			t.Fatalf("convertArgument(%T) kind = %d, want %d", input, argument.kind, wantKind)
		}
	})
}

func FuzzScaledIntegerFormatting(f *testing.F) {
	f.Add(int64(0), int16(0))
	f.Add(int64(-123), int16(-2))
	f.Add(int64(5), int16(-2))
	f.Add(int64(12), int16(2))
	f.Add(int64(1), int16(10000))

	f.Fuzz(func(t *testing.T, value int64, scale int16) {
		if scale < -512 || scale > 512 {
			t.Skip()
		}
		formatted, err := formatScaledInteger(value, scale)
		if err != nil {
			return
		}
		if len(formatted) > 1024 {
			t.Fatalf("scaled integer output is unexpectedly large: %d bytes", len(formatted))
		}
		if scale == 0 && formatted != strconv.FormatInt(value, 10) {
			t.Fatalf("scale-zero value = %q, want %q", formatted, strconv.FormatInt(value, 10))
		}
	})
}

func FuzzDirectArrayNormalization(f *testing.F) {
	f.Add(int16(0), []byte{1, 2, 3})
	f.Add(int16(-2), []byte{0, 255})
	f.Add(int16(32767), []byte{1})
	f.Add(int16(0), []byte(nil))

	f.Fuzz(func(t *testing.T, lower int16, raw []byte) {
		if len(raw) > maxHardeningFuzzArray {
			t.Skip()
		}
		upper := int64(lower) + int64(len(raw)) - 1
		if upper > math.MaxInt16 {
			t.Skip()
		}
		array := Array{
			Bounds: []ArrayBound{{Lower: int32(lower), Upper: int32(upper)}},
			Elements: func() []any {
				elements := make([]any, len(raw))
				for index, value := range raw {
					elements[index] = int64(value)
				}
				return elements
			}(),
		}

		normalized, err := normalizeDirectArray(array)
		if err != nil {
			return
		}
		if normalized == nil || len(normalized.Bounds) != 1 || len(normalized.Elements) != len(raw) {
			t.Fatalf("normalized array = %#v, want one dimension and %d elements", normalized, len(raw))
		}
		if err := validateDirectArray(*normalized); err != nil {
			t.Fatalf("normalized array failed validation: %v", err)
		}
	})
}

func FuzzInformationResponseParsing(f *testing.F) {
	f.Add([]byte{InfoDatabaseVersion, 1, 0, 'V', infoEnd}, uint8(InfoDatabaseVersion))
	f.Add([]byte{InfoTransactionID, 4, 0, 1, 0, 0, 0, infoEnd}, uint8(InfoTransactionID))
	f.Add([]byte{infoTruncated}, uint8(InfoDatabaseVersion))
	f.Add([]byte(nil), uint8(0))

	f.Fuzz(func(t *testing.T, response []byte, requested uint8) {
		if len(response) > maxHardeningFuzzBytes {
			t.Skip()
		}
		item, err := parseInfoItem(response, requested)
		if err == nil {
			if item.Code != requested || len(item.Data) > len(response) {
				t.Fatalf("parsed item = %#v for request %d", item, requested)
			}
		}
		items, err := parseInfoItems(response, requested)
		if err == nil {
			if len(items) == 0 {
				t.Fatal("parseInfoItems returned no items without an error")
			}
			for index, parsed := range items {
				if parsed.Code != requested || len(parsed.Data) > len(response) {
					t.Fatalf("parsed item %d = %#v for request %d", index, parsed, requested)
				}
			}
		}
	})
}
