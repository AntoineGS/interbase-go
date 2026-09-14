package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config contains the connection settings for an InterBase attachment.
//
// The driver uses Dialect 1. Charset is the InterBase attachment character
// set; an empty value selects UTF8. Passwords are sent only while opening an
// attachment (and are retained only for error redaction); they are never
// included in driver errors.
type Config struct {
	Database string
	User     string
	Password string
	Charset  string
}

var (
	errConnectorOnly   = errors.New("DSN-based opening is unsupported; use NewConnector")
	errTransactionDone = errors.New("interbase: transaction is already completed")
	errStatementClosed = errors.New("interbase: prepared statement is closed")
)

// NewConnector validates cfg and returns a database/sql connector. Open a
// database with sql.OpenDB(NewConnector(...)).
func NewConnector(cfg Config) (driver.Connector, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	charset, err := normalizeCharset(cfg.Charset)
	if err != nil {
		return nil, err
	}
	cfg.Charset = charset
	return &connector{cfg: cfg}, nil
}

func validateConfig(cfg Config) error {
	if cfg.Database == "" {
		return errors.New("interbase: database is required")
	}
	if cfg.User == "" {
		return errors.New("interbase: user is required")
	}
	if strings.IndexByte(cfg.Database, 0) >= 0 || strings.IndexByte(cfg.User, 0) >= 0 || strings.IndexByte(cfg.Password, 0) >= 0 {
		return errors.New("interbase: connection settings cannot contain NUL bytes")
	}
	if len(cfg.Database) > math.MaxInt16 {
		return errors.New("interbase: database name is too long")
	}
	if len(cfg.User) > math.MaxUint8 || len(cfg.Password) > math.MaxUint8 {
		return errors.New("interbase: credential is too long")
	}
	if _, err := normalizeCharset(cfg.Charset); err != nil {
		return err
	}
	return nil
}

func normalizeCharset(charset string) (string, error) {
	switch strings.ToUpper(charset) {
	case "":
		return "UTF8", nil
	case "UTF8", "WIN1250":
		return strings.ToUpper(charset), nil
	default:
		return "", fmt.Errorf("interbase: unsupported character set %q", charset)
	}
}

type connector struct {
	cfg Config
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	native, err := openNative(c.cfg)
	if err != nil {
		return nil, sanitizeError("connect", err, c.cfg.Database, c.cfg.User, c.cfg.Password)
	}
	if err := contextError(ctx); err != nil {
		closeErr := native.close()
		if closeErr != nil {
			return nil, errors.Join(err, sanitizeError("close connection", closeErr, c.cfg.Database, c.cfg.User, c.cfg.Password))
		}
		return nil, err
	}

	return &conn{
		native:   native,
		database: c.cfg.Database,
		user:     c.cfg.User,
		password: c.cfg.Password,
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
)

type argument struct {
	kind         argumentKind
	stringValue  string
	int64Value   int64
	float64Value float64
	boolValue    bool
	timeValue    time.Time
	bytesValue   []byte
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
	if err := validateQueryText(query); err != nil {
		return nil, err
	}
	return convertNamedValues(values)
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

// database/sql's standard isolation constants are shared with driver.TxOptions;
// InterBase supports its default and read-committed levels for this driver.
const (
	isolationLevelDefault       driver.IsolationLevel = 0
	isolationLevelReadCommitted driver.IsolationLevel = 2
)

func validateTxOptions(options driver.TxOptions) error {
	if options.Isolation != isolationLevelDefault &&
		options.Isolation != isolationLevelReadCommitted {
		return fmt.Errorf("interbase: unsupported isolation level %d", options.Isolation)
	}
	return nil
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
	mu         sync.Mutex
	native     *nativeConnection
	database   string
	user       string
	password   string
	closed     bool
	statements map[*stmt]struct{}
	activeRows *rows
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
	if s.native != nil && s.conn.activeRows != nil && s.conn.activeRows.native != nil &&
		s.conn.activeRows.native.statement == s.native {
		firstErr = s.conn.activeRows.closeLocked()
	}
	if err := s.conn.closeStatementLocked(s); err != nil {
		firstErr = errors.Join(firstErr, err)
		s.conn.invalidateLocked()
	}
	if firstErr != nil {
		return sanitizeError("close statement", firstErr, s.conn.database, s.conn.password)
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
	affected, err := native.exec(args)
	if err != nil {
		if conn.native == nil || conn.native.broken() {
			conn.invalidateLocked()
		}
		return nil, sanitizeError("exec prepared statement", err, conn.database, conn.password)
	}
	if err := contextError(ctx); err != nil {
		return nil, err
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
	nativeRows, columns, err := native.query(args)
	if err != nil {
		if conn.native == nil || conn.native.broken() {
			conn.invalidateLocked()
		}
		return nil, sanitizeError("query prepared statement", err, conn.database, conn.password)
	}
	result := &rows{
		conn:    conn,
		native:  nativeRows,
		ctx:     ctx,
		columns: columns,
	}
	conn.activeRows = result
	if err := contextError(ctx); err != nil {
		closeErr := result.closeLocked()
		if closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	return result, nil
}

func (s *stmt) nativeLocked() (*nativeStatement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errStatementClosed
	}
	if s.conn == nil || s.native == nil || s.native.ptr == nil {
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
	if t.conn.activeRows != nil {
		if err := t.conn.activeRows.closeLocked(); err != nil {
			return err
		}
	}
	var err error
	if commit {
		err = t.conn.native.commit()
	} else {
		err = t.conn.native.rollback()
	}
	if err != nil {
		if t.conn.native.broken() {
			t.conn.invalidateLocked()
		}
		action := "rollback transaction"
		if commit {
			action = "commit transaction"
		}
		return sanitizeError(action, err, t.conn.database, t.conn.password)
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
	if err := validateQueryText(query); err != nil {
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
	nativeStatement, err := c.native.prepare(query)
	if err != nil {
		if c.native.broken() {
			c.invalidateLocked()
		}
		return nil, sanitizeError("prepare", err, c.database, c.password)
	}
	prepared := &stmt{
		conn:     c,
		native:   nativeStatement,
		numInput: nativeStatement.numInput(),
	}
	if c.statements == nil {
		c.statements = make(map[*stmt]struct{})
	}
	c.statements[prepared] = struct{}{}
	return prepared, nil
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	var firstErr error
	if c.activeRows != nil {
		firstErr = c.activeRows.closeLocked()
	}
	if c.closed {
		return firstErr
	}
	if err := c.closeStatementsLocked(); err != nil {
		firstErr = errors.Join(firstErr, sanitizeError("close statement", err, c.database, c.password))
	}
	c.closed = true
	native := c.native
	c.native = nil
	if native != nil {
		if err := native.close(); err != nil {
			firstErr = errors.Join(firstErr, sanitizeError("close connection", err, c.database, c.user, c.password))
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
	if c.closed || c.native == nil || c.native.broken() {
		return nil, driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := c.native.begin(options.ReadOnly); err != nil {
		if c.native.broken() {
			c.invalidateLocked()
		}
		return nil, sanitizeError("begin transaction", err, c.database, c.password)
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
	affected, err := c.native.exec(query, args)
	if err != nil {
		if c.native.broken() {
			c.invalidateLocked()
		}
		return nil, sanitizeError("exec", err, c.database, c.password)
	}
	if err := contextError(ctx); err != nil {
		if c.native.broken() {
			c.invalidateLocked()
		}
		return nil, err
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

	nativeRows, columns, err := c.native.query(query, args)
	if err != nil {
		if c.native.broken() {
			c.invalidateLocked()
		}
		return nil, sanitizeError("query", err, c.database, c.password)
	}
	result := &rows{
		conn:    c,
		native:  nativeRows,
		ctx:     ctx,
		columns: columns,
	}
	c.activeRows = result
	if err := contextError(ctx); err != nil {
		closeErr := result.closeLocked()
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

func (c *conn) invalidateLocked() {
	if c.closed {
		return
	}
	if c.activeRows != nil {
		c.activeRows.closed = true
		c.activeRows.native = nil
		c.activeRows = nil
	}
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
	conn    *conn
	native  *nativeCursor
	ctx     context.Context
	columns []string
	closed  bool
}

func (r *rows) Columns() []string {
	return append([]string(nil), r.columns...)
}

func (r *rows) Next(dest []driver.Value) error {
	r.conn.mu.Lock()
	defer r.conn.mu.Unlock()

	if r.closed {
		return io.EOF
	}
	if r.conn.closed || r.conn.native == nil {
		r.closed = true
		r.native = nil
		return driver.ErrBadConn
	}
	if err := contextError(r.ctx); err != nil {
		return errors.Join(err, r.closeLocked())
	}
	if len(dest) < len(r.columns) {
		return errors.New("interbase: destination has fewer values than result columns")
	}

	hasRow, err := r.native.next()
	if err != nil {
		operationErr := sanitizeError("fetch", err, r.conn.database, r.conn.password)
		return errors.Join(operationErr, r.closeLocked())
	}
	if err := contextError(r.ctx); err != nil {
		return errors.Join(err, r.closeLocked())
	}
	if !hasRow {
		if err := r.closeLocked(); err != nil {
			return err
		}
		return io.EOF
	}

	for i := range r.columns {
		value, err := r.native.value(i)
		if err != nil {
			operationErr := sanitizeError("decode result", err, r.conn.database, r.conn.password)
			return errors.Join(operationErr, r.closeLocked())
		}
		dest[i] = value
	}
	if err := contextError(r.ctx); err != nil {
		return errors.Join(err, r.closeLocked())
	}
	return nil
}

func (r *rows) Close() error {
	r.conn.mu.Lock()
	defer r.conn.mu.Unlock()
	return r.closeLocked()
}

func (r *rows) closeLocked() error {
	if r.closed {
		if r.conn.activeRows == r {
			r.conn.activeRows = nil
		}
		return nil
	}
	r.closed = true
	if r.conn.activeRows == r {
		r.conn.activeRows = nil
	}
	if r.conn.closed {
		r.native = nil
		return nil
	}
	native := r.native
	r.native = nil
	if native == nil {
		return nil
	}
	if err := native.close(); err != nil {
		r.conn.invalidateLocked()
		return sanitizeError("close cursor", err, r.conn.database, r.conn.password)
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("interbase: nil context")
	}
	return ctx.Err()
}

func sanitizeError(operation string, err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if message == "" {
		message = "native operation failed"
	}
	return fmt.Errorf("interbase: %s: %s", operation, message)
}

var (
	_ driver.Driver             = driverInstance
	_ driver.DriverContext      = driverInstance
	_ driver.Conn               = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.Rows               = (*rows)(nil)
)
