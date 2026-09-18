package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEncodeBackupRequestUsesOfficialServiceParameterLayout(t *testing.T) {
	req := BackupRequest{
		SourceDatabase: "source.ib",
		Destinations: []BackupDestination{
			{Path: "first.ibk", MaxBytes: 4096},
			{Path: "second.ibk"},
		},
		Options: BackupOptions{
			IgnoreChecksums:  true,
			MetadataOnly:     true,
			NoGarbageCollect: true,
			NonTransportable: true,
			ConvertExternal:  true,
			Expand:           true,
		},
	}

	got, err := req.build()
	if err != nil {
		t.Fatalf("build backup request: %v", err)
	}

	want := []byte{
		1, // isc_action_svc_backup
		106, 9, 0, 's', 'o', 'u', 'r', 'c', 'e', '.', 'i', 'b',
		5, 9, 0, 'f', 'i', 'r', 's', 't', '.', 'i', 'b', 'k',
		7, 0x00, 0x10, 0x00, 0x00, // 4096, little-endian VAX integer
		5, 10, 0, 's', 'e', 'c', 'o', 'n', 'd', '.', 'i', 'b', 'k',
		108, 0xED, 0x00, 0x00, 0x00, // option mask
		107, // isc_spb_verbose
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded request = %v, want %v", got, want)
	}
}

func TestEncodeDatabaseStatisticsRequestDoesNotAddVerboseMarker(t *testing.T) {
	got, err := (DatabaseStatisticsRequest{Database: "db.ib"}).build()
	if err != nil {
		t.Fatalf("build statistics request: %v", err)
	}
	want := []byte{
		11, // isc_action_svc_db_stats
		106, 5, 0, 'd', 'b', '.', 'i', 'b',
		108, 0x09, 0x00, 0x00, 0x00, // data and index pages
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded statistics request = %v, want %v", got, want)
	}
}

func TestEncodeDatabaseStatisticsHeaderOverridesOtherSections(t *testing.T) {
	got, err := (DatabaseStatisticsRequest{
		Database: "db.ib",
		Options: DatabaseStatisticsOptions{
			OnlyLogPages:          true,
			OnlyHeaderPages:       true,
			IncludeSystemTables:   true,
			IncludeRecordVersions: true,
		},
	}).build()
	if err != nil {
		t.Fatalf("build header-only statistics request: %v", err)
	}
	want := []byte{
		11, // isc_action_svc_db_stats
		106, 5, 0, 'd', 'b', '.', 'i', 'b',
		108, 0x04, 0x00, 0x00, 0x00, // header pages only
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded header-only statistics request = %v, want %v", got, want)
	}
}

func TestEncodeDumpRequestMatchesPythonServiceParameterOrder(t *testing.T) {
	got, err := (DumpRequest{
		Database:  "source.ib",
		DumpFile:  "dump.ibd",
		Overwrite: true,
	}).build()
	if err != nil {
		t.Fatalf("build dump request: %v", err)
	}
	want := []byte{
		16,                          // isc_action_svc_dump
		108, 0x00, 0x00, 0x08, 0x00, // isc_spb_dmp_create
		106, 9, 0, 's', 'o', 'u', 'r', 'c', 'e', '.', 'i', 'b',
		5, 8, 0, 'd', 'u', 'm', 'p', '.', 'i', 'b', 'd',
		20, // isc_spb_dmp_overwrite
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded dump request = %v, want %v", got, want)
	}
}

func TestEncodeModifyUserSendsEmptyOptionalFields(t *testing.T) {
	got, err := (ModifyUserRequest{User: User{
		Name:     "IBGO_USER",
		Password: "new-password",
	}}).build()
	if err != nil {
		t.Fatalf("build modify-user request: %v", err)
	}
	want := []byte{
		6, // isc_action_svc_modify_user
		7, 9, 0, 'I', 'B', 'G', 'O', '_', 'U', 'S', 'E', 'R',
		8, 12, 0, 'n', 'e', 'w', '-', 'p', 'a', 's', 's', 'w', 'o', 'r', 'd',
		10, 0, 0, // clear first name
		11, 0, 0, // clear middle name
		12, 0, 0, // clear last name
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded modify-user request = %v, want %v", got, want)
	}
}

func TestBackupRequestRejectsUnsafeOrAmbiguousDestinations(t *testing.T) {
	tests := []struct {
		name string
		req  BackupRequest
		want string
	}{
		{
			name: "missing source",
			req:  BackupRequest{Destinations: []BackupDestination{{Path: "backup.ibk"}}},
			want: "source database",
		},
		{
			name: "missing destination",
			req:  BackupRequest{SourceDatabase: "source.ib"},
			want: "destination",
		},
		{
			name: "destination size count",
			req: BackupRequest{
				SourceDatabase: "source.ib",
				Destinations: []BackupDestination{
					{Path: "one.ibk", MaxBytes: 1},
					{Path: "two.ibk", MaxBytes: 2},
				},
			},
			want: "exactly one less",
		},
		{
			name: "NUL path",
			req: BackupRequest{
				SourceDatabase: "source.ib",
				Destinations:   []BackupDestination{{Path: "bad\x00.ibk"}},
			},
			want: "NUL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.req.build()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("build error = %v, want text containing %q", err, tt.want)
			}
		})
	}
}

func TestDumpRequestRequiresDatabaseAndDumpFile(t *testing.T) {
	tests := []struct {
		name string
		req  DumpRequest
		want string
	}{
		{
			name: "missing database",
			req:  DumpRequest{DumpFile: "dump.ibd"},
			want: "database",
		},
		{
			name: "missing dump file",
			req:  DumpRequest{Database: "db.ib"},
			want: "dump file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.req.build()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("build error = %v, want text containing %q", err, tt.want)
			}
		})
	}
}

func TestDecodeServiceInfoRejectsMalformedAndTruncatedPayloads(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "empty", raw: nil},
		{name: "missing length", raw: []byte{infoServerVersion}},
		{name: "length exceeds payload", raw: []byte{infoServerVersion, 4, 0, 'I'}},
		{name: "truncated marker", raw: []byte{infoTruncated}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeInfoString(tt.raw, infoServerVersion); err == nil {
				t.Fatal("decodeInfoString succeeded for malformed payload")
			}
		})
	}
}

func TestDecodeRunningRejectsMalformedResponses(t *testing.T) {
	tests := []struct {
		name    string
		raw     []byte
		want    bool
		wantErr bool
	}{
		{
			name: "running",
			raw:  []byte{infoRunning, 1, 0, 0, 0, infoEnd},
			want: true,
		},
		{
			name: "stopped",
			raw:  []byte{infoRunning, 0, 0, 0, 0, infoEnd},
		},
		{
			name: "data not ready",
			raw:  []byte{infoDataNotReady},
			want: true,
		},
		{
			name:    "short",
			raw:     []byte{infoRunning, 0, 0},
			wantErr: true,
		},
		{
			name:    "wrong item",
			raw:     []byte{infoUsers, 0, 0, 0, 0, infoEnd},
			wantErr: true,
		},
		{
			name:    "wrong terminator",
			raw:     []byte{infoRunning, 0, 0, 0, 0, infoFlagEnd},
			wantErr: true,
		},
		{
			name:    "invalid value",
			raw:     []byte{infoRunning, 2, 0, 0, 0, infoEnd},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeRunning(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeRunning(%x) succeeded, want malformed response error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeRunning(%x): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("decodeRunning(%x) = %t, want %t", tt.raw, got, tt.want)
			}
		})
	}
}

func TestDecodeOutputParsesNativeLengthDelimitedFrames(t *testing.T) {
	tests := []struct {
		name  string
		raw   []byte
		ended bool
		want  string
	}{
		{
			name:  "more output",
			raw:   []byte{infoToEOF, 5, 0, 'h', 'e', 'l', 'l', 'o', infoFlagEnd},
			want:  "hello",
			ended: false,
		},
		{
			name:  "final output",
			raw:   []byte{infoToEOF, 6, 0, 'f', 'i', 'n', 'a', 'l', '\n', infoEnd},
			want:  "final\n",
			ended: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ended, err := decodeOutput(tt.raw, false)
			if err != nil {
				t.Fatalf("decode output: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("output = %q, want %q", got, tt.want)
			}
			if ended != tt.ended {
				t.Fatalf("ended = %t, want %t", ended, tt.ended)
			}
		})
	}
}

func TestDecodeOutputRejectsMalformedAndTruncatedFrames(t *testing.T) {
	tests := []struct {
		name      string
		raw       []byte
		truncated bool
	}{
		{name: "empty", raw: nil},
		{name: "unframed end marker", raw: []byte{infoEnd}},
		{name: "missing length", raw: []byte{infoToEOF}},
		{name: "length exceeds payload", raw: []byte{infoToEOF, 4, 0, 'x', infoEnd}},
		{name: "missing marker", raw: []byte{infoToEOF, 1, 0, 'x'}},
		{name: "invalid marker", raw: []byte{infoToEOF, 1, 0, 'x', infoError}},
		{name: "truncated final frame", raw: []byte{infoToEOF, 1, 0, 'x', infoEnd}, truncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := decodeOutput(tt.raw, tt.truncated); err == nil {
				t.Fatal("decodeOutput succeeded for malformed or truncated frame")
			}
		})
	}
}

func TestDecodeServerDatabaseInfoPreservesStructuredEntries(t *testing.T) {
	raw := []byte{
		infoServerDatabase,
		serverInfoAttachments, 2, 0, 0, 0,
		serverInfoDatabases, 2, 0, 0, 0,
		serverInfoDatabaseName, 8, 0, '/', 't', 'm', 'p', '/', 'o', 'n', 'e',
		serverInfoDatabaseName, 8, 0, '/', 't', 'm', 'p', '/', 't', 'w', 'o',
		infoFlagEnd,
	}

	got, err := decodeServerDatabaseInfo(raw)
	if err != nil {
		t.Fatalf("decode server database info: %v", err)
	}
	if got.Attachments != 2 {
		t.Fatalf("attachments = %d, want 2", got.Attachments)
	}
	wantDatabases := []string{"/tmp/one", "/tmp/two"}
	if !equalStrings(got.Databases, wantDatabases) {
		t.Fatalf("databases = %v, want %v", got.Databases, wantDatabases)
	}
}

func TestDecodeAliasesUnwrapsLengthDelimitedPayload(t *testing.T) {
	name := "IBGO_ALIAS"
	database := "/tmp/database.ib"
	payload := []byte{aliasName, byte(len(name)), 0}
	payload = append(payload, name...)
	payload = append(payload, aliasPath, byte(len(database)), 0)
	payload = append(payload, database...)
	raw := []byte{infoAliases, byte(len(payload)), byte(len(payload) >> 8)}
	raw = append(raw, payload...)
	raw = append(raw, infoEnd)

	got, err := decodeAliases(raw)
	if err != nil {
		t.Fatalf("decode aliases: %v", err)
	}
	if got[name] != database {
		t.Fatalf("aliases[%q] = %q, want %q", name, got[name], database)
	}
}

func TestDecodeUsersUnwrapsPayloadAndReadsFourByteIDs(t *testing.T) {
	name := "IBGO_USER"
	payload := []byte{securityUserName, byte(len(name)), 0}
	payload = append(payload, name...)
	payload = append(payload, securityUserID, 0x04, 0x03, 0x02, 0x01)
	payload = append(payload, securityGroupID, 0x08, 0x07, 0x06, 0x05)
	raw := []byte{infoUsers, byte(len(payload)), byte(len(payload) >> 8)}
	raw = append(raw, payload...)
	raw = append(raw, infoEnd)

	got, err := decodeUsers(raw)
	if err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(got) != 1 || got[0].Name != name || got[0].UserID != 0x01020304 || got[0].GroupID != 0x05060708 {
		t.Fatalf("users = %+v, want one user with full-width IDs", got)
	}
}

func TestConfigBuildsServiceManagerTargetLikeRootAttachment(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "local default",
			cfg:  Config{User: "SYSDBA", Password: "masterkey"},
			want: "service_mgr",
		},
		{
			name: "remote",
			cfg:  Config{Host: "localhost", User: "SYSDBA", Password: "masterkey"},
			want: "localhost:service_mgr",
		},
		{
			name: "tls",
			cfg: Config{
				Host:     "localhost",
				User:     "SYSDBA",
				Password: "masterkey",
				TLS:      TLSConfig{Enabled: true, ServerPublicFile: "/tmp/public.pem"},
			},
			want: "localhost?ssl=true?serverPublicFile=/tmp/public.pem??:service_mgr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildServiceTarget(tt.cfg)
			if err != nil {
				t.Fatalf("build service target: %v", err)
			}
			if got != tt.want {
				t.Fatalf("service target = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildServiceSPBUsesCurrentVersionTag(t *testing.T) {
	got, err := buildServiceSPB(Config{User: "SYSDBA", Password: "masterkey"})
	if err != nil {
		t.Fatalf("build service SPB: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("service SPB = %v, want version header", got)
	}
	if got[0] != serviceSPBCurrentVersion || got[1] != serviceSPBCurrentVersion {
		t.Fatalf("service SPB version header = %v, want [%d %d]", got[:2], serviceSPBCurrentVersion, serviceSPBCurrentVersion)
	}
}

func TestPropertyFlagsMatchOfficialServiceValues(t *testing.T) {
	if propertyActivate != 0x0100 {
		t.Fatalf("activate property = %#x, want %#x", propertyActivate, 0x0100)
	}
	if propertyOnline != 0x0200 {
		t.Fatalf("online property = %#x, want %#x", propertyOnline, 0x0200)
	}
}

func TestUnsupportedShutdownAndOnlineModesDoNotEncodeUnknownCodes(t *testing.T) {
	for _, mode := range []ShutdownMode{ShutdownNormal, ShutdownMulti, ShutdownSingle, ShutdownFull} {
		_, err := (ShutdownRequest{
			Database: "db.ib",
			Mode:     mode,
			Method:   ShutdownForce,
		}).build()
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("shutdown mode %d error = %v, want ErrUnsupported", mode, err)
		}

		_, err = (OnlineRequest{Database: "db.ib", Mode: mode}).build()
		if mode == ShutdownFull {
			if err == nil || !strings.Contains(err.Error(), "invalid online mode") {
				t.Errorf("online mode %d error = %v, want invalid online mode", mode, err)
			}
		} else if !errors.Is(err, ErrUnsupported) {
			t.Errorf("online mode %d error = %v, want ErrUnsupported", mode, err)
		}
	}
}

func TestLegacyPropertyModesUseOfficialStandaloneOptions(t *testing.T) {
	shutdown, err := (ShutdownRequest{
		Database:       "db.ib",
		Mode:           ShutdownLegacy,
		Method:         ShutdownForce,
		TimeoutSeconds: 5,
	}).build()
	if err != nil {
		t.Fatalf("build legacy shutdown request: %v", err)
	}
	wantShutdown := []byte{8, 106, 5, 0, 'd', 'b', '.', 'i', 'b', 7, 5, 0, 0, 0}
	if !bytes.Equal(shutdown, wantShutdown) {
		t.Fatalf("legacy shutdown request = %v, want %v", shutdown, wantShutdown)
	}

	online, err := (OnlineRequest{Database: "db.ib", Mode: ShutdownLegacy}).build()
	if err != nil {
		t.Fatalf("build legacy online request: %v", err)
	}
	wantOnline := []byte{8, 106, 5, 0, 'd', 'b', '.', 'i', 'b', 108, 0, 2, 0, 0}
	if !bytes.Equal(online, wantOnline) {
		t.Fatalf("legacy online request = %v, want %v", online, wantOnline)
	}

	activate, err := (ActivateShadowRequest{Database: "db.ib"}).build()
	if err != nil {
		t.Fatalf("build activate-shadow request: %v", err)
	}
	wantActivate := []byte{8, 106, 5, 0, 'd', 'b', '.', 'i', 'b', 108, 0, 1, 0, 0}
	if !bytes.Equal(activate, wantActivate) {
		t.Fatalf("activate-shadow request = %v, want %v", activate, wantActivate)
	}
}

func TestStartAllowsOnlyOneActiveJob(t *testing.T) {
	backend := newFakeBackend()
	manager := newTestManager(backend)

	first, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("start first job: %v", err)
	}
	if _, err := manager.Start(context.Background(), LogRequest{}); !errors.Is(err, ErrJobActive) {
		t.Fatalf("second start error = %v, want ErrJobActive", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first job: %v", err)
	}
	if _, err := manager.Start(context.Background(), LogRequest{}); err != nil {
		t.Fatalf("start after first completion: %v", err)
	}
}

func TestJobReadAndWaitUseBoundedChunksWithoutAccumulatingOutput(t *testing.T) {
	backend := newFakeBackend()
	backend.chunks = [][]byte{[]byte("one\n"), []byte("two\n")}
	manager := newTestManager(backend)

	job, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}

	var got bytes.Buffer
	if _, err := io.Copy(&got, job); err != nil {
		t.Fatalf("read job output: %v", err)
	}
	if err := job.Wait(context.Background()); err != nil {
		t.Fatalf("wait job: %v", err)
	}
	if got.String() != "one\ntwo\n" {
		t.Fatalf("job output = %q, want %q", got.String(), "one\ntwo\n")
	}
	if backend.maxChunk > maxServiceChunk {
		t.Fatalf("native query chunk = %d, exceeds bound %d", backend.maxChunk, maxServiceChunk)
	}
}

func TestJobWaitContextDoesNotClaimNativeCancellation(t *testing.T) {
	backend := newFakeBackend()
	backend.blockUntilRelease = true
	manager := newTestManager(backend)

	job, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := job.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want context deadline", err)
	}
	if backend.cancelRequested {
		t.Fatal("context cancellation was forwarded as native cancellation")
	}
	close(backend.release)
	if err := job.Wait(context.Background()); err != nil {
		t.Fatalf("final wait: %v", err)
	}
}

func TestJobQueryFailureQuarantinesManagerUntilClose(t *testing.T) {
	nativeErr := errors.New("native output query failed")
	manager := newTestManager(&errorBackend{item: infoToEOF, err: nativeErr})

	job, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	if err := job.Wait(context.Background()); err == nil {
		t.Fatal("wait succeeded after native query failure")
	}
	if job.CompletionKnown() {
		t.Fatal("query failure was reported with known completion")
	}
	if _, err := manager.Start(context.Background(), LogRequest{}); !errors.Is(err, ErrCompletionUnknown) {
		t.Fatalf("start after uncertain completion = %v, want ErrCompletionUnknown", err)
	}
	if err := manager.Close(); err == nil {
		t.Fatal("close succeeded without reporting the quarantined operation error")
	}
	if _, err := manager.Start(context.Background(), LogRequest{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after manager close = %v, want ErrClosed", err)
	}
}

func TestJobDecodeFailureQuarantinesManagerUntilClose(t *testing.T) {
	manager := newTestManager(&errorBackend{
		item:     infoToEOF,
		response: []byte{infoToEOF},
	})

	job, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	if err := job.Wait(context.Background()); err == nil {
		t.Fatal("wait succeeded after malformed native output")
	}
	if job.CompletionKnown() {
		t.Fatal("decode failure was reported with known completion")
	}
	if _, err := manager.Start(context.Background(), LogRequest{}); !errors.Is(err, ErrCompletionUnknown) {
		t.Fatalf("start after uncertain decode = %v, want ErrCompletionUnknown", err)
	}
	if err := manager.Close(); err == nil {
		t.Fatal("close succeeded without reporting the quarantined decode failure")
	}
}

func TestJobHandlesPendingAndTruncatedContinuations(t *testing.T) {
	backend := newContinuationBackend()
	manager := newTestManager(backend)

	job, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	output, err := io.ReadAll(job)
	if err != nil {
		t.Fatalf("read job output: %v", err)
	}
	if err := job.Wait(context.Background()); err != nil {
		t.Fatalf("wait job: %v", err)
	}
	if got, want := string(output), "first\nsecond\n"; got != want {
		t.Fatalf("job output = %q, want %q", got, want)
	}
	if !job.CompletionKnown() {
		t.Fatal("pending/continuation sequence did not prove completion")
	}

	next, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("manager reuse after continuation sequence: %v", err)
	}
	if err := next.Wait(context.Background()); err != nil {
		t.Fatalf("wait reused manager job: %v", err)
	}
}

func TestSynchronousOperationKeepsManagerBusyAfterWaitCancellation(t *testing.T) {
	backend := newRunningBackend()
	manager := newTestManager(backend)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	operationDone := make(chan error, 1)
	go func() {
		operationDone <- manager.SetPageBuffers(ctx, PageBuffersRequest{Database: "db.ib", Buffers: 1})
	}()
	var operationErr error
	select {
	case operationErr = <-operationDone:
	case <-time.After(time.Second):
		close(backend.release)
		<-operationDone
		t.Fatal("synchronous operation ignored context cancellation indefinitely")
	}
	if !errors.Is(operationErr, context.DeadlineExceeded) {
		t.Fatalf("synchronous operation error = %v, want context deadline", operationErr)
	}
	if _, err := manager.Start(context.Background(), LogRequest{}); !errors.Is(err, ErrJobActive) {
		t.Fatalf("start during unfinished synchronous operation = %v, want ErrJobActive", err)
	}

	close(backend.release)
	select {
	case <-backend.finished:
	case <-time.After(time.Second):
		t.Fatal("unfinished synchronous operation did not drain after release")
	}
	job, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("start after synchronous operation: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Fatalf("close follow-up job: %v", err)
	}
}

func TestJobWaitReleasesManagerBeforeReturning(t *testing.T) {
	manager := newTestManager(newFakeBackend())
	job, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("start job: %v", err)
	}
	if err := job.Wait(context.Background()); err != nil {
		t.Fatalf("wait job: %v", err)
	}
	if !job.CompletionKnown() {
		t.Fatal("successful wait did not prove completion")
	}

	next, err := manager.Start(context.Background(), LogRequest{})
	if err != nil {
		t.Fatalf("start immediately after wait: %v", err)
	}
	if err := next.Close(); err != nil {
		t.Fatalf("close follow-up job: %v", err)
	}
}

func TestInformationQueryChecksContextAfterNativeResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	backend := &cancelingBackend{cancel: cancel}
	manager := newTestManager(backend)

	if _, err := manager.ServiceManagerVersion(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("service manager version error = %v, want context cancellation", err)
	}
}

func TestUsersAndAliasesQuarantineManagerAfterInformationFailure(t *testing.T) {
	tests := []struct {
		name string
		item byte
		run  func(*Manager) error
	}{
		{
			name: "users",
			item: infoUsers,
			run: func(manager *Manager) error {
				_, err := manager.Users(context.Background(), "")
				return err
			},
		},
		{
			name: "aliases",
			item: infoAliases,
			run: func(manager *Manager) error {
				_, err := manager.Aliases(context.Background())
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestManager(&errorBackend{item: tt.item, err: errors.New("information query failed")})
			if err := tt.run(manager); err == nil {
				t.Fatal("information query succeeded after native failure")
			}
			if _, err := manager.Start(context.Background(), LogRequest{}); !errors.Is(err, ErrCompletionUnknown) {
				t.Fatalf("start after uncertain information query = %v, want ErrCompletionUnknown", err)
			}
			if err := manager.Close(); err == nil {
				t.Fatal("close succeeded without reporting the quarantined information query")
			}
		})
	}
}

func TestUsersAndAliasesWaitForNativeActionBeforeInformationQuery(t *testing.T) {
	t.Run("users", func(t *testing.T) {
		backend := newInformationActionBackend(infoUsers)
		manager := newTestManager(backend)
		users, err := manager.Users(context.Background(), "")
		if err != nil {
			t.Fatalf("list users: %v", err)
		}
		want := User{
			Name:       "IBGO_USER",
			FirstName:  "First",
			MiddleName: "Middle",
			LastName:   "Services",
			UserID:     42,
			GroupID:    7,
		}
		if len(users) != 1 || users[0] != want {
			t.Fatalf("users = %+v, want [%+v]", users, want)
		}
		if !bytes.Equal(backend.queryItems, []byte{infoRunning, infoRunning, infoUsers}) {
			t.Fatalf("query order = %v, want %v", backend.queryItems, []byte{infoRunning, infoRunning, infoUsers})
		}
	})

	t.Run("aliases", func(t *testing.T) {
		backend := newInformationActionBackend(infoAliases)
		manager := newTestManager(backend)
		aliases, err := manager.Aliases(context.Background())
		if err != nil {
			t.Fatalf("list aliases: %v", err)
		}
		if len(aliases) != 1 || aliases["IBGO_ALIAS"] != "/tmp/database.ib" {
			t.Fatalf("aliases = %#v, want IBGO_ALIAS=/tmp/database.ib", aliases)
		}
		if !bytes.Equal(backend.queryItems, []byte{infoRunning, infoRunning, infoAliases}) {
			t.Fatalf("query order = %v, want %v", backend.queryItems, []byte{infoRunning, infoRunning, infoAliases})
		}
	})
}

func TestQueuedInformationCallHonorsContextCancellation(t *testing.T) {
	backend := newRunningBackend()
	manager := newTestManager(backend)
	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.Users(context.Background(), "")
		firstDone <- err
	}()

	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("first information action did not begin polling")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	secondDone := make(chan error, 1)
	go func() {
		_, err := manager.Aliases(ctx)
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued information call error = %v, want context deadline", err)
		}
	case <-time.After(250 * time.Millisecond):
		close(backend.release)
		if err := <-firstDone; err != nil {
			t.Fatalf("first information call after release: %v", err)
		}
		err := <-secondDone
		t.Fatalf("queued information call waited for native action: %v", err)
	}

	close(backend.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first information call after cancellation: %v", err)
	}
}

func TestQueuedStartHonorsContextCancellation(t *testing.T) {
	backend := newRunningBackend()
	manager := newTestManager(backend)
	firstDone := make(chan error, 1)
	go func() {
		_, err := manager.Users(context.Background(), "")
		firstDone <- err
	}()

	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("first information action did not begin polling")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	secondDone := make(chan error, 1)
	go func() {
		_, err := manager.Start(ctx, LogRequest{})
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued start error = %v, want context deadline", err)
		}
	case <-time.After(250 * time.Millisecond):
		close(backend.release)
		if err := <-firstDone; err != nil {
			t.Fatalf("first information call after release: %v", err)
		}
		err := <-secondDone
		t.Fatalf("queued start waited for native action: %v", err)
	}

	close(backend.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first information call after start cancellation: %v", err)
	}
}

func TestUsersAndAliasesStatusWaitErrorQuarantinesManager(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Manager) error
	}{
		{
			name: "users",
			run: func(manager *Manager) error {
				_, err := manager.Users(context.Background(), "")
				return err
			},
		},
		{
			name: "aliases",
			run: func(manager *Manager) error {
				_, err := manager.Aliases(context.Background())
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestManager(&errorBackend{runningErr: errors.New("status query failed")})
			if err := tt.run(manager); err == nil {
				t.Fatal("status wait succeeded after native failure")
			}
			if _, err := manager.Start(context.Background(), LogRequest{}); !errors.Is(err, ErrCompletionUnknown) {
				t.Fatalf("start after uncertain status query = %v, want ErrCompletionUnknown", err)
			}
			if err := manager.Close(); err == nil {
				t.Fatal("close succeeded without reporting the quarantined status query")
			}
		})
	}
}

func TestUsersAndAliasesStatusWaitCancellationQuarantinesManager(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Manager, context.Context) error
	}{
		{
			name: "users",
			run: func(manager *Manager, ctx context.Context) error {
				_, err := manager.Users(ctx, "")
				return err
			},
		},
		{
			name: "aliases",
			run: func(manager *Manager, ctx context.Context) error {
				_, err := manager.Aliases(ctx)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newTestManager(newRunningBackend())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			if err := tt.run(manager, ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("status wait error = %v, want context deadline", err)
			}
			if _, err := manager.Start(context.Background(), LogRequest{}); !errors.Is(err, ErrCompletionUnknown) {
				t.Fatalf("start after canceled status wait = %v, want ErrCompletionUnknown", err)
			}
			if err := manager.Close(); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("close after canceled status wait = %v, want context deadline", err)
			}
		})
	}
}

func TestCloseWaitsForInProgressInformationQuery(t *testing.T) {
	backend := newBlockingInformationBackend()
	manager := newTestManager(backend)
	usersDone := make(chan error, 1)
	go func() {
		_, err := manager.Users(context.Background(), "")
		usersDone <- err
	}()

	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("information query did not begin")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("close returned before information query completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(backend.release)
	if err := <-usersDone; err != nil {
		t.Fatalf("users after query release: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close after query release: %v", err)
	}
}

func TestManagerCloseRetainsBackendForDetachRetry(t *testing.T) {
	backend := &closeSequenceBackend{errors: []error{errors.New("detach failed"), nil}}
	manager := newTestManager(backend)

	if err := manager.Close(); err == nil {
		t.Fatal("first close succeeded after detach failure")
	}
	if _, err := manager.Start(context.Background(), LogRequest{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after failed close = %v, want ErrClosed", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	if backend.calls != 2 {
		t.Fatalf("detach calls = %d, want 2", backend.calls)
	}
	if !manager.closed || manager.backend != nil {
		t.Fatalf("manager state after successful retry = closed:%t backend:%T, want closed with no backend", manager.closed, manager.backend)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("idempotent close: %v", err)
	}
	if backend.calls != 2 {
		t.Fatalf("detach calls after idempotent close = %d, want 2", backend.calls)
	}
}

func TestManagerCloseSerializesConcurrentDetachAttempts(t *testing.T) {
	backend := &blockingCloseBackend{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager := newTestManager(backend)
	firstDone := make(chan error, 1)
	go func() { firstDone <- manager.Close() }()

	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("first close did not start detaching")
	}

	secondDone := make(chan error, 1)
	go func() { secondDone <- manager.Close() }()
	select {
	case err := <-secondDone:
		t.Fatalf("concurrent close returned before the first detach completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(backend.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second close: %v", err)
	}
	backend.mu.Lock()
	calls := backend.calls
	backend.mu.Unlock()
	if calls != 1 {
		t.Fatalf("detach calls = %d, want 1", calls)
	}
}

func TestManagerCloseWaiterReturnsItsAttemptResultAfterRetry(t *testing.T) {
	firstErr := errors.New("first detach failed")
	backend := &closeSequenceBackend{
		errors:  []error{firstErr, nil},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	manager := newTestManager(backend)

	firstDone := make(chan error, 1)
	go func() { firstDone <- manager.Close() }()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("first close did not start detaching")
	}

	waiterCaptured := make(chan struct{})
	waiterPaused := make(chan struct{})
	waiterRelease := make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- manager.closeWithWaitHooks(closeWaitHooks{
			afterCapture: func() { close(waiterCaptured) },
			beforeResult: func() {
				close(waiterPaused)
				<-waiterRelease
			},
		})
	}()
	select {
	case <-waiterCaptured:
	case <-time.After(time.Second):
		t.Fatal("close waiter did not capture the first attempt")
	}

	close(backend.release)
	firstResult := <-firstDone
	if firstResult == nil || !strings.Contains(firstResult.Error(), firstErr.Error()) {
		t.Fatalf("first close = %v, want %v", firstResult, firstErr)
	}
	select {
	case <-waiterPaused:
	case <-time.After(time.Second):
		t.Fatal("close waiter did not pause after the first attempt completed")
	}

	retryDone := make(chan error, 1)
	go func() { retryDone <- manager.Close() }()
	if err := <-retryDone; err != nil {
		t.Fatalf("successful close retry: %v", err)
	}
	close(waiterRelease)
	waiterResult := <-waiterDone
	if waiterResult != firstResult {
		t.Fatalf("late close waiter = %v, want exact captured first-attempt error %v", waiterResult, firstResult)
	}
	if backend.calls != 2 {
		t.Fatalf("detach calls = %d, want 2", backend.calls)
	}
}

func TestStartChecksContextAfterRequestBuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	backend := newFakeBackend()
	manager := newTestManager(backend)
	result := make(chan error, 1)

	go func() {
		_, err := manager.Start(ctx, blockingRequest{
			started: requestStarted,
			release: releaseRequest,
		})
		result <- err
	}()
	<-requestStarted
	cancel()
	close(releaseRequest)

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("start error = %v, want context cancellation", err)
	}
	backend.mu.Lock()
	startCount := backend.startCount
	backend.mu.Unlock()
	if startCount != 0 {
		t.Fatalf("native start count = %d, want zero after cancellation", startCount)
	}
}

func TestServiceErrorsRedactOverlappingCredentials(t *testing.T) {
	err := newServiceError("attach service", errors.New("password=supersecret; token=secret"),
		"supersecret", "secret")
	message := err.Error()
	if strings.Contains(message, "supersecret") || strings.Contains(message, "secret") {
		t.Fatalf("service error leaked credentials: %q", message)
	}
}

func TestTargetRedactsOverlappingPassphrases(t *testing.T) {
	manager := &Manager{
		target:  "server?clientPassPhrase=supersecret?clientPassPhraseFile=/tmp/secret??:service_mgr",
		secrets: []string{"supersecret", "secret"},
	}

	target := manager.Target()
	if strings.Contains(target, "supersecret") || strings.Contains(target, "secret") {
		t.Fatalf("target leaked a passphrase: %q", target)
	}
	if !strings.Contains(target, "[REDACTED]") {
		t.Fatalf("target = %q, want a redaction marker", target)
	}
}

// fakeBackend is deliberately small: it exercises Manager and Job lifecycle
// without replacing the native ABI in production or claiming live coverage.
type fakeBackend struct {
	mu                sync.Mutex
	chunks            [][]byte
	blockUntilRelease bool
	release           chan struct{}
	cancelRequested   bool
	maxChunk          int
	startCount        int
}

type errorBackend struct {
	item       byte
	err        error
	runningErr error
	response   []byte
}

type closeSequenceBackend struct {
	errors  []error
	calls   int
	started chan struct{}
	release chan struct{}
}

type blockingCloseBackend struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (b *closeSequenceBackend) start(_ []byte) error { return nil }

func (b *closeSequenceBackend) query(_ byte, _ int) ([]byte, bool, error) {
	return frameServiceOutput(nil, infoEnd), false, nil
}

func (b *closeSequenceBackend) close() error {
	b.calls++
	if b.calls == 1 && b.started != nil {
		close(b.started)
		<-b.release
	}
	if len(b.errors) == 0 {
		return nil
	}
	err := b.errors[0]
	b.errors = b.errors[1:]
	return err
}

func (b *blockingCloseBackend) start(_ []byte) error { return nil }

func (b *blockingCloseBackend) query(_ byte, _ int) ([]byte, bool, error) {
	return frameServiceOutput(nil, infoEnd), false, nil
}

func (b *blockingCloseBackend) close() error {
	b.mu.Lock()
	b.calls++
	call := b.calls
	b.mu.Unlock()
	if call == 1 {
		close(b.started)
		<-b.release
	}
	return nil
}

type informationActionBackend struct {
	mu          sync.Mutex
	item        byte
	queryItems  []byte
	runningCall int
}

func newInformationActionBackend(item byte) *informationActionBackend {
	return &informationActionBackend{item: item}
}

func (b *informationActionBackend) start(_ []byte) error { return nil }

func (b *informationActionBackend) query(item byte, _ int) ([]byte, bool, error) {
	b.mu.Lock()
	b.queryItems = append(b.queryItems, item)
	if item == infoRunning {
		b.runningCall++
		running := b.runningCall == 1
		b.mu.Unlock()
		return runningInformationResponse(running), false, nil
	}
	b.mu.Unlock()
	if item == b.item {
		if item == infoUsers {
			return testUserInformationResponse(), false, nil
		}
		return testAliasInformationResponse(), false, nil
	}
	return []byte{infoEnd}, false, nil
}

func (b *informationActionBackend) close() error { return nil }

func testUserInformationResponse() []byte {
	name := "IBGO_USER"
	payload := []byte{securityUserName, byte(len(name)), 0}
	payload = append(payload, name...)
	payload = append(payload, securityPassword, 6, 0)
	payload = append(payload, "hidden"...)
	payload = append(payload, securityFirstName, 5, 0)
	payload = append(payload, "First"...)
	payload = append(payload, securityMiddleName, 6, 0)
	payload = append(payload, "Middle"...)
	payload = append(payload, securityLastName, 8, 0)
	payload = append(payload, "Services"...)
	payload = append(payload, securityUserID, 42, 0, 0, 0)
	payload = append(payload, securityGroupID, 7, 0, 0, 0)
	raw := []byte{infoUsers, byte(len(payload)), byte(len(payload) >> 8)}
	raw = append(raw, payload...)
	return append(raw, infoEnd)
}

func testAliasInformationResponse() []byte {
	name := "IBGO_ALIAS"
	database := "/tmp/database.ib"
	payload := []byte{aliasName, byte(len(name)), 0}
	payload = append(payload, name...)
	payload = append(payload, aliasPath, byte(len(database)), 0)
	payload = append(payload, database...)
	raw := []byte{infoAliases, byte(len(payload)), byte(len(payload) >> 8)}
	raw = append(raw, payload...)
	return append(raw, infoEnd)
}

func runningInformationResponse(running bool) []byte {
	value := byte(0)
	if running {
		value = 1
	}
	return []byte{infoRunning, value, 0, 0, 0, infoEnd}
}

type continuationResponse struct {
	raw       []byte
	truncated bool
}

type continuationBackend struct {
	mu           sync.Mutex
	responses    []continuationResponse
	outputIndex  int
	runningIndex int
}

func newContinuationBackend() *continuationBackend {
	return &continuationBackend{
		responses: []continuationResponse{
			{raw: []byte{infoDataNotReady}},
			{raw: frameServiceOutput([]byte("first\n"), infoTruncated), truncated: true},
			{raw: frameServiceOutput([]byte("second\n"), infoEnd)},
		},
	}
}

func (b *continuationBackend) start(_ []byte) error {
	b.mu.Lock()
	b.runningIndex = 0
	b.mu.Unlock()
	return nil
}

func (b *continuationBackend) query(item byte, _ int) ([]byte, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch item {
	case infoToEOF:
		if b.outputIndex >= len(b.responses) {
			return frameServiceOutput(nil, infoEnd), false, nil
		}
		response := b.responses[b.outputIndex]
		b.outputIndex++
		return response.raw, response.truncated, nil
	case infoRunning:
		running := b.runningIndex < len(b.responses)-1
		b.runningIndex++
		return runningInformationResponse(running), false, nil
	default:
		return []byte{infoEnd}, false, nil
	}
}

func (b *continuationBackend) close() error { return nil }

func (b *errorBackend) start(_ []byte) error { return nil }

func (b *errorBackend) query(item byte, _ int) ([]byte, bool, error) {
	if item == infoRunning {
		if b.runningErr != nil {
			return nil, false, b.runningErr
		}
		return runningInformationResponse(false), false, nil
	}
	if item == b.item {
		if b.err != nil {
			return nil, false, b.err
		}
		return b.response, false, nil
	}
	return []byte{infoEnd}, false, nil
}

func (b *errorBackend) close() error { return nil }

func newTestManager(backend nativeBackend) *Manager {
	return &Manager{backend: backend, target: "test"}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{release: make(chan struct{})}
}

type runningBackend struct {
	entered   chan struct{}
	release   chan struct{}
	finished  chan struct{}
	enterOnce sync.Once
	once      sync.Once
}

type blockingInformationBackend struct {
	entered chan struct{}
	release chan struct{}
	closed  chan struct{}
}

func newBlockingInformationBackend() *blockingInformationBackend {
	return &blockingInformationBackend{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (b *blockingInformationBackend) start(_ []byte) error { return nil }

func (b *blockingInformationBackend) query(item byte, _ int) ([]byte, bool, error) {
	if item == infoRunning {
		return runningInformationResponse(false), false, nil
	}
	if item == infoUsers {
		close(b.entered)
		<-b.release
		return testUserInformationResponse(), false, nil
	}
	return []byte{infoEnd}, false, nil
}

func (b *blockingInformationBackend) close() error {
	close(b.closed)
	return nil
}

func newRunningBackend() *runningBackend {
	return &runningBackend{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
}

func (b *runningBackend) start(_ []byte) error { return nil }

func (b *runningBackend) query(item byte, _ int) ([]byte, bool, error) {
	if item == infoToEOF {
		return frameServiceOutput(nil, infoEnd), false, nil
	}
	if item == infoRunning {
		b.enterOnce.Do(func() { close(b.entered) })
		select {
		case <-b.release:
			b.once.Do(func() { close(b.finished) })
			return runningInformationResponse(false), false, nil
		default:
			return runningInformationResponse(true), false, nil
		}
	}
	if item == infoUsers {
		return testUserInformationResponse(), false, nil
	}
	if item == infoAliases {
		return testAliasInformationResponse(), false, nil
	}
	return []byte{infoEnd}, false, nil
}

func (b *runningBackend) close() error { return nil }

type cancelingBackend struct {
	cancel context.CancelFunc
}

func (b *cancelingBackend) start(_ []byte) error { return nil }

func (b *cancelingBackend) query(item byte, _ int) ([]byte, bool, error) {
	if item == infoVersion {
		b.cancel()
		return []byte{infoVersion, 2, 0}, false, nil
	}
	return []byte{infoEnd}, false, nil
}

func (b *cancelingBackend) close() error { return nil }

type blockingRequest struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (r blockingRequest) build() ([]byte, error) {
	close(r.started)
	<-r.release
	return []byte{actionGetLog}, nil
}

func (b *fakeBackend) start(_ []byte) error {
	b.mu.Lock()
	b.startCount++
	b.mu.Unlock()
	return nil
}

func (b *fakeBackend) query(item byte, capacity int) ([]byte, bool, error) {
	b.mu.Lock()
	if capacity > b.maxChunk {
		b.maxChunk = capacity
	}
	block := b.blockUntilRelease
	b.mu.Unlock()
	if block {
		<-b.release
	}
	if item == infoRunning {
		return runningInformationResponse(false), false, nil
	}
	if len(b.chunks) == 0 {
		return frameServiceOutput(nil, infoEnd), false, nil
	}
	chunk := b.chunks[0]
	b.chunks = b.chunks[1:]
	marker := infoFlagEnd
	if len(b.chunks) == 0 {
		marker = infoEnd
	}
	return frameServiceOutput(chunk, marker), false, nil
}

func (b *fakeBackend) close() error { return nil }

func frameServiceOutput(payload []byte, marker byte) []byte {
	frame := []byte{infoToEOF, byte(len(payload)), byte(len(payload) >> 8)}
	frame = append(frame, payload...)
	return append(frame, marker)
}
