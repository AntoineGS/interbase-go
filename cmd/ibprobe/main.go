package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	interbase "interbase-go"
)

func run(ctx context.Context, output io.Writer) (result error) {
	password, passwordSet := os.LookupEnv("INTERBASE_PASSWORD")
	if os.Getenv("INTERBASE_DATABASE") == "" || os.Getenv("INTERBASE_USER") == "" || !passwordSet {
		return errors.New("set INTERBASE_DATABASE, INTERBASE_USER, and INTERBASE_PASSWORD")
	}
	connector, err := interbase.NewConnector(interbase.Config{
		Database: os.Getenv("INTERBASE_DATABASE"), User: os.Getenv("INTERBASE_USER"), Password: password,
		Dialect: 1,
	})
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() { result = errors.Join(result, db.Close()) }()

	if err := db.PingContext(ctx); err != nil {
		return err
	}
	fmt.Fprintln(output, "PASS native attachment and read-only SELECT")
	var literal string
	if err := db.QueryRowContext(ctx, `SELECT "dialect one" FROM RDB$DATABASE`).Scan(&literal); err != nil {
		return err
	}
	if literal != "dialect one" {
		return errors.New("Dialect 1 string literal did not round-trip")
	}
	fmt.Fprintln(output, "PASS Dialect 1 double-quoted string")
	var relation string
	if err := db.QueryRowContext(ctx, "SELECT RDB$RELATION_NAME FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = ?", "RDB$DATABASE").Scan(&relation); err != nil {
		return err
	}
	if strings.TrimSpace(relation) != "RDB$DATABASE" {
		return errors.New("catalog parameter binding returned an unexpected relation")
	}
	fmt.Fprintln(output, "PASS parameter binding and catalog fetch")

	rows, err := db.QueryContext(ctx, `SELECT f.RDB$FUNCTION_NAME, f.RDB$RETURN_ARGUMENT,
 a.RDB$ARGUMENT_POSITION, a.RDB$FIELD_TYPE, a.RDB$FIELD_SCALE,
 a.RDB$FIELD_LENGTH, a.RDB$MECHANISM
 FROM RDB$FUNCTIONS f JOIN RDB$FUNCTION_ARGUMENTS a
 ON a.RDB$FUNCTION_NAME = f.RDB$FUNCTION_NAME`)
	if err != nil {
		return err
	}
	count := 0
	for rows.Next() {
		var name string
		var ret, pos, typ, scale, length, mechanism sql.NullInt64
		if err := rows.Scan(&name, &ret, &pos, &typ, &scale, &length, &mechanism); err != nil {
			return errors.Join(err, rows.Close())
		}
		if strings.TrimSpace(name) == "" || !pos.Valid || !typ.Valid {
			return errors.Join(errors.New("incomplete UDF argument metadata"), rows.Close())
		}
		count++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	fmt.Fprintf(output, "PASS UDF catalog query (%d argument rows; no functions invoked)\n", count)

	rows, err = db.QueryContext(ctx, "SELECT RDB$RELATION_NAME FROM RDB$RELATIONS")
	if err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	fmt.Fprintln(output, "PASS early cursor close and connection reuse")
	return nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := run(ctx, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ibprobe:", err)
		os.Exit(1)
	}
}
