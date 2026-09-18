package interbase

import (
	"database/sql/driver"
	"strconv"
	"testing"
	"time"
)

var hardeningBenchmarkSink any

func BenchmarkHardeningConvertArgument(b *testing.B) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "nil", value: nil},
		{name: "string", value: "interbase benchmark value"},
		{name: "bytes", value: []byte("interbase benchmark bytes")},
		{name: "int64", value: int64(42)},
		{name: "float64", value: float64(42.5)},
		{name: "bool", value: true},
		{name: "timestamp", value: time.Date(2026, time.September, 17, 12, 34, 56, 789000000, time.UTC)},
	}
	for _, test := range tests {
		test := test
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				value, err := convertArgument(test.value)
				if err != nil {
					b.Fatal(err)
				}
				hardeningBenchmarkSink = value
			}
		})
	}
}

func BenchmarkHardeningConvertNamedValues(b *testing.B) {
	for _, size := range []int{1, 8, 64} {
		b.Run("values="+benchmarkIntString(size), func(b *testing.B) {
			values := make([]driver.NamedValue, size)
			for i := range values {
				values[i] = driver.NamedValue{Ordinal: i + 1, Value: []byte("value")}
			}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				arguments, err := convertNamedValues(values)
				if err != nil {
					b.Fatal(err)
				}
				hardeningBenchmarkSink = arguments
			}
		})
	}
}

func BenchmarkHardeningConvertQuery(b *testing.B) {
	values := []driver.NamedValue{
		{Ordinal: 1, Value: int64(42)},
		{Ordinal: 2, Value: "benchmark"},
		{Ordinal: 3, Value: []byte("payload")},
	}
	const query = "SELECT ? FROM RDB$DATABASE WHERE ? = ?"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		arguments, err := convertQuery(query, values)
		if err != nil {
			b.Fatal(err)
		}
		hardeningBenchmarkSink = arguments
	}
}

func BenchmarkHardeningFormatScaledInteger(b *testing.B) {
	for _, test := range []struct {
		name  string
		value int64
		scale int16
	}{
		{name: "integer", value: 123456789, scale: 0},
		{name: "decimal", value: -123456789, scale: -4},
		{name: "large-scale", value: 123, scale: 12},
	} {
		test := test
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				value, err := formatScaledInteger(test.value, test.scale)
				if err != nil {
					b.Fatal(err)
				}
				hardeningBenchmarkSink = value
			}
		})
	}
}

func BenchmarkHardeningNormalizeDirectArray(b *testing.B) {
	array := Array{
		Bounds: []ArrayBound{{Lower: 0, Upper: 31}},
		Elements: []any{
			int64(0), int64(1), int64(2), int64(3),
			int64(4), int64(5), int64(6), int64(7),
			int64(8), int64(9), int64(10), int64(11),
			int64(12), int64(13), int64(14), int64(15),
			int64(16), int64(17), int64(18), int64(19),
			int64(20), int64(21), int64(22), int64(23),
			int64(24), int64(25), int64(26), int64(27),
			int64(28), int64(29), int64(30), int64(31),
		},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		normalized, err := normalizeDirectArray(array)
		if err != nil {
			b.Fatal(err)
		}
		hardeningBenchmarkSink = normalized
	}
}

func BenchmarkHardeningNewConnector(b *testing.B) {
	cfg := Config{
		Database:       "/tmp/interbase-hardening-benchmark.ib",
		Host:           "localhost/3050",
		User:           "SYSDBA",
		Password:       "masterkey",
		Charset:        "UTF8",
		ConnectTimeout: 1500 * time.Millisecond,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		connector, err := NewConnector(cfg)
		if err != nil {
			b.Fatal(err)
		}
		hardeningBenchmarkSink = connector
	}
}

func benchmarkIntString(value int) string {
	return strconv.Itoa(value)
}
