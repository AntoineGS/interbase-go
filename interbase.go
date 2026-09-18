package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TLSConfig contains the InterBase native client TLS attachment options.
//
// The native client expects these values in the attachment string. Enabled
// must be true for any of the option fields to be used.
type TLSConfig struct {
	Enabled              bool
	ServerPublicFile     string
	ClientCertFile       string
	ClientPassPhrase     string
	ClientPassPhraseFile string
	ServerPublicPath     string
}

func (c TLSConfig) hasOptions() bool {
	return c.Enabled || c.ServerPublicFile != "" || c.ClientCertFile != "" ||
		c.ClientPassPhrase != "" || c.ClientPassPhraseFile != "" ||
		c.ServerPublicPath != ""
}

// TableSharingMode selects how a table reservation is shared between
// transactions.
type TableSharingMode uint8

const (
	TableSharingShared    TableSharingMode = 3
	TableSharingProtected TableSharingMode = 4
	TableSharingExclusive TableSharingMode = 5
)

// TableAccessMode selects the access granted by a table reservation.
type TableAccessMode uint8

const (
	TableAccessRead  TableAccessMode = 10
	TableAccessWrite TableAccessMode = 11
)

// TableReservation describes one InterBase table reservation in a transaction
// parameter block.
type TableReservation struct {
	Table       string
	SharingMode TableSharingMode
	AccessMode  TableAccessMode
}

// TransactionOptions contains connector-wide defaults for native transaction
// parameter blocks. Isolation and ReadOnly are used by the direct API's
// Attachment.BeginTx method; database/sql.TxOptions still controls those
// properties for database/sql transactions.
//
// The zero value requests the Python-compatible WAIT, RECORD_VERSION,
// read-committed defaults.
type TransactionOptions struct {
	NoWait            bool
	NoRecordVersion   bool
	TableReservations []TableReservation
	Isolation         sql.IsolationLevel
	ReadOnly          bool
}

// Config contains the connection settings for an InterBase attachment.
//
// Dialect selects the SQL dialect for the attachment. Zero selects Dialect 3;
// Dialects 1 and 3 are supported. Set Dialect to 1 to opt into Dialect 1.
// Charset is the InterBase attachment
// character set; an empty value selects UTF8. Passwords are sent only while
// opening an attachment (and are retained only for error redaction); they are
// never included in driver errors.
type Config struct {
	Database                 string
	Host                     string
	User                     string
	Password                 string
	Role                     string
	EncryptedPassword        string
	SystemEncryptionPassword string
	Charset                  string
	Dialect                  int
	// ConnectTimeout bounds the native attachment handshake. Zero leaves the
	// InterBase client default unchanged; positive values are rounded up to
	// whole seconds because the native DPB stores an unsigned 32-bit count.
	ConnectTimeout     time.Duration
	TLS                TLSConfig
	TransactionOptions TransactionOptions
}

var (
	errConnectorOnly         = errors.New("DSN-based opening is unsupported; use NewConnector")
	errTransactionDone       = errors.New("interbase: transaction is already completed")
	errStatementClosed       = errors.New("interbase: prepared statement is closed")
	errConnectionInvalidated = errors.New("interbase: connection was invalidated")
)

// NewConnector validates cfg and returns a database/sql connector. Open a
// database with sql.OpenDB(NewConnector(...)).
func NewConnector(cfg Config) (driver.Connector, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	dialect, err := normalizeDialect(cfg.Dialect)
	if err != nil {
		return nil, err
	}
	charset, err := normalizeCharset(cfg.Charset)
	if err != nil {
		return nil, err
	}
	connectTimeout, err := normalizeConnectTimeout(cfg.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	transactionOptions, err := normalizeTransactionOptions(cfg.TransactionOptions)
	if err != nil {
		return nil, err
	}
	cfg.Dialect = dialect
	cfg.Charset = charset
	cfg.ConnectTimeout = connectTimeout
	cfg.TransactionOptions = transactionOptions
	return &connector{cfg: cfg}, nil
}

func validateConfig(cfg Config) error {
	if _, err := normalizeDialect(cfg.Dialect); err != nil {
		return err
	}
	if cfg.Database == "" {
		return errors.New("interbase: database is required")
	}
	if cfg.User == "" {
		return errors.New("interbase: user is required")
	}
	if strings.IndexByte(cfg.Database, 0) >= 0 || strings.IndexByte(cfg.Host, 0) >= 0 ||
		strings.IndexByte(cfg.User, 0) >= 0 || strings.IndexByte(cfg.Password, 0) >= 0 ||
		strings.IndexByte(cfg.Role, 0) >= 0 || strings.IndexByte(cfg.EncryptedPassword, 0) >= 0 ||
		strings.IndexByte(cfg.SystemEncryptionPassword, 0) >= 0 ||
		strings.IndexByte(cfg.TLS.ServerPublicFile, 0) >= 0 ||
		strings.IndexByte(cfg.TLS.ClientCertFile, 0) >= 0 ||
		strings.IndexByte(cfg.TLS.ClientPassPhrase, 0) >= 0 ||
		strings.IndexByte(cfg.TLS.ClientPassPhraseFile, 0) >= 0 ||
		strings.IndexByte(cfg.TLS.ServerPublicPath, 0) >= 0 {
		return errors.New("interbase: connection settings cannot contain NUL bytes")
	}
	if len(cfg.Database) > math.MaxInt16 {
		return errors.New("interbase: database name is too long")
	}
	if len(cfg.User) > math.MaxUint8 || len(cfg.Password) > math.MaxUint8 ||
		len(cfg.Role) > math.MaxUint8 || len(cfg.EncryptedPassword) > math.MaxUint8 ||
		len(cfg.SystemEncryptionPassword) > math.MaxUint8 {
		return errors.New("interbase: credential is too long")
	}
	if _, err := normalizeCharset(cfg.Charset); err != nil {
		return err
	}
	if _, err := normalizeConnectTimeout(cfg.ConnectTimeout); err != nil {
		return err
	}
	if _, err := normalizeTransactionOptions(cfg.TransactionOptions); err != nil {
		return err
	}
	if _, err := buildAttachment(cfg); err != nil {
		return err
	}
	return nil
}

func buildAttachment(cfg Config) (string, error) {
	tls := cfg.TLS
	if strings.IndexByte(cfg.Database, 0) >= 0 || strings.IndexByte(cfg.Host, 0) >= 0 {
		return "", errors.New("interbase: attachment settings cannot contain NUL bytes")
	}
	if cfg.Host == "" {
		if tls.hasOptions() {
			return "", errors.New("interbase: TLS options require a host")
		}
		return cfg.Database, nil
	}
	if strings.HasSuffix(cfg.Host, ":") {
		return "", errors.New("interbase: host must not end with a colon")
	}
	if strings.Contains(cfg.Host, "?") {
		return "", errors.New("interbase: host cannot contain attachment query delimiters")
	}
	if strings.Contains(cfg.Database, "?") {
		return "", errors.New("interbase: database cannot contain attachment query delimiters when host is set")
	}
	if !tls.Enabled && tls.hasOptions() {
		return "", errors.New("interbase: TLS options require TLS to be enabled")
	}

	attachment := cfg.Host
	if tls.Enabled {
		attachment += "?ssl=true"
		for _, option := range []struct {
			name  string
			value string
		}{
			{name: "serverPublicFile", value: tls.ServerPublicFile},
			{name: "clientCertFile", value: tls.ClientCertFile},
			{name: "clientPassPhrase", value: tls.ClientPassPhrase},
			{name: "clientPassPhraseFile", value: tls.ClientPassPhraseFile},
			{name: "serverPublicPath", value: tls.ServerPublicPath},
		} {
			if option.value == "" {
				continue
			}
			if strings.Contains(option.value, "?") {
				return "", fmt.Errorf("interbase: TLS option %s cannot contain attachment query delimiters", option.name)
			}
			attachment += "?" + option.name + "=" + option.value
		}
		attachment += "??"
	}
	if attachment != "" {
		attachment += ":"
	}
	attachment += cfg.Database
	if len(attachment) > math.MaxInt16 {
		return "", errors.New("interbase: attachment string is too long")
	}
	return attachment, nil
}

func normalizeTransactionOptions(options TransactionOptions) (TransactionOptions, error) {
	options.TableReservations = append([]TableReservation(nil), options.TableReservations...)
	for index := range options.TableReservations {
		reservation := &options.TableReservations[index]
		normalized, err := normalizeTableReservationName(reservation.Table)
		if err != nil {
			return TransactionOptions{}, fmt.Errorf("interbase: table reservation %d: %w", index, err)
		}
		reservation.Table = normalized
		switch reservation.SharingMode {
		case TableSharingShared, TableSharingProtected, TableSharingExclusive:
		default:
			return TransactionOptions{}, fmt.Errorf("interbase: table reservation %d has an unsupported sharing mode", index)
		}
		switch reservation.AccessMode {
		case TableAccessRead, TableAccessWrite:
		default:
			return TransactionOptions{}, fmt.Errorf("interbase: table reservation %d has an unsupported access mode", index)
		}
	}
	return options, nil
}

func normalizeTableReservationName(name string) (string, error) {
	if name == "" {
		return "", errors.New("has no table name")
	}
	startsQuoted := strings.HasPrefix(name, "\"")
	endsQuoted := strings.HasSuffix(name, "\"")
	if startsQuoted != endsQuoted {
		return "", errors.New("has an unmatched quote")
	}
	encoded := name
	if startsQuoted {
		if len(name) < 2 {
			return "", errors.New("has an incomplete quoted name")
		}
		encoded = name[1 : len(name)-1]
		if encoded == "" {
			return "", errors.New("has no table name")
		}
	} else if strings.Contains(name, "\"") {
		return "", errors.New("has an unmatched quote")
	}
	if len(encoded) > math.MaxUint8-1 {
		return "", errors.New("name is too long")
	}
	for position := 0; position < len(encoded); position++ {
		if encoded[position] > 0x7f || encoded[position] == 0 {
			return "", errors.New("name must be ASCII")
		}
	}
	if startsQuoted {
		// Keep the marker so a second normalization pass remains idempotent and
		// the TPB encoder can preserve the quoted identifier's case.
		return name, nil
	}
	return strings.ToUpper(name), nil
}

func encodeTableReservationName(name string) string {
	if len(name) >= 2 && strings.HasPrefix(name, "\"") && strings.HasSuffix(name, "\"") {
		return name[1 : len(name)-1]
	}
	return name
}

func buildTPB(options driver.TxOptions, defaults TransactionOptions) ([]byte, error) {
	if err := validateTxOptions(options); err != nil {
		return nil, err
	}
	defaults, err := normalizeTransactionOptions(defaults)
	if err != nil {
		return nil, err
	}

	const (
		tpbVersion3      byte = 3
		tpbConsistency   byte = 1
		tpbConcurrency   byte = 2
		tpbWait          byte = 6
		tpbNoWait        byte = 7
		tpbRead          byte = 8
		tpbWrite         byte = 9
		tpbReadCommitted byte = 15
		tpbRecordVersion byte = 17
		tpbNoRecord      byte = 18
	)

	tpb := []byte{tpbVersion3, tpbWrite}
	if options.ReadOnly {
		tpb[1] = tpbRead
	}
	if defaults.NoWait {
		tpb = append(tpb, tpbNoWait)
	} else {
		tpb = append(tpb, tpbWait)
	}
	switch options.Isolation {
	case driver.IsolationLevel(sql.LevelDefault), driver.IsolationLevel(sql.LevelReadCommitted):
		tpb = append(tpb, tpbReadCommitted)
		if defaults.NoRecordVersion {
			tpb = append(tpb, tpbNoRecord)
		} else {
			tpb = append(tpb, tpbRecordVersion)
		}
	case driver.IsolationLevel(sql.LevelRepeatableRead), driver.IsolationLevel(sql.LevelSnapshot):
		if defaults.NoRecordVersion {
			return nil, errors.New("interbase: record-version policy only applies to read-committed isolation")
		}
		tpb = append(tpb, tpbConcurrency)
	case driver.IsolationLevel(sql.LevelSerializable):
		if defaults.NoRecordVersion {
			return nil, errors.New("interbase: record-version policy only applies to read-committed isolation")
		}
		tpb = append(tpb, tpbConsistency)
	}
	for _, reservation := range defaults.TableReservations {
		tableName := encodeTableReservationName(reservation.Table)
		nameLength := len(tableName) + 1
		if len(tpb) > math.MaxInt16-(nameLength+4) {
			return nil, errors.New("interbase: transaction parameter block is too long")
		}
		tpb = append(tpb, byte(reservation.AccessMode), byte(nameLength))
		tpb = append(tpb, tableName...)
		tpb = append(tpb, 0, byte(reservation.SharingMode))
	}
	return tpb, nil
}

func normalizeDialect(dialect int) (int, error) {
	switch dialect {
	case 0, 3:
		return 3, nil
	case 1:
		return 1, nil
	default:
		return 0, fmt.Errorf("interbase: unsupported SQL dialect %d; want 1 or 3", dialect)
	}
}

func normalizeCharset(charset string) (string, error) {
	switch strings.ToUpper(charset) {
	case "":
		return "UTF8", nil
	case "UTF8", "WIN1250", "WIN1252", "ISO8859_1", "ASCII":
		return strings.ToUpper(charset), nil
	default:
		return "", fmt.Errorf("interbase: unsupported character set %q", charset)
	}
}

const maxConnectTimeoutSeconds = uint64(^uint32(0))

func normalizeConnectTimeout(timeout time.Duration) (time.Duration, error) {
	if timeout < 0 {
		return 0, errors.New("interbase: connect timeout cannot be negative")
	}
	if timeout == 0 {
		return 0, nil
	}

	seconds := uint64(timeout / time.Second)
	if timeout%time.Second != 0 {
		seconds++
	}
	if seconds > maxConnectTimeoutSeconds {
		return 0, errors.New("interbase: connect timeout exceeds the native limit")
	}
	return time.Duration(seconds) * time.Second, nil
}

type connector struct {
	cfg Config
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	native, err := openNativeContext(ctx, c.cfg)
	if err != nil {
		return nil, sanitizeError("connect", err, configSecrets(c.cfg)...)
	}
	if err := contextError(ctx); err != nil {
		closeErr := native.close()
		if closeErr != nil {
			return nil, errors.Join(err, sanitizeError("close connection", closeErr, configSecrets(c.cfg)...))
		}
		return nil, err
	}

	return &conn{
		native:             native,
		transactionOptions: c.cfg.TransactionOptions,
		redactionSecrets:   configSecrets(c.cfg),
	}, nil
}

func (c *connector) Driver() driver.Driver {
	return driverInstance
}

type driverImpl struct{}

func (driverImpl) Open(string) (driver.Conn, error) {
	return nil, errConnectorOnly
}

func (driverImpl) OpenConnector(string) (driver.Connector, error) {
	return nil, errConnectorOnly
}

var driverInstance driverImpl

type argumentKind uint8

const (
	argumentNull argumentKind = iota
	argumentString
	argumentInt64
	argumentFloat64
	argumentBool
	argumentTimestamp
	argumentBytes
	argumentArray
	argumentBlobRef
)

type argument struct {
	kind         argumentKind
	stringValue  string
	int64Value   int64
	float64Value float64
	boolValue    bool
	timeValue    time.Time
	bytesValue   []byte
	array        *Array
	blobRef      BlobRef
}

func normalizeArgumentValue(value any) (any, error) {
	converted, err := driver.DefaultParameterConverter.ConvertValue(value)
	if err != nil {
		return nil, fmt.Errorf("interbase: argument type %T is unsupported: %w", value, err)
	}
	return converted, nil
}

func convertNormalizedArgument(value any) (argument, error) {
	switch value := value.(type) {
	case nil:
		return argument{kind: argumentNull}, nil
	case string:
		return argument{kind: argumentString, stringValue: value}, nil
	case []byte:
		bytesValue := make([]byte, len(value))
		copy(bytesValue, value)
		return argument{kind: argumentBytes, bytesValue: bytesValue}, nil
	case int64:
		return argument{kind: argumentInt64, int64Value: value}, nil
	case float64:
		return argument{kind: argumentFloat64, float64Value: value}, nil
	case bool:
		return argument{kind: argumentBool, boolValue: value}, nil
	case time.Time:
		return argument{kind: argumentTimestamp, timeValue: value}, nil
	default:
		return argument{}, fmt.Errorf("interbase: argument type %T is unsupported", value)
	}
}

func convertArgument(value any) (argument, error) {
	switch typed := value.(type) {
	case Array:
		array, err := normalizeDirectArray(typed)
		if err != nil {
			return argument{}, err
		}
		return argument{kind: argumentArray, array: array}, nil
	case *Array:
		if typed == nil {
			return argument{kind: argumentNull}, nil
		}
		array, err := normalizeDirectArray(*typed)
		if err != nil {
			return argument{}, err
		}
		return argument{kind: argumentArray, array: array}, nil
	case BlobRef:
		if typed.attachment == nil || typed.tx == nil || typed.generation == 0 ||
			(typed.high == 0 && typed.low == 0) {
			return argument{}, errDirectBlobRefInvalid
		}
		return argument{
			kind:    argumentBlobRef,
			blobRef: typed,
		}, nil
	}
	converted, err := normalizeArgumentValue(value)
	if err != nil {
		return argument{}, err
	}
	return convertNormalizedArgument(converted)
}

func normalizeNamedValue(value *driver.NamedValue) error {
	if value == nil {
		return errors.New("interbase: nil named value")
	}
	if value.Name != "" {
		return errors.New("interbase: named arguments are unsupported; use positional '?' arguments")
	}
	converted, err := normalizeArgumentValue(value.Value)
	if err != nil {
		return err
	}
	value.Value = converted
	return nil
}

func convertNamedValues(values []driver.NamedValue) ([]argument, error) {
	if len(values) > math.MaxInt16 {
		return nil, errors.New("interbase: too many query arguments")
	}

	args := make([]argument, len(values))
	for i := range values {
		value := &values[i]
		if value.Ordinal != 0 && value.Ordinal != i+1 {
			return nil, errors.New("interbase: positional arguments must be ordered")
		}
		if err := normalizeNamedValue(value); err != nil {
			return nil, err
		}
		arg, err := convertNormalizedArgument(value.Value)
		if err != nil {
			return nil, err
		}
		args[i] = arg
	}
	return args, nil
}

func convertQuery(query string, values []driver.NamedValue) ([]argument, error) {
	if err := validateDatabaseSQLQuery(query); err != nil {
		return nil, err
	}
	return convertNamedValues(values)
}

func validateDatabaseSQLQuery(query string) error {
	if err := validateQueryText(query); err != nil {
		return err
	}
	if isSubscriptionSessionControl(query) {
		return errors.New("interbase: subscription session state requires the explicit direct API")
	}
	return nil
}

func validateQueryText(query string) error {
	if strings.IndexByte(query, 0) >= 0 {
		return errors.New("interbase: query cannot contain NUL bytes")
	}
	if len(query) > math.MaxUint16 {
		return errors.New("interbase: query is too long")
	}
	return nil
}

func isSubscriptionSessionControl(query string) bool {
	index, ok := sqlKeywordAt(query, 0, "SET")
	if !ok {
		return false
	}
	index, ok = sqlKeywordAt(query, index, "SUBSCRIPTION")
	return ok
}

func sqlKeywordAt(query string, index int, keyword string) (int, bool) {
	index = skipSQLSpaceAndComments(query, index)
	if index+len(keyword) > len(query) ||
		!strings.EqualFold(query[index:index+len(keyword)], keyword) {
		return index, false
	}
	end := index + len(keyword)
	if end < len(query) && isSQLIdentifierByte(query[end]) {
		return index, false
	}
	return end, true
}

func skipSQLSpaceAndComments(query string, index int) int {
	for index < len(query) {
		switch query[index] {
		case ' ', '\t', '\n', '\r', '\f':
			index++
			continue
		}
		if index+1 >= len(query) {
			return index
		}
		if query[index] == '-' && query[index+1] == '-' {
			index += 2
			for index < len(query) && query[index] != '\n' {
				index++
			}
			continue
		}
		if query[index] == '/' && query[index+1] == '*' {
			end := strings.Index(query[index+2:], "*/")
			if end < 0 {
				return len(query)
			}
			index += end + 4
			continue
		}
		return index
	}
	return index
}

func isSQLIdentifierByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' || value == '_' || value == '$'
}

func validateTxOptions(options driver.TxOptions) error {
	switch options.Isolation {
	case driver.IsolationLevel(sql.LevelDefault), driver.IsolationLevel(sql.LevelReadCommitted),
		driver.IsolationLevel(sql.LevelRepeatableRead), driver.IsolationLevel(sql.LevelSnapshot),
		driver.IsolationLevel(sql.LevelSerializable):
		return nil
	default:
		return fmt.Errorf("interbase: unsupported isolation level %d", options.Isolation)
	}
}

// formatScaledInteger returns an exact decimal representation of an
// InterBase scaled integer. It deliberately returns a string rather than a
// float64 so no decimal precision is lost.
func formatScaledInteger(value int64, scale int16) (string, error) {
	const maxScale = 10000
	if scale < -maxScale || scale > maxScale {
		return "", errors.New("interbase: scaled integer precision is unsupported")
	}

	digits := strconv.FormatInt(value, 10)
	if scale == 0 {
		return digits, nil
	}

	negative := strings.HasPrefix(digits, "-")
	if negative {
		digits = digits[1:]
	}

	var result string
	if scale > 0 {
		result = digits + strings.Repeat("0", int(scale))
	} else {
		fractionDigits := int(-scale)
		if len(digits) <= fractionDigits {
			result = "0." + strings.Repeat("0", fractionDigits-len(digits)) + digits
		} else {
			point := len(digits) - fractionDigits
			result = digits[:point] + "." + digits[point:]
		}
	}
	if negative {
		return "-" + result, nil
	}
	return result, nil
}

type conn struct {
	mu                 sync.Mutex
	native             *nativeConnection
	transactionOptions TransactionOptions
	redactionSecrets   []string
	closed             bool
	statements         map[*stmt]struct{}
	rows               map[*rows]struct{}
	distributed        *DistributedTransaction
	// directBeforeLock is a package-test seam for deterministic cancellation
	// tests while a direct operation waits for the connection mutex.
	directBeforeLock func()
}

func (c *conn) sanitizeError(operation string, err error) error {
	if c == nil {
		return sanitizeError(operation, err)
	}
	return sanitizeError(operation, err, c.redactionSecrets...)
}

func (c *conn) lockDirect() {
	if c.directBeforeLock != nil {
		c.directBeforeLock()
	}
	c.mu.Lock()
}

type tx struct {
	mu   sync.Mutex
	conn *conn
	done bool
}

type stmt struct {
	mu       sync.Mutex
	conn     *conn
	native   *nativeStatement
	numInput int
	closed   bool
}

func (s *stmt) Close() error {
	if s == nil || s.conn == nil {
		return nil
	}
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()
	if s.conn.closed {
		s.mu.Lock()
		s.closed = true
		s.native = nil
		s.mu.Unlock()
		return nil
	}
	var firstErr error
	if s.native != nil {
		for row := range s.conn.rows {
			if row.native != nil && row.native.statement == s.native {
				if err := row.closeLocked(); err != nil {
					firstErr = errors.Join(firstErr, err)
				}
			}
		}
	}
	if err := s.conn.closeStatementLocked(s); err != nil {
		firstErr = errors.Join(firstErr, err)
		s.conn.invalidateLocked(s.conn.sanitizeError("close statement", firstErr))
	}
	if firstErr != nil {
		return s.conn.sanitizeError("close statement", firstErr)
	}
	return nil
}

func (s *stmt) NumInput() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.numInput
}

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}

func (s *stmt) ExecContext(ctx context.Context, values []driver.NamedValue) (driver.Result, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	args, err := convertNamedValues(values)
	if err != nil {
		return nil, err
	}
	conn := s.conn
	if conn == nil {
		return nil, driver.ErrBadConn
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.closed {
		return nil, errStatementClosed
	}
	if conn.native == nil {
		return nil, driver.ErrBadConn
	}
	native, err := s.nativeLocked()
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	affected, err := native.exec(ctx, args)
	releaseNative()
	if err != nil {
		operationErr := conn.sanitizeError("exec prepared statement", err)
		operationErr = classifyNativeOutcome("execute prepared statement", true,
			contextCancellation(ctx), operationErr, nil)
		if conn.native == nil || conn.native.broken() {
			conn.invalidateLocked(operationErr)
		}
		return nil, operationErr
	}
	if err := contextError(ctx); err != nil {
		// Native execution succeeded before cancellation was observed. The
		// database/sql package discards a driver.Result when an error is
		// returned, so report the known success rather than an ambiguous
		// cancellation that could invite an unsafe retry.
		return driver.RowsAffected(affected), nil
	}
	return driver.RowsAffected(affected), nil
}

func (s *stmt) QueryContext(ctx context.Context, values []driver.NamedValue) (driver.Rows, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	args, err := convertNamedValues(values)
	if err != nil {
		return nil, err
	}
	conn := s.conn
	if conn == nil {
		return nil, driver.ErrBadConn
	}
	conn.mu.Lock()
	defer conn.mu.Unlock()
	if conn.closed {
		return nil, errStatementClosed
	}
	if conn.native == nil {
		return nil, driver.ErrBadConn
	}
	native, err := s.nativeLocked()
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	nativeRows, columns, err := native.query(ctx, args)
	releaseNative()
	if err != nil {
		operationErr := conn.sanitizeError("query prepared statement", err)
		operationErr = classifyNativeOutcome("execute prepared query", false,
			contextCancellation(ctx), operationErr, nil)
		if conn.native == nil || conn.native.broken() {
			conn.invalidateLocked(operationErr)
		}
		return nil, operationErr
	}
	result := &rows{
		conn:                conn,
		native:              nativeRows,
		explicitTransaction: nativeRows.explicitTransaction,
		ctx:                 ctx,
		columns:             columns,
		metadata:            append([]columnMetadata(nil), nativeRows.metadata...),
	}
	conn.registerRowsLocked(result)
	return result, nil
}

func (s *stmt) nativeLocked() (*nativeStatement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errStatementClosed
	}
	if s.conn == nil || s.native == nil ||
		(s.native.ptr == nil && s.native.execOverride == nil &&
			s.native.queryOverride == nil) {
		return nil, driver.ErrBadConn
	}
	return s.native, nil
}

func (c *conn) closeStatementLocked(s *stmt) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.native = nil
		s.mu.Unlock()
		delete(c.statements, s)
		return nil
	}
	s.closed = true
	native := s.native
	s.native = nil
	s.mu.Unlock()
	delete(c.statements, s)
	if native == nil {
		return nil
	}
	return native.close()
}

func (c *conn) registerRowsLocked(result *rows) {
	if result == nil {
		return
	}
	if c.rows == nil {
		c.rows = make(map[*rows]struct{})
	}
	c.rows[result] = struct{}{}
}

func (c *conn) unregisterRowsLocked(result *rows) {
	if c == nil || result == nil || c.rows == nil {
		return
	}
	delete(c.rows, result)
	if len(c.rows) == 0 {
		c.rows = nil
	}
}

func (c *conn) closeRowsLocked(predicate func(*rows) bool) error {
	var firstErr error
	for result := range c.rows {
		if predicate != nil && !predicate(result) {
			continue
		}
		if err := result.closeLocked(); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

func (c *conn) closeStatementsLocked() error {
	var firstErr error
	for len(c.statements) != 0 {
		for statement := range c.statements {
			if err := c.closeStatementLocked(statement); err != nil {
				firstErr = errors.Join(firstErr, err)
			}
			break
		}
	}
	return firstErr
}

func namedValues(values []driver.Value) []driver.NamedValue {
	converted := make([]driver.NamedValue, len(values))
	for index, value := range values {
		converted[index] = driver.NamedValue{Ordinal: index + 1, Value: value}
	}
	return converted
}

func (t *tx) Commit() error {
	return t.finish(true)
}

func (t *tx) Rollback() error {
	return t.finish(false)
}

func (t *tx) finish(commit bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return errTransactionDone
	}
	t.done = true

	if t.conn == nil {
		return driver.ErrBadConn
	}
	t.conn.mu.Lock()
	defer t.conn.mu.Unlock()
	if t.conn.closed || t.conn.native == nil {
		return driver.ErrBadConn
	}
	if err := t.conn.closeRowsLocked(func(result *rows) bool {
		return result.explicitTransaction
	}); err != nil {
		return err
	}
	var err error
	if commit {
		err = t.conn.native.commit()
	} else {
		err = t.conn.native.rollback()
	}
	if err != nil {
		action := "rollback transaction"
		if commit {
			action = "commit transaction"
		}
		operationErr := t.conn.sanitizeError(action, err)
		if t.conn.native.broken() {
			t.conn.invalidateLocked(operationErr)
		}
		return operationErr
	}
	return nil
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateDatabaseSQLQuery(query); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.native == nil || c.native.broken() {
		return nil, driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	nativeStatement, err := c.native.prepare(ctx, query)
	if err != nil {
		releaseNative()
	}
	if err != nil {
		operationErr := c.sanitizeError("prepare", err)
		operationErr = classifyNativeOutcome("prepare", false,
			contextCancellation(ctx), operationErr, nil)
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return nil, operationErr
	}
	prepared := &stmt{
		conn:     c,
		native:   nativeStatement,
		numInput: nativeStatement.numInput(),
	}
	releaseNative()
	if c.statements == nil {
		c.statements = make(map[*stmt]struct{})
	}
	c.statements[prepared] = struct{}{}
	return prepared, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeLocked()
}

func (c *conn) closeLocked() error {
	if c.closed {
		return nil
	}
	var firstErr error
	if err := c.closeRowsLocked(nil); err != nil {
		firstErr = err
	}
	if c.closed {
		return firstErr
	}
	if err := c.closeStatementsLocked(); err != nil {
		firstErr = errors.Join(firstErr, c.sanitizeError("close statement", err))
	}
	c.closed = true
	native := c.native
	c.native = nil
	if native != nil {
		if err := native.close(); err != nil {
			firstErr = errors.Join(firstErr, c.sanitizeError("close connection", err))
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return nil
}

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateTxOptions(options); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.distributed != nil {
		return nil, ErrDistributedParticipantManaged
	}
	if c.closed || c.native == nil || c.native.broken() {
		return nil, driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	tpb, err := buildTPB(options, c.transactionOptions)
	if err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	if err := c.native.begin(tpb); err != nil {
		releaseNative()
		operationErr := c.sanitizeError("begin transaction", err)
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return nil, operationErr
	}
	return &tx{conn: c}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, values []driver.NamedValue) (driver.Result, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	args, err := convertQuery(query, values)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.native == nil {
		return nil, driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	affected, err := c.native.exec(ctx, query, args, false)
	releaseNative()
	if err != nil {
		operationErr := c.sanitizeError("exec", err)
		operationErr = classifyNativeOutcome("execute", true,
			contextCancellation(ctx), operationErr, nil)
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return nil, operationErr
	}
	if err := contextError(ctx); err != nil {
		if c.native.broken() {
			c.invalidateLocked(err)
		}
		// Native execution succeeded before cancellation was observed. The
		// database/sql package discards a driver.Result when an error is
		// returned, so report the known success rather than an ambiguous
		// cancellation that could invite an unsafe retry.
		return driver.RowsAffected(affected), nil
	}
	return driver.RowsAffected(affected), nil
}

func (c *conn) CheckNamedValue(value *driver.NamedValue) error {
	return normalizeNamedValue(value)
}

func (c *conn) QueryContext(ctx context.Context, query string, values []driver.NamedValue) (driver.Rows, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	args, err := convertQuery(query, values)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.native == nil {
		return nil, driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	nativeRows, columns, err := c.native.query(ctx, query, args, false)
	releaseNative()
	if err != nil {
		operationErr := c.sanitizeError("query", err)
		operationErr = classifyNativeOutcome("query", false,
			contextCancellation(ctx), operationErr, nil)
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return nil, operationErr
	}
	result := &rows{
		conn:                c,
		native:              nativeRows,
		explicitTransaction: nativeRows.explicitTransaction,
		ctx:                 ctx,
		columns:             columns,
		metadata:            append([]columnMetadata(nil), nativeRows.metadata...),
	}
	c.registerRowsLocked(result)
	if err := contextError(ctx); err != nil {
		closeErr := result.abortLocked(err)
		if closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	return result, nil
}

func (c *conn) Ping(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}

	result, err := c.QueryContext(ctx, "SELECT 1 FROM RDB$DATABASE", nil)
	if err != nil {
		return err
	}
	values := make([]driver.Value, len(result.Columns()))
	nextErr := result.Next(values)
	closeErr := result.Close()
	if nextErr != nil && !errors.Is(nextErr, io.EOF) {
		if closeErr != nil {
			return errors.Join(nextErr, closeErr)
		}
		return nextErr
	}
	if closeErr != nil {
		return closeErr
	}
	if errors.Is(nextErr, io.EOF) {
		return errors.New("interbase: ping query returned no row")
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	return nil
}

// IsValid lets database/sql discard unusable connections without replaying a
// query whose original operation error must be preserved.
func (c *conn) IsValid() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && c.native != nil && !c.native.broken()
}

func (c *conn) invalidateLocked(cause error) {
	if c.closed {
		return
	}
	if cause == nil {
		cause = errConnectionInvalidated
	}
	for result := range c.rows {
		result.terminalErr = cause
		result.closed = true
		result.native = nil
	}
	c.rows = nil
	for statement := range c.statements {
		statement.mu.Lock()
		statement.closed = true
		statement.native = nil
		statement.mu.Unlock()
	}
	c.statements = nil
	c.closed = true
	native := c.native
	c.native = nil
	if native != nil {
		_ = native.close()
	}
}

type rows struct {
	conn                *conn
	native              *nativeCursor
	explicitTransaction bool
	ctx                 context.Context
	columns             []string
	metadata            []columnMetadata
	closed              bool
	terminalErr         error
}

func (r *rows) Columns() []string {
	return append([]string(nil), r.columns...)
}

func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	if r == nil || index < 0 || index >= len(r.metadata) {
		return ""
	}
	return r.metadata[index].databaseTypeName
}

func (r *rows) ColumnTypeLength(index int) (int64, bool) {
	if r == nil || index < 0 || index >= len(r.metadata) {
		return 0, false
	}
	metadata := r.metadata[index]
	return metadata.length, metadata.hasLength
}

func (r *rows) ColumnTypeNullable(index int) (bool, bool) {
	if r == nil || index < 0 || index >= len(r.metadata) {
		return false, false
	}
	metadata := r.metadata[index]
	return metadata.nullable, metadata.hasNullable
}

func (r *rows) ColumnTypePrecisionScale(index int) (int64, int64, bool) {
	if r == nil || index < 0 || index >= len(r.metadata) {
		return 0, 0, false
	}
	metadata := r.metadata[index]
	return metadata.precision, metadata.scale, metadata.hasPrecision
}

func (r *rows) ColumnTypeScanType(index int) reflect.Type {
	if r == nil || index < 0 || index >= len(r.metadata) {
		return reflect.TypeOf((*any)(nil)).Elem()
	}
	return r.metadata[index].scanType
}

func (r *rows) Next(dest []driver.Value) error {
	r.conn.mu.Lock()
	defer r.conn.mu.Unlock()

	if r.terminalErr != nil {
		return r.terminalErr
	}
	if r.closed {
		return io.EOF
	}
	if r.conn.closed || r.conn.native == nil {
		r.closed = true
		r.native = nil
		return driver.ErrBadConn
	}
	if err := contextError(r.ctx); err != nil {
		return errors.Join(err, r.abortLocked(err))
	}
	if len(dest) < len(r.columns) {
		err := errors.New("interbase: destination has fewer values than result columns")
		return errors.Join(err, r.abortLocked(err))
	}
	releaseNative, err := enterNativeContext(r.ctx)
	if err != nil {
		return errors.Join(err, r.abortLocked(err))
	}
	defer releaseNative()

	hasRow, err := r.native.next(r.ctx)
	if err != nil {
		releaseNative()
		operationErr := r.conn.sanitizeError("fetch", err)
		operationErr = classifyNativeOutcome("fetch row", false,
			contextCancellation(r.ctx), operationErr, nil)
		return errors.Join(operationErr, r.abortLocked(operationErr))
	}
	if !hasRow {
		releaseNative()
		if err := r.closeLocked(); err != nil {
			return err
		}
		return io.EOF
	}

	for i := range r.columns {
		indicator, err := r.native.indicator(i)
		if err != nil {
			releaseNative()
			operationErr := r.conn.sanitizeError("read result indicator", err)
			return errors.Join(operationErr, r.abortLocked(operationErr))
		}
		if err := validateSQLRowIndicator(indicator); err != nil {
			releaseNative()
			return errors.Join(err, r.abortLocked(err))
		}
		value, err := r.native.value(i)
		if err != nil {
			releaseNative()
			operationErr := r.conn.sanitizeError("decode result", err)
			return errors.Join(operationErr, r.abortLocked(operationErr))
		}
		dest[i] = value
	}
	releaseNative()
	return nil
}

func (r *rows) Close() error {
	r.conn.mu.Lock()
	defer r.conn.mu.Unlock()
	if r.ctx != nil && r.ctx.Err() != nil {
		return r.abortLocked(r.ctx.Err())
	}
	return r.closeLocked()
}

func (r *rows) closeLocked() error {
	return r.finishLocked(false, nil)
}

func (r *rows) abortLocked(cause error) error {
	return r.finishLocked(true, cause)
}

func (r *rows) finishLocked(abort bool, cause error) error {
	if r.closed {
		r.conn.unregisterRowsLocked(r)
		return nil
	}
	r.closed = true
	r.conn.unregisterRowsLocked(r)
	if r.conn.closed {
		r.native = nil
		return nil
	}
	native := r.native
	r.native = nil
	if native == nil {
		return nil
	}
	close := native.close
	if abort {
		close = native.abort
	}
	if err := close(); err != nil {
		cleanupErr := r.conn.sanitizeError("close cursor", err)
		if cause != nil {
			cleanupErr = errors.Join(cause, cleanupErr)
		}
		r.terminalErr = cleanupErr
		r.conn.invalidateLocked(cleanupErr)
		return cleanupErr
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("interbase: nil context")
	}
	return ctx.Err()
}

func configSecrets(cfg Config) []string {
	return []string{
		cfg.Database,
		cfg.Host,
		cfg.User,
		cfg.Password,
		cfg.Role,
		cfg.EncryptedPassword,
		cfg.SystemEncryptionPassword,
		cfg.TLS.ServerPublicFile,
		cfg.TLS.ClientCertFile,
		cfg.TLS.ClientPassPhrase,
		cfg.TLS.ClientPassPhraseFile,
		cfg.TLS.ServerPublicPath,
	}
}

// Error describes a failure returned by the InterBase native client.
//
// Use errors.As to inspect native failures without depending on their rendered
// text. SQLCode and NativeCode are zero when the failure was detected before
// the client returned a status vector.
type Error struct {
	Operation  string
	SQLCode    int
	NativeCode int64
	Message    string
	cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return "interbase: native operation failed"
	}
	operation := e.Operation
	if operation == "" {
		operation = "native operation"
	}
	if e.SQLCode != 0 || e.NativeCode != 0 {
		if e.Message != "" {
			return fmt.Sprintf("interbase: %s failed (SQLCODE %d, native status %d): %s",
				operation, e.SQLCode, e.NativeCode, e.Message)
		}
		return fmt.Sprintf("interbase: %s failed (SQLCODE %d, native status %d)",
			operation, e.SQLCode, e.NativeCode)
	}
	if e.Message == "" {
		return fmt.Sprintf("interbase: %s failed", operation)
	}
	return fmt.Sprintf("interbase: %s: %s", operation, e.Message)
}

func (e *Error) Is(target error) bool {
	return e != nil && e.cause != nil && errors.Is(e.cause, target)
}

func parseNativeError(message string) error {
	const marker = " failed (SQLCODE "
	markerStart := strings.Index(message, marker)
	if markerStart < 0 {
		return &Error{Message: message}
	}
	remainder := message[markerStart+len(marker):]
	statusMarker := ", native status "
	statusStart := strings.Index(remainder, statusMarker)
	if statusStart < 0 {
		return &Error{Message: message}
	}
	close := strings.IndexByte(remainder[statusStart+len(statusMarker):], ')')
	if close < 0 {
		return &Error{Message: message}
	}
	close += statusStart + len(statusMarker)
	sqlCode, sqlErr := strconv.Atoi(strings.TrimSpace(remainder[:statusStart]))
	nativeCode, nativeErr := strconv.ParseInt(
		strings.TrimSpace(remainder[statusStart+len(statusMarker):close]), 10, 64)
	if sqlErr != nil || nativeErr != nil {
		return &Error{Message: message}
	}

	detail := strings.TrimSpace(remainder[close+1:])
	detail = strings.TrimPrefix(detail, ":")
	detail = strings.TrimSpace(detail)
	return &Error{
		Operation:  message[:markerStart],
		SQLCode:    sqlCode,
		NativeCode: nativeCode,
		Message:    detail,
	}
}

func sanitizeError(operation string, err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	redacted := sanitizeErrorTree(err, secrets...)
	if _, ok := err.(*Error); ok {
		sanitized := *redacted.(*Error)
		if operation != "" {
			sanitized.Operation = operation
		}
		return &sanitized
	}
	if operation == "" {
		return redacted
	}
	return &redactedError{
		message:  fmt.Sprintf("interbase: %s: %s", operation, redacted.Error()),
		cause:    redacted,
		original: err,
	}
}

type redactedError struct {
	message  string
	cause    error
	original error
}

func (e *redactedError) Error() string { return e.message }

func (e *redactedError) Unwrap() error { return e.cause }

func (e *redactedError) Is(target error) bool {
	return e.original != nil && errors.Is(e.original, target)
}

type redactedJoinError struct {
	message  string
	causes   []error
	original error
}

func (e *redactedJoinError) Error() string { return e.message }

func (e *redactedJoinError) Unwrap() []error { return e.causes }

func (e *redactedJoinError) Is(target error) bool {
	return e.original != nil && errors.Is(e.original, target)
}

type redactedLeafError struct {
	message  string
	original error
}

func (e *redactedLeafError) Error() string { return e.message }

func (e *redactedLeafError) Is(target error) bool {
	return e.original != nil && errors.Is(e.original, target)
}

func sanitizeErrorTree(err error, secrets ...string) error {
	if nativeErr, ok := err.(*Error); ok {
		sanitized := *nativeErr
		sanitized.Message = redactSecrets(sanitized.Message, secrets...)
		if sanitized.Message == "" {
			sanitized.Message = "native operation failed"
		}
		sanitized.cause = nativeErr
		return &sanitized
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		sanitizedChildren := make([]error, len(children))
		for index, child := range children {
			sanitizedChildren[index] = sanitizeErrorTree(child, secrets...)
		}
		message := make([]string, len(sanitizedChildren))
		for index, child := range sanitizedChildren {
			message[index] = child.Error()
		}
		if len(message) == 0 {
			return &redactedJoinError{
				message:  redactSecrets(err.Error(), secrets...),
				causes:   sanitizedChildren,
				original: err,
			}
		}
		return &redactedJoinError{
			message:  strings.Join(message, "\n"),
			causes:   sanitizedChildren,
			original: err,
		}
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		if cause == nil {
			return &redactedLeafError{
				message:  redactSecrets(err.Error(), secrets...),
				original: err,
			}
		}
		return &redactedError{
			message:  redactSecrets(err.Error(), secrets...),
			cause:    sanitizeErrorTree(cause, secrets...),
			original: err,
		}
	}
	return &redactedLeafError{
		message:  redactSecrets(err.Error(), secrets...),
		original: err,
	}
}

type redactionRange struct {
	start int
	end   int
}

func redactSecrets(message string, secrets ...string) string {
	if message == "" {
		return message
	}
	ranges := make([]redactionRange, 0, len(secrets))
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		searchStart := 0
		for searchStart < len(message) {
			relativeStart := strings.Index(message[searchStart:], secret)
			if relativeStart < 0 {
				break
			}
			start := searchStart + relativeStart
			ranges = append(ranges, redactionRange{start: start, end: start + len(secret)})
			searchStart = start + 1
		}
	}
	if len(ranges) == 0 {
		return message
	}
	sort.Slice(ranges, func(left, right int) bool {
		if ranges[left].start == ranges[right].start {
			return ranges[left].end > ranges[right].end
		}
		return ranges[left].start < ranges[right].start
	})
	merged := ranges[:1]
	for _, current := range ranges[1:] {
		last := &merged[len(merged)-1]
		if current.start <= last.end {
			if current.end > last.end {
				last.end = current.end
			}
			continue
		}
		merged = append(merged, current)
	}
	var redacted strings.Builder
	redacted.Grow(len(message))
	lastEnd := 0
	for _, span := range merged {
		redacted.WriteString(message[lastEnd:span.start])
		redacted.WriteString("[redacted]")
		lastEnd = span.end
	}
	redacted.WriteString(message[lastEnd:])
	return redacted.String()
}

var (
	_ driver.Driver                         = driverInstance
	_ driver.DriverContext                  = driverInstance
	_ driver.Conn                           = (*conn)(nil)
	_ driver.Pinger                         = (*conn)(nil)
	_ driver.Validator                      = (*conn)(nil)
	_ driver.ExecerContext                  = (*conn)(nil)
	_ driver.QueryerContext                 = (*conn)(nil)
	_ driver.NamedValueChecker              = (*conn)(nil)
	_ driver.ConnPrepareContext             = (*conn)(nil)
	_ driver.ConnBeginTx                    = (*conn)(nil)
	_ driver.Rows                           = (*rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*rows)(nil)
	_ driver.RowsColumnTypeLength           = (*rows)(nil)
	_ driver.RowsColumnTypeNullable         = (*rows)(nil)
	_ driver.RowsColumnTypePrecisionScale   = (*rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*rows)(nil)
)
