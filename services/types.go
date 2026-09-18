// Package services exposes the InterBase Services Manager through the native
// client library.  It intentionally does not expose a general-purpose service
// parameter block builder: callers select one of the typed requests below.
package services

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	interbase "interbase-go"
)

const (
	serviceSPBVersion           byte = 2
	serviceSPBCurrentVersion    byte = 2
	serviceSPBUserName          byte = 28
	serviceSPBPassword          byte = 29
	serviceSPBPasswordEncrypted byte = 30
	serviceSPBSystemEncryption  byte = 85
	serviceSPBCommandLine       byte = 105
	serviceSPBDatabaseName      byte = 106
	serviceSPBVerbose           byte = 107
	serviceSPBOptions           byte = 108

	actionBackup        byte = 1
	actionRestore       byte = 2
	actionRepair        byte = 3
	actionAddUser       byte = 4
	actionDeleteUser    byte = 5
	actionModifyUser    byte = 6
	actionDisplayUser   byte = 7
	actionProperties    byte = 8
	actionDatabaseStats byte = 11
	actionGetLog        byte = 12
	actionAddAlias      byte = 13
	actionDeleteAlias   byte = 14
	actionDisplayAlias  byte = 15
	actionDump          byte = 16

	infoServerDatabase   byte = 50
	infoVersion          byte = 54
	infoServerVersion    byte = 55
	infoImplementation   byte = 56
	infoCapabilities     byte = 57
	infoSecurityPath     byte = 58
	infoHomeDirectory    byte = 59
	infoLockDirectory    byte = 60
	infoMessageDirectory byte = 61
	infoLine             byte = 62
	infoToEOF            byte = 63
	infoTimeout          byte = 64
	infoLimbo            byte = 66
	infoRunning          byte = 67
	infoUsers            byte = 68
	infoAliases          byte = 69

	infoEnd          byte = 1
	infoTruncated    byte = 2
	infoError        byte = 3
	infoDataNotReady byte = 4
	infoFlagEnd      byte = 127

	serverInfoAttachments  byte = 5
	serverInfoDatabases    byte = 6
	serverInfoDatabaseName byte = 106

	securityUserID     byte = 5
	securityGroupID    byte = 6
	securityUserName   byte = 7
	securityPassword   byte = 8
	securityGroupName  byte = 9
	securityFirstName  byte = 10
	securityMiddleName byte = 11
	securityLastName   byte = 12

	aliasName byte = 20
	aliasPath byte = 21

	propertyPageBuffers      byte   = 5
	propertySweepInterval    byte   = 6
	propertyShutdownDatabase byte   = 7
	propertyDenyAttachments  byte   = 9
	propertyDenyTransactions byte   = 10
	propertyReserveSpace     byte   = 11
	propertyWriteMode        byte   = 12
	propertyAccessMode       byte   = 13
	propertySQLDialect       byte   = 14
	propertyActivate         uint32 = 0x0100
	propertyOnline           uint32 = 0x0200

	propertyReserveUseFull  byte = 35
	propertyReserve         byte = 36
	propertyWriteAsync      byte = 37
	propertyWriteSync       byte = 38
	propertyAccessReadOnly  byte = 39
	propertyAccessReadWrite byte = 40

	repairCommitTransaction   byte = 15
	repairRollbackTransaction byte = 34

	repairValidateDatabase byte = 0x01
	repairSweepDatabase    byte = 0x02
	repairMendDatabase     byte = 0x04
	repairListLimbo        byte = 0x08
	repairCheckDatabase    byte = 0x10
	repairIgnoreChecksum   byte = 0x20
	repairKillShadows      byte = 0x40
	repairFull             byte = 0x80

	backupFile             byte   = 5
	backupLength           byte   = 7
	backupEncryptName      byte   = 14
	backupIgnoreChecksums  uint32 = 0x01
	backupIgnoreLimbo      uint32 = 0x02
	backupMetadataOnly     uint32 = 0x04
	backupNoGarbage        uint32 = 0x08
	backupNonTransportable uint32 = 0x20
	backupConvert          uint32 = 0x40
	backupExpand           uint32 = 0x80
	backupArchiveDatabase  uint32 = 0x010000
	backupArchiveJournals  uint32 = 0x020000

	restoreBuffers           byte   = 9
	restorePageSize          byte   = 10
	restoreLength            byte   = 11
	restoreAccessMode        byte   = 12
	restoreDecryptPassword   byte   = 16
	restoreArchiveUntil      byte   = 23
	restoreMetadataOnly      uint32 = backupMetadataOnly
	restoreDeactivateIndexes uint32 = 0x0100
	restoreNoShadows         uint32 = 0x0200
	restoreNoValidity        uint32 = 0x0400
	restoreOneAtATime        uint32 = 0x0800
	restoreReplace           uint32 = 0x1000
	restoreCreate            uint32 = 0x2000
	restoreUseAllSpace       uint32 = 0x4000
	restoreArchiveRecover    uint32 = 0x040000
	restoreCreateTablespace  uint32 = 0x0100000
	restoreReplaceTablespace uint32 = 0x0200000

	dumpFile      byte   = backupFile
	dumpOverwrite byte   = 20
	dumpCreate    uint32 = 0x080000

	tablespaceInclude byte = 24
	tablespaceFile    byte = backupFile

	maxServiceParameterLength = math.MaxUint16
	defaultServiceQueryBuffer = 1024
	maxServiceQueryBuffer     = math.MaxUint16
	maxServiceChunk           = maxServiceQueryBuffer
)

// ErrClosed indicates that a manager or job can no longer be used.
var ErrClosed = errors.New("interbase services: manager is closed")

// ErrJobActive indicates that another asynchronous service operation owns the
// manager. The Services API does not support concurrent actions on one handle.
var ErrJobActive = errors.New("interbase services: another job is active")

// ErrCompletionUnknown indicates that a native action encountered an error
// before the client proved that it had completed. The manager remains
// quarantined until it is closed so a later action cannot overlap the unknown
// server-side operation.
var ErrCompletionUnknown = errors.New("interbase services: native operation completion is unknown; close the manager")

// ErrOutputLimit indicates that a single native information response exceeded
// the maximum buffer accepted by the Services API.
var ErrOutputLimit = errors.New("interbase services: information response exceeds native buffer limit")

// ErrMalformedResponse indicates that the native service returned an invalid
// or incomplete information buffer.
var ErrMalformedResponse = errors.New("interbase services: malformed information response")

// ErrUnsupported indicates that the linked official client does not expose a
// requested service feature.
var ErrUnsupported = errors.New("interbase services: operation is not supported by the linked client")

// TLSConfig aliases the attachment TLS settings used by the root package.
type TLSConfig = interbase.TLSConfig

// Config contains the credentials and host settings for a Services Manager
// attachment. Host is a server prefix such as "localhost" or
// "localhost:service_mgr". An empty Host selects the local service manager.
type Config struct {
	Host                     string
	User                     string
	Password                 string
	EncryptedPassword        string
	SystemEncryptionPassword string
	TLS                      TLSConfig
}

// Capability is a bit reported by ServerInfo.Capabilities.
type Capability uint32

const (
	CapabilityMultiClient    Capability = 0x02
	CapabilityRemoteHop      Capability = 0x04
	CapabilityNoShutdown     Capability = 0x100
	CapabilityServerConfig   Capability = 0x200
	CapabilityQuotedFilename Capability = 0x400
)

// ServerInfo describes read-only properties returned by the Services Manager.
type ServerInfo struct {
	ServiceManagerVersion int
	ServerVersion         string
	Architecture          string
	HomeDirectory         string
	SecurityDatabasePath  string
	LockFileDirectory     string
	MessageFileDirectory  string
	Capabilities          uint32
	ConnectionCount       int
	AttachedDatabases     []string
}

// ServerDatabaseInfo is the structured result of the server attachment query.
type ServerDatabaseInfo struct {
	Attachments int
	Databases   []string
}

// BackupDestination describes one server-side backup file. MaxBytes applies
// to every destination except the final destination, matching the native SPB
// sequence format.
type BackupDestination struct {
	Path     string
	MaxBytes uint32
}

// BackupOptions selects documented logical backup options.
type BackupOptions struct {
	IgnoreChecksums  bool
	IgnoreLimbo      bool
	MetadataOnly     bool
	NoGarbageCollect bool
	NonTransportable bool
	ConvertExternal  bool
	Expand           bool
	ArchiveDatabase  bool
	ArchiveJournals  bool
}

// BackupRequest starts a native logical backup or archive backup.
type BackupRequest struct {
	SourceDatabase string
	Destinations   []BackupDestination
	Options        BackupOptions
	SEPPassword    string
	EncryptName    string
}

// RestoreDestination describes one restored database file. MaxPages applies
// to every destination except the final destination.
type RestoreDestination struct {
	Path     string
	MaxPages uint32
}

// RestoreOptions selects documented logical restore options.
type RestoreOptions struct {
	Replace              bool
	DeactivateIndexes    bool
	DoNotRestoreShadows  bool
	DoNotEnforceValidity bool
	CommitAfterEachTable bool
	UseAllPageSpace      bool
	NoDatabaseTriggers   bool // unsupported by the supplied official header
	MetadataOnly         bool
	ReadOnly             bool
	PageSize             uint32
	CacheBuffers         uint32
	SEPPassword          string
	DecryptPassword      string
}

// RestoreRequest starts a native logical restore.
type RestoreRequest struct {
	SourceFiles  []string
	Destinations []RestoreDestination
	Options      RestoreOptions
}

// DatabaseStatisticsOptions selects the gstat-like report sections. Data and
// index pages are included by default; the No* fields opt out of those sections.
type DatabaseStatisticsOptions struct {
	OnlyLogPages          bool
	OnlyHeaderPages       bool
	NoUserDataPages       bool
	NoUserIndexPages      bool
	IncludeSystemTables   bool
	IncludeRecordVersions bool
}

// DatabaseStatisticsRequest starts a database statistics report.
type DatabaseStatisticsRequest struct {
	Database string
	Options  DatabaseStatisticsOptions
}

// LogRequest starts retrieval of the server log.
type LogRequest struct{}

// DumpRequest starts a server-side database dump.
type DumpRequest struct {
	Database  string
	DumpFile  string
	Overwrite bool
}

// ArchiveBackupRequest starts an archive database or journal backup.
type ArchiveBackupRequest struct {
	Database string
	Journals bool
}

// ArchiveRestoreRequest recovers an archive into a new database.
type ArchiveRestoreRequest struct {
	BackupFile   string
	Database     string
	RecoverUntil *time.Time
}

// TablespaceBackupRequest backs up one tablespace.
type TablespaceBackupRequest struct {
	Database   string
	Tablespace string
	BackupFile string
}

// TablespaceRestoreRequest restores one tablespace backup.
type TablespaceRestoreRequest struct {
	SourceFiles  []string
	Destinations []RestoreDestination
	Tablespace   string
	BackupFile   string
	Create       bool
	Replace      bool
}

// ValidationRequest validates or repairs a database.
type ValidationRequest struct {
	Database               string
	ReadOnly               bool
	IgnoreChecksums        bool
	KillUnavailableShadows bool
	MendDatabase           bool
	SkipDatabaseValidation bool
	SkipRecordFragments    bool
}

// SweepRequest performs a database sweep.
type SweepRequest struct{ Database string }

// LimboTransactionsRequest lists transactions in limbo.
type LimboTransactionsRequest struct{ Database string }

// ResolveLimboRequest commits or rolls back one limbo transaction.
type ResolveLimboRequest struct {
	Database      string
	TransactionID int64
	Commit        bool
}

// PageBuffersRequest changes the database page cache size.
type PageBuffersRequest struct {
	Database string
	Buffers  uint32
}

// SweepIntervalRequest changes the automatic sweep threshold.
type SweepIntervalRequest struct {
	Database string
	Interval uint32
}

// ReserveSpaceRequest changes data-page reserve policy.
type ReserveSpaceRequest struct {
	Database string
	Reserve  bool
}

// WriteMode identifies forced or buffered writes.
type WriteMode uint8

const (
	WriteBuffered WriteMode = WriteMode(propertyWriteAsync)
	WriteForced   WriteMode = WriteMode(propertyWriteSync)
)

// WriteModeRequest changes the database write mode.
type WriteModeRequest struct {
	Database string
	Mode     WriteMode
}

// AccessMode identifies read-only or read-write database access.
type AccessMode uint8

const (
	AccessReadOnly  AccessMode = AccessMode(propertyAccessReadOnly)
	AccessReadWrite AccessMode = AccessMode(propertyAccessReadWrite)
)

// AccessModeRequest changes the database access mode.
type AccessModeRequest struct {
	Database string
	Mode     AccessMode
}

// SQLDialectRequest changes the database SQL dialect.
type SQLDialectRequest struct {
	Database string
	Dialect  int
}

// ActivateShadowRequest activates database shadows.
type ActivateShadowRequest struct{ Database string }

// ShutdownMode selects a database shutdown mode. ShutdownLegacy is the only
// mode encoded by this package because the supplied official SDK does not
// declare the newer shutdown-mode property code; other values return
// ErrUnsupported.
type ShutdownMode int8

const (
	ShutdownLegacy ShutdownMode = -1
	ShutdownNormal ShutdownMode = 0
	ShutdownMulti  ShutdownMode = 1
	ShutdownSingle ShutdownMode = 2
	ShutdownFull   ShutdownMode = 3
)

// ShutdownMethod selects which new activity is denied during shutdown.
type ShutdownMethod uint8

const (
	ShutdownForce               ShutdownMethod = ShutdownMethod(propertyShutdownDatabase)
	ShutdownDenyNewAttachments  ShutdownMethod = ShutdownMethod(propertyDenyAttachments)
	ShutdownDenyNewTransactions ShutdownMethod = ShutdownMethod(propertyDenyTransactions)
)

// ShutdownRequest shuts down a database for TimeoutSeconds.
type ShutdownRequest struct {
	Database       string
	Mode           ShutdownMode
	Method         ShutdownMethod
	TimeoutSeconds uint32
}

// OnlineRequest brings a shut-down database back online.
type OnlineRequest struct {
	Database string
	Mode     ShutdownMode
}

// User describes a security-database user. Password is accepted for add and
// modify requests but is never populated by ListUsers.
type User struct {
	Name       string
	Password   string
	FirstName  string
	MiddleName string
	LastName   string
	UserID     uint32
	GroupID    uint32
}

// ListUsersRequest lists all users or one named user.
type ListUsersRequest struct{ Name string }

// AddUserRequest adds a user.
type AddUserRequest struct{ User User }

// ModifyUserRequest modifies a user.
type ModifyUserRequest struct{ User User }

// DeleteUserRequest removes a user.
type DeleteUserRequest struct{ Name string }

// AddAliasRequest adds a server database alias.
type AddAliasRequest struct {
	Alias    string
	Database string
}

// DeleteAliasRequest removes a server database alias.
type DeleteAliasRequest struct{ Alias string }

// ListAliasesRequest lists server database aliases.
type ListAliasesRequest struct{}

// Request is the closed set of typed native service actions accepted by Start.
// Its unexported method prevents callers from injecting arbitrary SPB bytes.
type Request interface{ build() ([]byte, error) }

func (r BackupRequest) build() ([]byte, error) {
	if err := requireValue("source database", r.SourceDatabase); err != nil {
		return nil, err
	}
	if len(r.Destinations) == 0 {
		return nil, errors.New("interbase services: at least one backup destination is required")
	}
	if len(r.Destinations) > 9999 {
		return nil, errors.New("interbase services: at most 9999 backup destinations are supported")
	}
	if err := requireASCII("SEP password", r.SEPPassword); err != nil {
		return nil, err
	}
	if err := requireASCII("encryption name", r.EncryptName); err != nil {
		return nil, err
	}
	if r.Options.ArchiveDatabase && r.Options.ArchiveJournals {
		return nil, errors.New("interbase services: archive database and archive journals are mutually exclusive")
	}
	b := newRequestBuilder(actionBackup)
	if err := b.database(r.SourceDatabase); err != nil {
		return nil, err
	}
	for i, destination := range r.Destinations {
		if err := requireValue(fmt.Sprintf("backup destination %d", i), destination.Path); err != nil {
			return nil, err
		}
		if err := b.string(backupFile, destination.Path); err != nil {
			return nil, fmt.Errorf("destination %d: %w", i, err)
		}
		if i < len(r.Destinations)-1 {
			if err := b.numeric(backupLength, uint64(destination.MaxBytes), 4); err != nil {
				return nil, err
			}
		} else if destination.MaxBytes != 0 {
			return nil, errors.New("interbase services: backup destination sizes must contain exactly one less value than destinations")
		}
	}
	if err := b.stringOptional(serviceSPBSystemEncryption, r.SEPPassword); err != nil {
		return nil, err
	}
	if err := b.stringOptional(backupEncryptName, r.EncryptName); err != nil {
		return nil, err
	}
	b.options(r.Options.mask())
	b.code(serviceSPBVerbose)
	return b.bytes(), nil
}

func (o BackupOptions) mask() uint32 {
	var mask uint32
	if o.IgnoreChecksums {
		mask |= backupIgnoreChecksums
	}
	if o.IgnoreLimbo {
		mask |= backupIgnoreLimbo
	}
	if o.MetadataOnly {
		mask |= backupMetadataOnly
	}
	if o.NoGarbageCollect {
		mask |= backupNoGarbage
	}
	if o.NonTransportable {
		mask |= backupNonTransportable
	}
	if o.ConvertExternal {
		mask |= backupConvert
	}
	if o.Expand {
		mask |= backupExpand
	}
	if o.ArchiveDatabase {
		mask |= backupArchiveDatabase
	}
	if o.ArchiveJournals {
		mask |= backupArchiveJournals
	}
	return mask
}

func (r RestoreRequest) build() ([]byte, error) {
	if err := validateStringList("restore source", r.SourceFiles, true); err != nil {
		return nil, err
	}
	if len(r.Destinations) == 0 {
		return nil, errors.New("interbase services: at least one restore destination is required")
	}
	for i, destination := range r.Destinations {
		if err := requireValue(fmt.Sprintf("restore destination %d", i), destination.Path); err != nil {
			return nil, err
		}
		if i == len(r.Destinations)-1 && destination.MaxPages != 0 {
			return nil, errors.New("interbase services: restore destination sizes must contain exactly one less value than destinations")
		}
	}
	if err := requireASCII("SEP password", r.Options.SEPPassword); err != nil {
		return nil, err
	}
	if err := requireASCII("decrypt password", r.Options.DecryptPassword); err != nil {
		return nil, err
	}
	if r.Options.NoDatabaseTriggers {
		return nil, fmt.Errorf("%w: restore database triggers option", ErrUnsupported)
	}
	b := newRequestBuilder(actionRestore)
	for _, source := range r.SourceFiles {
		if err := b.string(backupFile, source); err != nil {
			return nil, err
		}
	}
	for i, destination := range r.Destinations {
		if err := b.string(serviceSPBDatabaseName, destination.Path); err != nil {
			return nil, err
		}
		if i < len(r.Destinations)-1 {
			if err := b.numeric(restoreLength, uint64(destination.MaxPages), 4); err != nil {
				return nil, err
			}
		}
	}
	if r.Options.PageSize != 0 {
		if err := b.numeric(restorePageSize, uint64(r.Options.PageSize), 4); err != nil {
			return nil, err
		}
	}
	if r.Options.SEPPassword != "" {
		if err := b.string(serviceSPBSystemEncryption, r.Options.SEPPassword); err != nil {
			return nil, err
		}
	}
	if r.Options.DecryptPassword != "" {
		if err := b.string(restoreDecryptPassword, r.Options.DecryptPassword); err != nil {
			return nil, err
		}
	}
	if r.Options.CacheBuffers != 0 {
		if err := b.numeric(restoreBuffers, uint64(r.Options.CacheBuffers), 4); err != nil {
			return nil, err
		}
	}
	access := uint64(propertyAccessReadWrite)
	if r.Options.ReadOnly {
		access = uint64(propertyAccessReadOnly)
	}
	if err := b.numeric(restoreAccessMode, access, 1); err != nil {
		return nil, err
	}
	var mask uint32
	if r.Options.Replace {
		mask |= restoreReplace
	} else {
		mask |= restoreCreate
	}
	if r.Options.DeactivateIndexes {
		mask |= restoreDeactivateIndexes
	}
	if r.Options.DoNotRestoreShadows {
		mask |= restoreNoShadows
	}
	if r.Options.DoNotEnforceValidity {
		mask |= restoreNoValidity
	}
	if r.Options.CommitAfterEachTable {
		mask |= restoreOneAtATime
	}
	if r.Options.UseAllPageSpace {
		mask |= restoreUseAllSpace
	}
	if r.Options.MetadataOnly {
		mask |= restoreMetadataOnly
	}
	b.options(mask)
	b.code(serviceSPBVerbose)
	return b.bytes(), nil
}

func (r DatabaseStatisticsRequest) build() ([]byte, error) {
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionDatabaseStats)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	o := r.Options
	var mask uint32
	if o.OnlyHeaderPages {
		mask = 0x04
	} else {
		if o.OnlyLogPages {
			mask |= 0x02
		}
		if !o.NoUserDataPages {
			mask |= 0x01
		}
		if !o.NoUserIndexPages {
			mask |= 0x08
		}
		if o.IncludeSystemTables {
			mask |= 0x10
		}
		if o.IncludeRecordVersions {
			mask |= 0x20
		}
	}
	b.options(mask)
	return b.bytes(), nil
}

func (LogRequest) build() ([]byte, error) { return newRequestBuilder(actionGetLog).bytes(), nil }

func (r DumpRequest) build() ([]byte, error) {
	if err := requireValue("database", r.Database); err != nil {
		return nil, err
	}
	if err := requireValue("dump file", r.DumpFile); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionDump)
	b.options(dumpCreate)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	if err := b.string(dumpFile, r.DumpFile); err != nil {
		return nil, err
	}
	if r.Overwrite {
		b.code(dumpOverwrite)
	}
	return b.bytes(), nil
}

func (r ArchiveBackupRequest) build() ([]byte, error) {
	if err := requireValue("database", r.Database); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionBackup)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	mask := backupArchiveDatabase
	if r.Journals {
		mask = backupArchiveJournals
	}
	b.options(mask)
	b.code(serviceSPBVerbose)
	return b.bytes(), nil
}

func (r ArchiveRestoreRequest) build() ([]byte, error) {
	if err := requireASCII("archive backup file", r.BackupFile); err != nil {
		return nil, err
	}
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionRestore)
	if r.RecoverUntil != nil {
		if err := b.string(restoreArchiveUntil, r.RecoverUntil.UTC().Format("2006-01-02 15:04:05.000000")[:24]); err != nil {
			return nil, err
		}
	}
	b.options(restoreArchiveRecover)
	if err := b.string(backupFile, r.BackupFile); err != nil {
		return nil, err
	}
	if err := b.string(serviceSPBDatabaseName, r.Database); err != nil {
		return nil, err
	}
	b.code(serviceSPBVerbose)
	return b.bytes(), nil
}

func (r TablespaceBackupRequest) build() ([]byte, error) {
	if err := requireValue("database", r.Database); err != nil {
		return nil, err
	}
	if err := requireValue("tablespace", r.Tablespace); err != nil {
		return nil, err
	}
	if err := requireValue("tablespace backup file", r.BackupFile); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionBackup)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	if err := b.string(tablespaceFile, r.BackupFile); err != nil {
		return nil, err
	}
	if err := b.string(tablespaceInclude, r.Tablespace); err != nil {
		return nil, err
	}
	b.code(serviceSPBVerbose)
	return b.bytes(), nil
}

func (r TablespaceRestoreRequest) build() ([]byte, error) {
	if err := validateStringList("tablespace source", r.SourceFiles, true); err != nil {
		return nil, err
	}
	if len(r.Destinations) == 0 {
		return nil, errors.New("interbase services: at least one tablespace destination is required")
	}
	for i, d := range r.Destinations {
		if err := requireValue(fmt.Sprintf("tablespace destination %d", i), d.Path); err != nil {
			return nil, err
		}
		if i == len(r.Destinations)-1 && d.MaxPages != 0 {
			return nil, errors.New("interbase services: tablespace destination sizes must contain exactly one less value than destinations")
		}
	}
	b := newRequestBuilder(actionRestore)
	var mask uint32
	if r.Create {
		mask |= restoreCreateTablespace
	}
	if r.Replace {
		mask |= restoreReplaceTablespace
	}
	b.options(mask)
	for _, source := range r.SourceFiles {
		if err := b.string(backupFile, source); err != nil {
			return nil, err
		}
	}
	for i, d := range r.Destinations {
		if err := b.string(serviceSPBDatabaseName, d.Path); err != nil {
			return nil, err
		}
		if i < len(r.Destinations)-1 {
			if err := b.numeric(restoreLength, uint64(d.MaxPages), 4); err != nil {
				return nil, err
			}
		}
	}
	if err := b.string(tablespaceFile, r.BackupFile); err != nil {
		return nil, err
	}
	if err := b.string(tablespaceInclude, r.Tablespace); err != nil {
		return nil, err
	}
	b.code(serviceSPBVerbose)
	return b.bytes(), nil
}

func (r ValidationRequest) build() ([]byte, error) {
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	var mask uint32
	if r.ReadOnly {
		mask |= uint32(repairCheckDatabase)
	}
	if r.IgnoreChecksums {
		mask |= uint32(repairIgnoreChecksum)
	}
	if r.KillUnavailableShadows {
		mask |= uint32(repairKillShadows)
	}
	if r.MendDatabase {
		mask |= uint32(repairMendDatabase)
	}
	if !r.SkipDatabaseValidation {
		mask |= uint32(repairValidateDatabase)
	}
	if !r.SkipRecordFragments {
		mask |= uint32(repairFull)
	}
	b := newRequestBuilder(actionRepair)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	b.options(mask)
	return b.bytes(), nil
}

func (r SweepRequest) build() ([]byte, error) {
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionRepair)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	b.options(uint32(repairSweepDatabase))
	return b.bytes(), nil
}

func (r LimboTransactionsRequest) build() ([]byte, error) {
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionRepair)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	b.options(uint32(repairListLimbo))
	return b.bytes(), nil
}

func (r ResolveLimboRequest) build() ([]byte, error) {
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	if r.TransactionID < 0 || r.TransactionID > math.MaxUint32 {
		return nil, errors.New("interbase services: transaction ID is outside the native range")
	}
	b := newRequestBuilder(actionRepair)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	code := repairRollbackTransaction
	if r.Commit {
		code = repairCommitTransaction
	}
	if err := b.numeric(code, uint64(r.TransactionID), 4); err != nil {
		return nil, err
	}
	return b.bytes(), nil
}

func (r PageBuffersRequest) build() ([]byte, error) {
	return buildPropertyNumeric(r.Database, propertyPageBuffers, uint64(r.Buffers), 4)
}
func (r SweepIntervalRequest) build() ([]byte, error) {
	return buildPropertyNumeric(r.Database, propertySweepInterval, uint64(r.Interval), 4)
}

func (r ReserveSpaceRequest) build() ([]byte, error) {
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionProperties)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	value := uint64(propertyReserveUseFull)
	if r.Reserve {
		value = uint64(propertyReserve)
	}
	if err := b.numeric(propertyReserveSpace, value, 1); err != nil {
		return nil, err
	}
	return b.bytes(), nil
}

func (r WriteModeRequest) build() ([]byte, error) {
	if r.Mode != WriteForced && r.Mode != WriteBuffered {
		return nil, errors.New("interbase services: write mode must be WriteForced or WriteBuffered")
	}
	return buildPropertyNumeric(r.Database, propertyWriteMode, uint64(r.Mode), 1)
}

func (r AccessModeRequest) build() ([]byte, error) {
	if r.Mode != AccessReadOnly && r.Mode != AccessReadWrite {
		return nil, errors.New("interbase services: access mode must be AccessReadOnly or AccessReadWrite")
	}
	return buildPropertyNumeric(r.Database, propertyAccessMode, uint64(r.Mode), 1)
}

func (r SQLDialectRequest) build() ([]byte, error) {
	if r.Dialect < 0 || r.Dialect > math.MaxUint32 {
		return nil, errors.New("interbase services: SQL dialect is outside the native range")
	}
	return buildPropertyNumeric(r.Database, propertySQLDialect, uint64(r.Dialect), 4)
}

func (r ActivateShadowRequest) build() ([]byte, error) {
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionProperties)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	b.options(propertyActivate)
	return b.bytes(), nil
}

func (r ShutdownRequest) build() ([]byte, error) {
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	if r.Mode < ShutdownLegacy || r.Mode > ShutdownFull {
		return nil, errors.New("interbase services: invalid shutdown mode")
	}
	if r.Mode != ShutdownLegacy {
		return nil, fmt.Errorf("%w: shutdown modes beyond legacy are not declared by the official SDK", ErrUnsupported)
	}
	if r.Method != ShutdownForce && r.Method != ShutdownDenyNewAttachments && r.Method != ShutdownDenyNewTransactions {
		return nil, errors.New("interbase services: invalid shutdown method")
	}
	b := newRequestBuilder(actionProperties)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	if err := b.numeric(byte(r.Method), uint64(r.TimeoutSeconds), 4); err != nil {
		return nil, err
	}
	return b.bytes(), nil
}

func (r OnlineRequest) build() ([]byte, error) {
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	if r.Mode < ShutdownLegacy || r.Mode > ShutdownSingle {
		return nil, errors.New("interbase services: invalid online mode")
	}
	b := newRequestBuilder(actionProperties)
	if err := b.database(r.Database); err != nil {
		return nil, err
	}
	if r.Mode == ShutdownLegacy {
		b.options(propertyOnline)
	} else {
		return nil, fmt.Errorf("%w: online modes beyond legacy are not declared by the official SDK", ErrUnsupported)
	}
	return b.bytes(), nil
}

func (r ListUsersRequest) build() ([]byte, error) {
	return buildListRequest(actionDisplayUser, r.Name, securityUserName)
}

func (r AddUserRequest) build() ([]byte, error) { return buildUserRequest(actionAddUser, r.User, true) }
func (r ModifyUserRequest) build() ([]byte, error) {
	return buildUserRequest(actionModifyUser, r.User, false)
}

func (r DeleteUserRequest) build() ([]byte, error) {
	if err := requireValue("username", r.Name); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionDeleteUser)
	if err := b.string(securityUserName, strings.ToUpper(r.Name)); err != nil {
		return nil, err
	}
	return b.bytes(), nil
}

func (r AddAliasRequest) build() ([]byte, error) {
	if err := requireASCII("alias", r.Alias); err != nil {
		return nil, err
	}
	if err := requireASCII("database", r.Database); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionAddAlias)
	if err := b.string(aliasName, r.Alias); err != nil {
		return nil, err
	}
	if err := b.string(aliasPath, r.Database); err != nil {
		return nil, err
	}
	return b.bytes(), nil
}

func (r DeleteAliasRequest) build() ([]byte, error) {
	if err := requireASCII("alias", r.Alias); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionDeleteAlias)
	if err := b.string(aliasName, r.Alias); err != nil {
		return nil, err
	}
	return b.bytes(), nil
}

func (ListAliasesRequest) build() ([]byte, error) {
	return newRequestBuilder(actionDisplayAlias).bytes(), nil
}

func buildPropertyNumeric(database string, code byte, value uint64, width int) ([]byte, error) {
	if err := requireASCII("database", database); err != nil {
		return nil, err
	}
	b := newRequestBuilder(actionProperties)
	if err := b.database(database); err != nil {
		return nil, err
	}
	if err := b.numeric(code, value, width); err != nil {
		return nil, err
	}
	return b.bytes(), nil
}

func buildListRequest(action byte, name string, code byte) ([]byte, error) {
	if name != "" {
		if err := requireASCII("name", name); err != nil {
			return nil, err
		}
	}
	b := newRequestBuilder(action)
	if name != "" {
		if err := b.string(code, strings.ToUpper(name)); err != nil {
			return nil, err
		}
	}
	return b.bytes(), nil
}

func buildUserRequest(action byte, user User, requirePassword bool) ([]byte, error) {
	if err := requireValue("username", user.Name); err != nil {
		return nil, err
	}
	if requirePassword && user.Password == "" {
		return nil, errors.New("interbase services: password is required")
	}
	if err := requireASCII("password", user.Password); err != nil {
		return nil, err
	}
	for field, value := range map[string]string{"first name": user.FirstName, "middle name": user.MiddleName, "last name": user.LastName} {
		if err := requireASCII(field, value); err != nil {
			return nil, err
		}
	}
	b := newRequestBuilder(action)
	if err := b.string(securityUserName, strings.ToUpper(user.Name)); err != nil {
		return nil, err
	}
	if user.Password != "" {
		if err := b.string(securityPassword, user.Password); err != nil {
			return nil, err
		}
	}
	if action == actionModifyUser {
		if err := b.string(securityFirstName, user.FirstName); err != nil {
			return nil, err
		}
		if err := b.string(securityMiddleName, user.MiddleName); err != nil {
			return nil, err
		}
		if err := b.string(securityLastName, user.LastName); err != nil {
			return nil, err
		}
	} else {
		if err := b.stringOptional(securityFirstName, user.FirstName); err != nil {
			return nil, err
		}
		if err := b.stringOptional(securityMiddleName, user.MiddleName); err != nil {
			return nil, err
		}
		if err := b.stringOptional(securityLastName, user.LastName); err != nil {
			return nil, err
		}
	}
	return b.bytes(), nil
}

type requestBuilder struct{ buffer []byte }

func newRequestBuilder(action byte) *requestBuilder { return &requestBuilder{buffer: []byte{action}} }
func (b *requestBuilder) bytes() []byte             { return append([]byte(nil), b.buffer...) }
func (b *requestBuilder) code(code byte)            { b.buffer = append(b.buffer, code) }
func (b *requestBuilder) options(mask uint32)       { _ = b.numeric(serviceSPBOptions, uint64(mask), 4) }
func (b *requestBuilder) database(database string) error {
	return b.string(serviceSPBDatabaseName, database)
}
func (b *requestBuilder) stringOptional(code byte, value string) error {
	if value == "" {
		return nil
	}
	return b.string(code, value)
}

func (b *requestBuilder) string(code byte, value string) error {
	if err := requireASCII("service string", value); err != nil {
		return err
	}
	if len(value) > maxServiceParameterLength {
		return errors.New("interbase services: service string is too long")
	}
	b.buffer = append(b.buffer, code, byte(len(value)), byte(len(value)>>8))
	b.buffer = append(b.buffer, value...)
	return nil
}

func (b *requestBuilder) numeric(code byte, value uint64, width int) error {
	if width != 1 && width != 2 && width != 4 && width != 8 {
		return errors.New("interbase services: invalid numeric field width")
	}
	max := uint64(1)<<(uint(width)*8) - 1
	if value > max {
		return fmt.Errorf("interbase services: numeric value %d does not fit in %d bytes", value, width)
	}
	b.buffer = append(b.buffer, code)
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	b.buffer = append(b.buffer, encoded[:width]...)
	return nil
}

func appendAttachString(buffer []byte, code byte, value string) ([]byte, error) {
	if err := requireASCII("service credential", value); err != nil {
		return nil, err
	}
	if len(value) > math.MaxUint8 {
		return nil, errors.New("interbase services: credential is too long")
	}
	buffer = append(buffer, code, byte(len(value)))
	return append(buffer, value...), nil
}

func buildServiceSPB(cfg Config) ([]byte, error) {
	if err := requireASCII("user", cfg.User); err != nil {
		return nil, err
	}
	if cfg.Password == "" && cfg.EncryptedPassword == "" {
		return nil, errors.New("interbase services: password or encrypted password is required")
	}
	if err := requireASCII("password", cfg.Password); err != nil {
		return nil, err
	}
	if err := requireASCII("encrypted password", cfg.EncryptedPassword); err != nil {
		return nil, err
	}
	if err := requireASCII("system encryption password", cfg.SystemEncryptionPassword); err != nil {
		return nil, err
	}
	spb := []byte{serviceSPBVersion, serviceSPBCurrentVersion}
	var err error
	if spb, err = appendAttachString(spb, serviceSPBUserName, cfg.User); err != nil {
		return nil, err
	}
	if cfg.Password != "" {
		if spb, err = appendAttachString(spb, serviceSPBPassword, cfg.Password); err != nil {
			return nil, err
		}
	}
	if cfg.EncryptedPassword != "" {
		if spb, err = appendAttachString(spb, serviceSPBPasswordEncrypted, cfg.EncryptedPassword); err != nil {
			return nil, err
		}
	}
	if cfg.SystemEncryptionPassword != "" {
		if spb, err = appendAttachString(spb, serviceSPBSystemEncryption, cfg.SystemEncryptionPassword); err != nil {
			return nil, err
		}
	}
	return spb, nil
}

func buildServiceTarget(cfg Config) (string, error) {
	if err := requireASCII("host", cfg.Host); err != nil {
		return "", err
	}
	if strings.HasSuffix(cfg.Host, ":") {
		return "", errors.New("interbase services: host must not end with a colon")
	}
	if strings.Contains(cfg.Host, "?") {
		return "", errors.New("interbase services: host cannot contain attachment query delimiters")
	}
	target := cfg.Host
	if target == "" {
		target = "service_mgr"
	}
	if !strings.HasSuffix(target, "service_mgr") {
		target += ":service_mgr"
	}
	if cfg.TLS.Enabled || cfg.TLS.ServerPublicFile != "" || cfg.TLS.ClientCertFile != "" || cfg.TLS.ClientPassPhrase != "" || cfg.TLS.ClientPassPhraseFile != "" || cfg.TLS.ServerPublicPath != "" {
		if cfg.Host == "" {
			return "", errors.New("interbase services: TLS options require a host")
		}
		if !cfg.TLS.Enabled {
			return "", errors.New("interbase services: TLS options require TLS to be enabled")
		}
		parts := strings.Split(target, ":")
		if len(parts) < 2 {
			return "", errors.New("interbase services: invalid service manager target")
		}
		options := parts[0] + "?ssl=true"
		for _, option := range []struct{ name, value string }{
			{"serverPublicFile", cfg.TLS.ServerPublicFile},
			{"clientCertFile", cfg.TLS.ClientCertFile},
			{"clientPassPhrase", cfg.TLS.ClientPassPhrase},
			{"clientPassPhraseFile", cfg.TLS.ClientPassPhraseFile},
			{"serverPublicPath", cfg.TLS.ServerPublicPath},
		} {
			if option.value == "" {
				continue
			}
			if err := requireASCII(option.name, option.value); err != nil {
				return "", err
			}
			if strings.Contains(option.value, "?") {
				return "", fmt.Errorf("interbase services: TLS option %s contains attachment query delimiters", option.name)
			}
			options += "?" + option.name + "=" + option.value
		}
		target = options + ":" + strings.Join(parts[1:], ":")
		target = strings.Replace(target, "?ssl=true:service_mgr", "?ssl=true??:service_mgr", 1)
		if !strings.Contains(target, "??:") {
			target = strings.Replace(target, ":service_mgr", "??:service_mgr", 1)
		}
	}
	if len(target) > math.MaxInt16 {
		return "", errors.New("interbase services: service manager target is too long")
	}
	return target, nil
}

func requireASCII(field, value string) error {
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("interbase services: %s contains NUL", field)
	}
	for _, r := range value {
		if r > unicode.MaxASCII || unicode.IsControl(r) {
			return fmt.Errorf("interbase services: %s must contain printable ASCII", field)
		}
	}
	return nil
}

func requireValue(field, value string) error {
	if value == "" {
		return fmt.Errorf("interbase services: %s is required", field)
	}
	return requireASCII(field, value)
}

func validateStringList(field string, values []string, required bool) error {
	if required && len(values) == 0 {
		return fmt.Errorf("interbase services: at least one %s is required", field)
	}
	for i, value := range values {
		if err := requireASCII(fmt.Sprintf("%s %d", field, i), value); err != nil {
			return err
		}
	}
	return nil
}

func decodeInfoString(raw []byte, expected byte) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("%w: empty string response", ErrMalformedResponse)
	}
	if raw[0] == infoTruncated {
		return "", ErrOutputLimit
	}
	if raw[0] == infoError {
		return "", fmt.Errorf("%w: service returned information error", ErrMalformedResponse)
	}
	if raw[0] != expected || len(raw) < 3 {
		return "", fmt.Errorf("%w: string item %d", ErrMalformedResponse, expected)
	}
	length := int(binary.LittleEndian.Uint16(raw[1:3]))
	if length > len(raw)-3 {
		return "", fmt.Errorf("%w: string length %d exceeds %d bytes", ErrMalformedResponse, length, len(raw)-3)
	}
	return string(raw[3 : 3+length]), nil
}

func decodeInfoUint16(raw []byte, expected byte) (uint16, error) {
	if len(raw) < 3 || raw[0] != expected {
		return 0, fmt.Errorf("%w: integer item %d", ErrMalformedResponse, expected)
	}
	return binary.LittleEndian.Uint16(raw[1:3]), nil
}

// decodeOutput decodes one bounded isc_info_svc_to_eof response. Pending and
// continuation markers are valid intermediate states; Job.run must consult
// isc_info_svc_running before declaring the action complete.
func decodeOutput(raw []byte, truncated bool) ([]byte, bool, error) {
	if len(raw) == 1 && raw[0] == infoDataNotReady {
		return nil, false, nil
	}
	if len(raw) == 1 && raw[0] == infoTruncated {
		return nil, false, nil
	}
	if len(raw) == 0 {
		return nil, false, fmt.Errorf("%w: empty output response", ErrMalformedResponse)
	}
	if len(raw) < 4 {
		return nil, false, fmt.Errorf("%w: incomplete output frame", ErrMalformedResponse)
	}
	if raw[0] != infoToEOF && raw[0] != infoLine {
		return nil, false, fmt.Errorf("%w: output item %d", ErrMalformedResponse, raw[0])
	}
	length := int(binaryLittleEndianUint16(raw[1:3]))
	markerPosition := 3 + length
	if markerPosition >= len(raw) || markerPosition != len(raw)-1 {
		return nil, false, fmt.Errorf("%w: output frame length", ErrMalformedResponse)
	}
	marker := raw[markerPosition]
	if marker != infoEnd && marker != infoFlagEnd && marker != infoTruncated {
		return nil, false, fmt.Errorf("%w: output frame marker %d", ErrMalformedResponse, marker)
	}
	if truncated && marker == infoEnd {
		return nil, false, ErrOutputLimit
	}
	return append([]byte(nil), raw[3:markerPosition]...), marker == infoEnd, nil
}

func decodeServerDatabaseInfo(raw []byte) (ServerDatabaseInfo, error) {
	if len(raw) < 2 || raw[0] != infoServerDatabase {
		return ServerDatabaseInfo{}, fmt.Errorf("%w: server database item", ErrMalformedResponse)
	}
	result := ServerDatabaseInfo{}
	for position := 1; position < len(raw); {
		if raw[position] == infoFlagEnd || raw[position] == infoEnd {
			break
		}
		code := raw[position]
		position++
		switch code {
		case serverInfoAttachments:
			if position+4 > len(raw) {
				return ServerDatabaseInfo{}, fmt.Errorf("%w: attachment count", ErrMalformedResponse)
			}
			result.Attachments = int(binary.LittleEndian.Uint32(raw[position : position+4]))
			position += 4
		case serverInfoDatabases:
			if position+4 > len(raw) {
				return ServerDatabaseInfo{}, fmt.Errorf("%w: database count", ErrMalformedResponse)
			}
			position += 4
		case serverInfoDatabaseName:
			if position+2 > len(raw) {
				return ServerDatabaseInfo{}, fmt.Errorf("%w: database name length", ErrMalformedResponse)
			}
			length := int(binary.LittleEndian.Uint16(raw[position : position+2]))
			position += 2
			if position+length > len(raw) {
				return ServerDatabaseInfo{}, fmt.Errorf("%w: database name", ErrMalformedResponse)
			}
			result.Databases = append(result.Databases, string(raw[position:position+length]))
			position += length
		default:
			return ServerDatabaseInfo{}, fmt.Errorf("%w: unknown server database item %d", ErrMalformedResponse, code)
		}
	}
	return result, nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
