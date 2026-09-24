package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
)

// Introspector is implemented by the driver connection passed to
// (*sql.Conn).Raw. The value must not be retained beyond the callback.
type Introspector interface {
	Diagnostics(ctx context.Context) (DatabaseDiagnostics, error)
	Plan(ctx context.Context, query string) (string, error)
}

// InputDescriptor is one positional input parameter description returned by
// the server for a prepared statement.
type InputDescriptor struct {
	Kind                      string
	Subtype, Scale, Precision int
	Nullable                  bool
}

// InputDescriber is an optional interface implemented by driver connections
// that can describe prepared input parameters.
type InputDescriber interface {
	DescribeInputs(ctx context.Context, query string) ([]InputDescriptor, error)
}

// StatementDescriptor classifies a prepared statement without running it.
// The application must reject Kind "unsupported" before attempting execution.
type StatementDescriptor struct {
	Kind        string
	ReturnsRows bool
	Mutating    bool
	InputCount  int
}

// StatementDescriber is an optional extension for driver connections.
type StatementDescriber interface {
	DescribeStatement(ctx context.Context, query string) (StatementDescriptor, error)
}

var _ Introspector = (*conn)(nil)
var _ InputDescriber = (*conn)(nil)
var _ StatementDescriber = (*conn)(nil)

// ErrNotInterBaseConn reports a connection that does not belong to this driver.
var ErrNotInterBaseConn = errors.New("interbase: connection is not an InterBase connection")

// Diagnostics returns server, database, and linked client diagnostics for the
// attachment behind a pooled database/sql connection, without opening a second
// native attachment. SQLDialect is the dialect the server reports for this
// attachment, not an echo of Config.Dialect.
//
// The driver error is returned unchanged. A connection that does not belong to
// this driver, including a nil one, returns ErrNotInterBaseConn; a conn that
// has already been closed returns sql.ErrConnDone from Raw.
func Diagnostics(ctx context.Context, conn *sql.Conn) (DatabaseDiagnostics, error) {
	if conn == nil {
		return DatabaseDiagnostics{}, ErrNotInterBaseConn
	}
	var result DatabaseDiagnostics
	if err := conn.Raw(func(driverConn any) error {
		introspector, ok := driverConn.(Introspector)
		if !ok {
			return ErrNotInterBaseConn
		}
		diagnostics, err := introspector.Diagnostics(ctx)
		if err != nil {
			return err
		}
		result = diagnostics
		return nil
	}); err != nil {
		return DatabaseDiagnostics{}, err
	}
	return result, nil
}

// Diagnostics returns server, database, and linked client diagnostics for the
// attachment behind this connection. It is the shared implementation used by
// both Attachment.Diagnostics and the pooled Diagnostics helper.
func (c *conn) Diagnostics(ctx context.Context) (DatabaseDiagnostics, error) {
	if err := contextError(ctx); err != nil {
		return DatabaseDiagnostics{}, err
	}
	c.lockDirect()
	defer c.mu.Unlock()
	if c.closed || c.native == nil || c.native.broken() {
		return DatabaseDiagnostics{}, driver.ErrBadConn
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	defer releaseNative()
	readInfo := func(code byte) (InfoItem, error) {
		if err := contextError(ctx); err != nil {
			return InfoItem{}, err
		}
		response, err := c.native.databaseInfo(code)
		if err != nil {
			return InfoItem{}, c.sanitizeError("database diagnostics", err)
		}
		item, err := parseInfoItem(response, code)
		if err != nil {
			return InfoItem{}, err
		}
		if err := contextError(ctx); err != nil {
			return InfoItem{}, err
		}
		return item, nil
	}
	serverVersionItem, err := readInfo(InfoDatabaseVersion)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	serverVersion, err := serverVersionItem.Text()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsItem, err := readInfo(InfoDatabaseODSVersion)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsVersion, err := odsItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsMinorItem, err := readInfo(InfoDatabaseODSMinorVersion)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsMinorVersion, err := odsMinorItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	pageSizeItem, err := readInfo(InfoDatabasePageSize)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	pageSize, err := pageSizeItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	dialectItem, err := readInfo(InfoDatabaseSQLDialect)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	dialect, err := dialectItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	readOnlyItem, err := readInfo(InfoDatabaseReadOnly)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	readOnly, err := readOnlyItem.Bool()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	if err := contextError(ctx); err != nil {
		return DatabaseDiagnostics{}, err
	}
	clientVersion, err := c.native.clientVersion()
	if err != nil {
		return DatabaseDiagnostics{}, c.sanitizeError("client diagnostics", err)
	}
	return DatabaseDiagnostics{
		ClientVersion:   clientVersion,
		ServerVersion:   serverVersion,
		DatabaseVersion: fmt.Sprintf("%d.%d", odsVersion, odsMinorVersion),
		ODSVersion:      odsVersion,
		ODSMinorVersion: odsMinorVersion,
		PageSize:        pageSize,
		SQLDialect:      dialect,
		ReadOnly:        readOnly,
	}, nil
}

// Plan prepares query on the attachment behind a pooled database/sql
// connection and returns its server-generated plan. It never executes the
// statement, for any statement type, and it opens no second native attachment.
//
// Native prepare uses the connection's active explicit transaction when one
// was started with BeginTx on the same *sql.Conn and has not completed, and
// otherwise uses a read-only transaction that begins and ends inside the
// native prepare call. A valid DML statement may return an empty plan string,
// and an empty plan never implies that the statement ran.
//
// The driver error is returned unchanged. A connection that does not belong to
// this driver, including a nil one, returns ErrNotInterBaseConn; a conn that
// has already been closed returns sql.ErrConnDone from Raw.
func Plan(ctx context.Context, conn *sql.Conn, query string) (string, error) {
	if conn == nil {
		return "", ErrNotInterBaseConn
	}
	var result string
	if err := conn.Raw(func(driverConn any) error {
		introspector, ok := driverConn.(Introspector)
		if !ok {
			return ErrNotInterBaseConn
		}
		plan, err := introspector.Plan(ctx, query)
		if err != nil {
			return err
		}
		result = plan
		return nil
	}); err != nil {
		return "", err
	}
	return result, nil
}

// DescribeInputs describes the positional input parameters for query using the
// attachment behind a pooled database/sql connection. It never executes the
// statement or opens another native attachment.
//
// The driver error is returned unchanged. A connection that does not belong to
// this driver, including a nil one, returns ErrNotInterBaseConn; a conn that
// has already been closed returns sql.ErrConnDone from Raw.
func DescribeInputs(ctx context.Context, conn *sql.Conn, query string) ([]InputDescriptor, error) {
	if conn == nil {
		return nil, ErrNotInterBaseConn
	}
	var result []InputDescriptor
	if err := conn.Raw(func(driverConn any) error {
		describer, ok := driverConn.(InputDescriber)
		if !ok {
			return ErrNotInterBaseConn
		}
		descriptors, err := describer.DescribeInputs(ctx, query)
		if err != nil {
			return err
		}
		result = descriptors
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

// DescribeStatement inspects a statement on the supplied pinned connection.
// It never executes the SQL, changes the caller's transaction, or keeps the
// driver connection beyond the Raw callback.
func DescribeStatement(ctx context.Context, conn *sql.Conn, query string) (StatementDescriptor, error) {
	if conn == nil {
		return StatementDescriptor{}, ErrNotInterBaseConn
	}
	var result StatementDescriptor
	if err := conn.Raw(func(driverConn any) error {
		describer, ok := driverConn.(StatementDescriber)
		if !ok {
			return ErrNotInterBaseConn
		}
		var err error
		result, err = describer.DescribeStatement(ctx, query)
		return err
	}); err != nil {
		return StatementDescriptor{}, err
	}
	return result, nil
}

// Plan prepares query and returns its server-generated plan without executing
// it, using the attachment behind this connection. The prepared statement is
// never registered with the connection and is always closed before returning.
//
// Native prepare selects the connection's active explicit transaction when
// BeginTx started one and has not completed it, and otherwise starts, uses,
// and completes its own read-only transaction inside the prepare call. Plan
// never begins or completes a caller-owned transaction.
func (c *conn) Plan(ctx context.Context, query string) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if err := validateDatabaseSQLQuery(query); err != nil {
		return "", err
	}
	c.lockDirect()
	defer c.mu.Unlock()
	// Unreachable from the pool by design: c.distributed is only ever assigned
	// through BeginDistributed, which accepts explicit Attachment participants
	// (distributed.go:216). The guard exists because this method lives on the
	// shared *conn. Do not treat it as a live path.
	if c.distributed != nil {
		return "", ErrDistributedParticipantManaged
	}
	if c.closed || c.native == nil || c.native.broken() {
		return "", driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return "", err
	}
	var plan string
	if err := c.inspectPreparedStatementLocked(ctx, query, "plan", func(statement *nativeStatement) error {
		var err error
		plan, err = statement.plan()
		return err
	}); err != nil {
		return "", err
	}
	return plan, nil
}

// DescribeInputs prepares query and copies its positional input SQLDA fields
// before closing the statement. It never executes the statement or registers it
// with the connection.
func (c *conn) DescribeInputs(ctx context.Context, query string) ([]InputDescriptor, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateDatabaseSQLQuery(query); err != nil {
		return nil, err
	}
	c.lockDirect()
	defer c.mu.Unlock()
	// Distributed attachments are only used through explicit Attachment
	// participants, but retain Plan's guard on this shared connection method.
	if c.distributed != nil {
		return nil, ErrDistributedParticipantManaged
	}
	if c.closed || c.native == nil || c.native.broken() {
		return nil, driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	var descriptors []InputDescriptor
	if err := c.inspectPreparedStatementLocked(ctx, query, "describe inputs", func(statement *nativeStatement) error {
		var err error
		descriptors, err = statement.inputDescriptors()
		return err
	}); err != nil {
		return nil, err
	}
	return descriptors, nil
}

// DescribeStatement uses the shared prepare/inspect/close lifecycle while
// retaining any caller-owned explicit transaction on this connection.
func (c *conn) DescribeStatement(ctx context.Context, query string) (StatementDescriptor, error) {
	if err := contextError(ctx); err != nil {
		return StatementDescriptor{}, err
	}
	if err := validateDatabaseSQLQuery(query); err != nil {
		return StatementDescriptor{}, err
	}
	c.lockDirect()
	defer c.mu.Unlock()
	// Distributed attachments are only used through explicit Attachment
	// participants, but retain Plan's guard on this shared connection method.
	if c.distributed != nil {
		return StatementDescriptor{}, ErrDistributedParticipantManaged
	}
	if c.closed || c.native == nil || c.native.broken() {
		return StatementDescriptor{}, driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return StatementDescriptor{}, err
	}
	var description StatementDescriptor
	if err := c.inspectPreparedStatementLocked(ctx, query, "describe statement", func(statement *nativeStatement) error {
		var inspectErr error
		description, inspectErr = statement.description()
		return inspectErr
	}); err != nil {
		return StatementDescriptor{}, err
	}
	return description, nil
}

// inspectPreparedStatementLocked shares Plan's prepare, cancellation, error
// classification, invalidation, and close lifecycle with input description.
// The caller holds c.mu for the duration of this helper.
func (c *conn) inspectPreparedStatementLocked(ctx context.Context, query, operation string,
	inspect func(*nativeStatement) error) error {
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return err
	}
	defer releaseNative()
	statement, err := c.native.prepare(ctx, query)
	if err != nil {
		releaseNative()
		prepareOperation := "prepare " + operation
		evidence := nativeCancellationEvidenceOf(err)
		parts := splitNativeExecutionError(err)
		operationErr := c.sanitizeError(prepareOperation, parts.primary)
		cleanupErr := c.sanitizeError("", parts.cleanup)
		operationErr = classifyNativeOutcome(prepareOperation, false,
			contextCancellation(ctx), operationErr, cleanupErr, evidence)
		operationErr = joinNativeRequestDiagnostic(operationErr,
			c.sanitizeError("", parts.request))
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return operationErr
	}
	if err := contextError(ctx); err != nil {
		return errors.Join(err, statement.close())
	}
	inspectErr := inspect(statement)
	closeErr := statement.close()
	releaseNative()
	if inspectErr != nil || closeErr != nil {
		operationErr := c.sanitizeError(operation, errors.Join(inspectErr, closeErr))
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return operationErr
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	return nil
}
