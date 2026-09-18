package services

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"
)

const (
	maxServicesFuzzString = 4 << 10
	maxServicesFuzzBytes  = 4 << 10
)

func FuzzServiceRequestBuilders(f *testing.F) {
	f.Add("database.ib", "backup.ibk", "IBGO_USER", "password", uint32(4096), int64(17), true)
	f.Add("", "", "", "", uint32(0), int64(0), false)
	f.Add("db\x00.ib", "dump.ibd", "user", "secret", uint32(math.MaxUint32), int64(-1), true)
	f.Add("db", "path", "user", "password", uint32(1), int64(math.MaxInt64), false)

	f.Fuzz(func(t *testing.T, database, path, user, password string, number uint32, transactionID int64, flag bool) {
		for _, value := range []string{database, path, user, password} {
			if len(value) > maxServicesFuzzString {
				t.Skip()
			}
		}

		recoverUntil := int64ToFuzzTime(transactionID)
		requests := []Request{
			BackupRequest{
				SourceDatabase: database,
				Destinations: []BackupDestination{
					{Path: path, MaxBytes: number},
					{Path: database},
				},
				Options: BackupOptions{
					IgnoreChecksums:  flag,
					IgnoreLimbo:      !flag,
					MetadataOnly:     flag,
					NoGarbageCollect: !flag,
					NonTransportable: flag,
					ConvertExternal:  !flag,
					Expand:           flag,
					ArchiveDatabase:  flag,
				},
				SEPPassword: password,
				EncryptName: user,
			},
			RestoreRequest{
				SourceFiles:  []string{path},
				Destinations: []RestoreDestination{{Path: database}},
				Options: RestoreOptions{
					Replace:              flag,
					DeactivateIndexes:    !flag,
					DoNotRestoreShadows:  flag,
					DoNotEnforceValidity: !flag,
					CommitAfterEachTable: flag,
					UseAllPageSpace:      !flag,
					MetadataOnly:         flag,
					ReadOnly:             !flag,
					PageSize:             number,
					CacheBuffers:         number,
					SEPPassword:          password,
					DecryptPassword:      user,
				},
			},
			DatabaseStatisticsRequest{
				Database: database,
				Options: DatabaseStatisticsOptions{
					OnlyLogPages:          flag,
					OnlyHeaderPages:       !flag,
					NoUserDataPages:       flag,
					NoUserIndexPages:      !flag,
					IncludeSystemTables:   flag,
					IncludeRecordVersions: !flag,
				},
			},
			LogRequest{},
			DumpRequest{Database: database, DumpFile: path, Overwrite: flag},
			ArchiveBackupRequest{Database: database, Journals: flag},
			ArchiveRestoreRequest{BackupFile: path, Database: database, RecoverUntil: &recoverUntil},
			TablespaceBackupRequest{Database: database, Tablespace: user, BackupFile: path},
			TablespaceRestoreRequest{
				SourceFiles:  []string{path},
				Destinations: []RestoreDestination{{Path: database}},
				Tablespace:   user,
				BackupFile:   path,
				Create:       flag,
				Replace:      !flag,
			},
			ValidationRequest{
				Database:               database,
				ReadOnly:               flag,
				IgnoreChecksums:        !flag,
				KillUnavailableShadows: flag,
				MendDatabase:           !flag,
				SkipDatabaseValidation: flag,
				SkipRecordFragments:    !flag,
			},
			SweepRequest{Database: database},
			LimboTransactionsRequest{Database: database},
			ResolveLimboRequest{Database: database, TransactionID: transactionID, Commit: flag},
			PageBuffersRequest{Database: database, Buffers: number},
			SweepIntervalRequest{Database: database, Interval: number},
			ReserveSpaceRequest{Database: database, Reserve: flag},
			WriteModeRequest{Database: database, Mode: chooseWriteMode(flag)},
			AccessModeRequest{Database: database, Mode: chooseAccessMode(flag)},
			SQLDialectRequest{Database: database, Dialect: int(transactionID)},
			ActivateShadowRequest{Database: database},
			ShutdownRequest{
				Database:       database,
				Mode:           ShutdownLegacy,
				Method:         chooseShutdownMethod(number),
				TimeoutSeconds: number,
			},
			OnlineRequest{Database: database, Mode: ShutdownLegacy},
			ListUsersRequest{Name: user},
			AddUserRequest{User: User{Name: user, Password: password, FirstName: database, LastName: path}},
			ModifyUserRequest{User: User{Name: user, Password: password, FirstName: database, LastName: path}},
			DeleteUserRequest{Name: user},
			AddAliasRequest{Alias: user, Database: database},
			DeleteAliasRequest{Alias: user},
			ListAliasesRequest{},
		}

		for index, request := range requests {
			encoded, err := request.build()
			if err != nil {
				continue
			}
			if len(encoded) == 0 {
				t.Fatalf("request %d returned an empty service parameter block", index)
			}
			if len(encoded) > 1<<20 {
				t.Fatalf("request %d returned an unexpectedly large parameter block: %d bytes", index, len(encoded))
			}
		}

		if spb, err := buildServiceSPB(Config{User: user, Password: password}); err == nil && len(spb) < 2 {
			t.Fatalf("service SPB has invalid length %d", len(spb))
		}
		if target, err := buildServiceTarget(Config{Host: database}); err == nil && target == "" {
			t.Fatal("successful service target is empty")
		}
	})
}

func FuzzServiceResponseFraming(f *testing.F) {
	f.Add([]byte("hello"), uint8(infoToEOF), uint8(0), false)
	f.Add([]byte(nil), uint8(infoRunning), uint8(1), true)
	f.Add([]byte{0, 1, infoEnd}, uint8(infoUsers), uint8(255), false)
	f.Add([]byte{0xff, 0x00, 0x7f}, uint8(infoAliases), uint8(4), false)

	f.Fuzz(func(t *testing.T, payload []byte, expected, positionSeed uint8, truncated bool) {
		if len(payload) > maxServicesFuzzBytes {
			t.Skip()
		}

		terminal := serviceFuzzFrame(infoToEOF, payload, infoEnd)
		decoded, ended, err := decodeOutput(terminal, false)
		if err != nil {
			t.Fatalf("decodeOutput(valid frame): %v", err)
		}
		if !ended || !bytes.Equal(decoded, payload) {
			t.Fatalf("decodeOutput(valid frame) = (%x, %t), want (%x, true)", decoded, ended, payload)
		}

		continuation := serviceFuzzFrame(infoToEOF, payload, infoFlagEnd)
		decoded, ended, err = decodeOutput(continuation, false)
		if err != nil {
			t.Fatalf("decodeOutput(continuation frame): %v", err)
		}
		if ended || !bytes.Equal(decoded, payload) {
			t.Fatalf("decodeOutput(continuation frame) = (%x, %t), want (%x, false)", decoded, ended, payload)
		}

		if truncated {
			if _, _, err := decodeOutput(terminal, true); !errors.Is(err, ErrOutputLimit) {
				t.Fatalf("decodeOutput(truncated terminal) error = %v, want ErrOutputLimit", err)
			}
		}

		// Every decoder below must reject malformed input with an error or return
		// a bounded value; arbitrary bytes must never cause a panic.
		_, _ = decodeRunning(payload)
		_, _ = decodeInfoString(payload, expected)
		_, _ = decodeInfoUint16(payload, expected)
		_, _ = decodeUsers(payload)
		_, _ = decodeAliases(payload)
		_, _ = decodeServerDatabaseInfo(payload)
		_, _ = unwrapServicePayload(payload, expected)
		_, _, _ = readInfoString(payload, int(positionSeed))
		_ = parseServiceNativeError(string(payload))
		_ = redactServiceSecrets(string(payload), string(payload))

		infoFrame := serviceFuzzFrame(expected, payload, infoEnd)
		if got, err := decodeInfoString(infoFrame, expected); err == nil && got != string(payload) {
			t.Fatalf("decodeInfoString(valid frame) = %q, want %q", got, payload)
		}
		integerFrame := []byte{expected, 0, 0}
		binary.LittleEndian.PutUint16(integerFrame[1:], uint16(len(payload)))
		if got, err := decodeInfoUint16(integerFrame, expected); err != nil || got != uint16(len(payload)) {
			t.Fatalf("decodeInfoUint16(valid frame) = (%d, %v), want (%d, nil)", got, err, len(payload))
		}
	})
}

func FuzzServiceStructuredResponseFraming(f *testing.F) {
	f.Add("IBGO_USER", "/tmp/database.ib", uint32(17), "main", "/tmp/main.ib")
	f.Add("", "", uint32(0), "", "")
	f.Add("user\x00", "database", uint32(math.MaxUint32), "alias", "path")

	f.Fuzz(func(t *testing.T, userName, database string, userID uint32, aliasValue, aliasPathValue string) {
		for _, value := range []string{userName, database, aliasValue, aliasPathValue} {
			if len(value) > maxServicesFuzzString || len(value) > math.MaxUint16 {
				t.Skip()
			}
		}

		userPayload := make([]byte, 0, len(userName)+16)
		userPayload = appendServiceFuzzString(userPayload, securityUserName, userName)
		userPayload = append(userPayload, securityUserID)
		var encodedID [4]byte
		binary.LittleEndian.PutUint32(encodedID[:], userID)
		userPayload = append(userPayload, encodedID[:]...)
		userPayload = append(userPayload, securityFirstName)
		userPayload = append(userPayload, 0, 0)

		rawUsers := serviceFuzzFrame(infoUsers, userPayload, infoEnd)
		users, err := decodeUsers(rawUsers)
		if err != nil {
			t.Fatalf("decodeUsers(valid response): %v", err)
		}
		if len(users) != 1 || users[0].Name != userName || users[0].UserID != userID {
			t.Fatalf("decodeUsers(valid response) = %#v, want user %q/%d", users, userName, userID)
		}

		aliasPayload := appendServiceFuzzString(nil, aliasName, aliasValue)
		aliasPayload = appendServiceFuzzString(aliasPayload, aliasPath, aliasPathValue)
		rawAliases := serviceFuzzFrame(infoAliases, aliasPayload, infoEnd)
		aliases, err := decodeAliases(rawAliases)
		if err != nil {
			t.Fatalf("decodeAliases(valid response): %v", err)
		}
		if aliasValue != "" && aliasPathValue != "" && aliases[aliasValue] != aliasPathValue {
			t.Fatalf("decodeAliases(valid response) = %#v, want %q -> %q", aliases, aliasValue, aliasPathValue)
		}
	})
}

func serviceFuzzFrame(code byte, payload []byte, marker byte) []byte {
	frame := make([]byte, 3, 4+len(payload))
	frame[0] = code
	binary.LittleEndian.PutUint16(frame[1:], uint16(len(payload)))
	frame = append(frame, payload...)
	return append(frame, marker)
}

func appendServiceFuzzString(buffer []byte, code byte, value string) []byte {
	buffer = append(buffer, code, byte(len(value)), byte(len(value)>>8))
	return append(buffer, value...)
}

func int64ToFuzzTime(value int64) time.Time {
	return time.Unix(value%4_000_000_000, 0).UTC()
}

func chooseWriteMode(flag bool) WriteMode {
	if flag {
		return WriteForced
	}
	return WriteBuffered
}

func chooseAccessMode(flag bool) AccessMode {
	if flag {
		return AccessReadOnly
	}
	return AccessReadWrite
}

func chooseShutdownMethod(value uint32) ShutdownMethod {
	switch value % 3 {
	case 0:
		return ShutdownForce
	case 1:
		return ShutdownDenyNewAttachments
	default:
		return ShutdownDenyNewTransactions
	}
}
