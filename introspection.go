package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
)

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
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return "", err
	}
	defer releaseNative()
	statement, err := c.native.prepare(ctx, query)
	if err != nil {
		releaseNative()
		evidence := nativeCancellationEvidenceOf(err)
		parts := splitNativeExecutionError(err)
		operationErr := c.sanitizeError("prepare plan", parts.primary)
		cleanupErr := c.sanitizeError("", parts.cleanup)
		operationErr = classifyNativeOutcome("prepare plan", false,
			contextCancellation(ctx), operationErr, cleanupErr, evidence)
		operationErr = joinNativeRequestDiagnostic(operationErr,
			c.sanitizeError("", parts.request))
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return "", operationErr
	}
	if err := contextError(ctx); err != nil {
		return "", errors.Join(err, statement.close())
	}
	plan, planErr := statement.plan()
	closeErr := statement.close()
	releaseNative()
	if planErr != nil || closeErr != nil {
		operationErr := c.sanitizeError("plan", errors.Join(planErr, closeErr))
		if c.native.broken() {
			c.invalidateLocked(operationErr)
		}
		return "", operationErr
	}
	if err := contextError(ctx); err != nil {
		return "", err
	}
	return plan, nil
}
