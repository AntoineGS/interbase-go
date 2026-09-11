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
)

// Config contains the connection settings for an InterBase attachment.
//
// The proof of concept uses Dialect 1 and the UTF8 character set for every
// connection. Passwords are sent only while opening an attachment (and are
// retained only for error redaction); they are never included in driver
// errors.
type Config struct {
	Database string
	User     string
	Password string
}

var (
	errUnsupportedPrepare = errors.New("prepared statements are unsupported by the InterBase proof of concept")
	errUnsupportedExec    = errors.New("exec is unsupported by the read-only InterBase proof of concept")
	errUnsupportedBegin   = errors.New("transactions are unsupported; each query owns a read-only transaction")
	errConnectorOnly      = errors.New("DSN-based opening is unsupported; use NewConnector")
)

// NewConnector validates cfg and returns a database/sql connector. Open a
// database with sql.OpenDB(NewConnector(...)).
func NewConnector(cfg Config) (driver.Connector, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
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
	return nil
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
)

type argument struct {
	kind         argumentKind
	stringValue  string
	int64Value   int64
	float64Value float64
	boolValue    bool
}

func convertArgument(value any) (argument, error) {
	switch value := value.(type) {
	case nil:
		return argument{kind: argumentNull}, nil
	case string:
		if len(value) > math.MaxInt16 {
			return argument{}, errors.New("interbase: string argument exceeds the SQLDA length limit")
		}
		return argument{kind: argumentString, stringValue: value}, nil
	case int64:
		return argument{kind: argumentInt64, int64Value: value}, nil
	case float64:
		return argument{kind: argumentFloat64, float64Value: value}, nil
	case bool:
		return argument{kind: argumentBool, boolValue: value}, nil
	default:
		return argument{}, fmt.Errorf("interbase: argument type %T is unsupported", value)
	}
}

func convertNamedValues(values []driver.NamedValue) ([]argument, error) {
	if len(values) > math.MaxInt16 {
		return nil, errors.New("interbase: too many query arguments")
	}

	args := make([]argument, len(values))
	for i, value := range values {
		if value.Name != "" {
			return nil, errors.New("interbase: named arguments are unsupported; use positional '?' arguments")
		}
		if value.Ordinal != 0 && value.Ordinal != i+1 {
			return nil, errors.New("interbase: positional arguments must be ordered")
		}
		arg, err := convertArgument(value.Value)
		if err != nil {
			return nil, err
		}
		args[i] = arg
	}
	return args, nil
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
	mu       sync.Mutex
	native   *nativeConnection
	database string
	user     string
	password string
	closed   bool
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return nil, errUnsupportedPrepare
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return nil, errUnsupportedPrepare
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true
	native := c.native
	c.native = nil
	if native == nil {
		return nil
	}
	if err := native.close(); err != nil {
		return sanitizeError("close connection", err, c.database, c.user, c.password)
	}
	return nil
}

func (c *conn) Begin() (driver.Tx, error) {
	return nil, errUnsupportedBegin
}

func (c *conn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return nil, errUnsupportedBegin
}

func (c *conn) ExecContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	return nil, errUnsupportedExec
}

func (c *conn) CheckNamedValue(value *driver.NamedValue) error {
	if value == nil {
		return errors.New("interbase: nil named value")
	}
	if value.Name != "" {
		return errors.New("interbase: named arguments are unsupported; use positional '?' arguments")
	}
	_, err := convertArgument(value.Value)
	return err
}

func (c *conn) QueryContext(ctx context.Context, query string, values []driver.NamedValue) (driver.Rows, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	args, err := convertNamedValues(values)
	if err != nil {
		return nil, err
	}
	if strings.IndexByte(query, 0) >= 0 {
		return nil, errors.New("interbase: query cannot contain NUL bytes")
	}
	if len(query) > math.MaxUint16 {
		return nil, errors.New("interbase: query is too long")
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
		return nil
	}
	r.closed = true
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
