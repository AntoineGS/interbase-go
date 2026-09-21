package interbase

import (
	"context"
	"database/sql/driver"
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
