//go:build integration

package services

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

func TestNativeServiceOutputUsesLengthDelimitedFrame(t *testing.T) {
	server := os.Getenv("INTERBASE_TEST_SERVER")
	user := os.Getenv("INTERBASE_TEST_USER")
	password := os.Getenv("INTERBASE_TEST_PASSWORD")
	if server == "" || user == "" {
		t.Skip("INTERBASE_TEST_SERVER and INTERBASE_TEST_USER are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	manager, err := Open(ctx, Config{Host: server, User: user, Password: password})
	if err != nil {
		if manager != nil {
			if closeErr := manager.Close(); closeErr != nil {
				t.Logf("close services manager after attach error: %v", closeErr)
			}
		}
		t.Fatalf("open services manager: %v", err)
	}
	defer func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close services manager: %v", err)
		}
	}()

	native, ok := manager.backend.(*nativeService)
	if !ok {
		t.Fatalf("services backend = %T, want *nativeService", manager.backend)
	}
	request, err := (LogRequest{}).build()
	if err != nil {
		t.Fatalf("build log request: %v", err)
	}
	if err := native.start(request); err != nil {
		t.Fatalf("start log request: %v", err)
	}

	for attempt := 0; attempt < 2400; attempt++ {
		raw, truncated, err := native.query(infoToEOF, maxServiceQueryBuffer)
		if err != nil {
			t.Fatalf("query service output: %v", err)
		}
		if truncated {
			t.Fatal("native service output was truncated at the maximum buffer size")
		}
		if len(raw) == 1 && (raw[0] == infoDataNotReady || raw[0] == infoTruncated) {
			time.Sleep(servicePollInterval)
			continue
		}
		if len(raw) < 4 || raw[0] != infoToEOF {
			t.Fatalf("raw service output = %v, want isc_info_svc_to_eof frame", raw)
		}
		length := int(binaryLittleEndianUint16(raw[1:3]))
		markerPosition := 3 + length
		if markerPosition >= len(raw) || markerPosition != len(raw)-1 {
			t.Fatalf("raw service output = %v, want length-delimited payload and marker", raw)
		}
		marker := raw[markerPosition]
		if marker != infoEnd && marker != infoFlagEnd && marker != infoTruncated {
			t.Fatalf("raw service output marker = %d, want info_end, info_flag_end, or info_truncated", marker)
		}
		if _, _, err := decodeOutput(raw, truncated); err != nil {
			t.Fatalf("decode native service output frame: %v", err)
		}
		if marker == infoEnd {
			return
		}
	}
	t.Fatal("service log did not return a terminal output frame")
}

func TestNativeServiceJobReturnsCleanOutputAndCanBeReused(t *testing.T) {
	server := os.Getenv("INTERBASE_TEST_SERVER")
	user := os.Getenv("INTERBASE_TEST_USER")
	password := os.Getenv("INTERBASE_TEST_PASSWORD")
	if server == "" || user == "" {
		t.Skip("INTERBASE_TEST_SERVER and INTERBASE_TEST_USER are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	manager, err := Open(ctx, Config{Host: server, User: user, Password: password})
	if err != nil {
		if manager != nil {
			if closeErr := manager.Close(); closeErr != nil {
				t.Logf("close services manager after attach error: %v", closeErr)
			}
		}
		t.Fatalf("open services manager: %v", err)
	}
	defer func() {
		if err := manager.Close(); err != nil {
			t.Errorf("close services manager: %v", err)
		}
	}()

	job, err := manager.Logs(ctx)
	if err != nil {
		t.Fatalf("start log job: %v", err)
	}
	output, err := io.ReadAll(job)
	if err != nil {
		t.Fatalf("read log job output: %v", err)
	}
	if err := job.Wait(ctx); err != nil {
		t.Fatalf("wait log job: %v", err)
	}
	if !job.CompletionKnown() {
		t.Fatal("log job did not prove completion")
	}
	if len(output) > 0 && output[0] < ' ' && output[0] != '\n' && output[0] != '\r' && output[0] != '\t' {
		t.Fatalf("log output starts with a protocol byte: %v", output)
	}

	next, err := manager.Logs(ctx)
	if err != nil {
		t.Fatalf("reuse manager after log job: %v", err)
	}
	if err := next.Close(); err != nil {
		t.Fatalf("close reused log job: %v", err)
	}
}
