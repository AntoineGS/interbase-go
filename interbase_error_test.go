package interbase

import (
	"errors"
	"strings"
	"testing"
)

func TestParseNativeErrorExposesGDSCodesAndExceptionNumber(t *testing.T) {
	err := parseNativeError("execute procedure failed (SQLCODE -836, native status 335544517, gds 335544517 335544382, exception 42): \nexception 42\nLP_EX\nboom")
	var nativeErr *Error
	if !errors.As(err, &nativeErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if nativeErr.SQLCode != -836 || nativeErr.NativeCode != 335544517 {
		t.Fatalf("native error metadata = %#v", nativeErr)
	}
	if got := nativeErr.GDSCodes(); len(got) != 2 || got[0] != 335544517 || got[1] != 335544382 {
		t.Fatalf("GDSCodes() = %v, want [335544517 335544382]", got)
	}
	if got := nativeErr.GDSCode(); got != 335544517 {
		t.Fatalf("GDSCode() = %d, want 335544517", got)
	}
	number, ok := nativeErr.ExceptionNumber()
	if !ok || number != 42 {
		t.Fatalf("ExceptionNumber() = %d, %v, want 42, true", number, ok)
	}
	if !strings.Contains(nativeErr.Message, "boom") || strings.Contains(nativeErr.Message, "gds ") {
		t.Fatalf("Message = %q", nativeErr.Message)
	}
	if strings.Contains(nativeErr.Error(), "gds 335544517") {
		t.Fatalf("Error() renders status codes: %q", nativeErr.Error())
	}
}

func TestParseNativeErrorWithoutCodesKeepsAccessorsEmpty(t *testing.T) {
	err := parseNativeError("attach database failed (SQLCODE -902, native status 335544472): x")
	var nativeErr *Error
	if !errors.As(err, &nativeErr) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if nativeErr.NativeCode != 335544472 {
		t.Fatalf("NativeCode = %d", nativeErr.NativeCode)
	}
	if len(nativeErr.GDSCodes()) != 0 || nativeErr.GDSCode() != 0 {
		t.Fatalf("codes = %v, want none", nativeErr.GDSCodes())
	}
	if _, ok := nativeErr.ExceptionNumber(); ok {
		t.Fatal("ExceptionNumber() reported a number")
	}
	var nilErr *Error
	if nilErr.GDSCodes() != nil || nilErr.GDSCode() != 0 {
		t.Fatal("nil Error accessors must be empty")
	}
}

func TestErrorCodesSurviveSanitizationAndGDSCodesIsACopy(t *testing.T) {
	err := parseNativeError("x failed (SQLCODE -836, native status 335544517, gds 335544517, exception 7): secret")
	sanitized := sanitizeError("op", err, "secret")
	var nativeErr *Error
	if !errors.As(sanitized, &nativeErr) {
		t.Fatalf("error type = %T", sanitized)
	}
	if strings.Contains(nativeErr.Error(), "secret") {
		t.Fatalf("secret leaked: %q", nativeErr.Error())
	}
	if number, ok := nativeErr.ExceptionNumber(); !ok || number != 7 {
		t.Fatalf("ExceptionNumber() = %d, %v", number, ok)
	}
	nativeErr.GDSCodes()[0] = 1
	if nativeErr.GDSCode() != 335544517 {
		t.Fatal("GDSCodes() exposed internal storage")
	}
}

func TestGDSCodeByName(t *testing.T) {
	for name, want := range map[string]int64{
		"sing_select_err":      335544652,
		"except":               335544517,
		"arith_except":         335544321,
		"unique_key_violation": 335544665,
		"foreign_key":          335544466,
		"deadlock":             335544336,
		"lock_conflict":        335544345,
		"no_dup":               335544349,
		"not_valid":            335544347,
		"update_conflict":      335544451,
	} {
		got, ok := GDSCodeByName(name)
		if !ok || got != want {
			t.Errorf("GDSCodeByName(%q) = %d, %v, want %d", name, got, ok, want)
		}
	}
	if got, ok := GDSCodeByName("  LOCK_CONFLICT "); !ok || got != 335544345 {
		t.Errorf("case-insensitive lookup = %d, %v", got, ok)
	}
	if got, ok := GDSCodeByName("isc_deadlock"); !ok || got != 335544336 {
		t.Errorf("isc_ prefix lookup = %d, %v", got, ok)
	}
	if _, ok := GDSCodeByName("no_such_code"); ok {
		t.Error("unknown name resolved")
	}
}
