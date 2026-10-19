//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
)

var errLineProfileNativeRollback = errors.New("line profile test: roll back the native run")

// lpFixture creates the synthetic line profile schema in a fresh database
// and returns a NONE-charset pooled connection to it.
func lpFixture(t *testing.T, dialect int) (*sql.Conn, context.Context) {
	t.Helper()
	fixture, cfg, cleanup := createFixture(t, dialect, "")
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "NONE", dialect,
		interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	ctx := readContextWithTimeout(t, 3*time.Minute)

	since := "CAST('NOW' AS DATE) - D"
	quotedAlias := `"Q"`
	// ibg-tvs: the widest exact scaled type holding one digit more than its
	// precision (BIGINT storage in dialect 3, INTEGER in dialect 1).
	wide, wideValue := "NUMERIC(18,2)", "92233720368547758.07"
	if dialect == 1 {
		since = "'NOW' - D"
		quotedAlias = "AS Q"
		wide, wideValue = "NUMERIC(9,2)", "21474836.47"
	}
	ddl := []string{
		"CREATE TABLE LP_ITEMS (ID INTEGER, CODE CHAR(3) CHARACTER SET NONE, NAME VARCHAR(20) CHARACTER SET NONE, " +
			"AMOUNT NUMERIC(15,2), D DATE)",
		"CREATE TABLE LP_LOG (ID INTEGER, MSG VARCHAR(40))",
		"CREATE TABLE LP_AUDIT (ID INTEGER, MSG VARCHAR(40))",
		"CREATE TABLE LP_BIG (ID INTEGER)",
		"CREATE TABLE LP_BLOBS (ID INTEGER, B BLOB SUB_TYPE TEXT CHARACTER SET NONE)",
		"INSERT INTO LP_BLOBS VALUES (1, 'hello blob')",
		"CREATE TABLE LP_BLOBS8 (ID INTEGER, B BLOB SUB_TYPE TEXT CHARACTER SET UTF8)",
		`CREATE PROCEDURE LP_BLOBCOPY (ID INTEGER) RETURNS (V VARCHAR(40), W VARCHAR(40)) AS
DECLARE VARIABLE B BLOB SUB_TYPE TEXT CHARACTER SET NONE;
DECLARE VARIABLE B2 BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  SELECT B FROM LP_BLOBS WHERE ID = :ID INTO :B;
  INSERT INTO LP_BLOBS8 (ID, B) VALUES (:ID, :B);
  SELECT CAST(B AS VARCHAR(40)) FROM LP_BLOBS8 WHERE ID = :ID INTO :V;
  B2 = B;
  INSERT INTO LP_BLOBS8 (ID, B) VALUES (:ID + 1, :B2);
  SELECT CAST(B AS VARCHAR(40)) FROM LP_BLOBS8 WHERE ID = :ID + 1 INTO :W;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_BLOBIN (B BLOB SUB_TYPE TEXT CHARACTER SET NONE) RETURNS (N INTEGER) AS
BEGIN
  INSERT INTO LP_BLOBS8 (ID, B) VALUES (100, :B);
  N = 1;
END`,
		`CREATE PROCEDURE LP_BLOBCALL (ID INTEGER) RETURNS (V VARCHAR(40), N INTEGER) AS
DECLARE VARIABLE B BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  FOR SELECT B FROM LP_BLOBS WHERE ID = :ID INTO :B DO
  BEGIN
    EXECUTE PROCEDURE LP_BLOBIN :B RETURNING_VALUES :N;
    SELECT CAST(B AS VARCHAR(40)) FROM LP_BLOBS8 WHERE ID = 100 INTO :V;
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_BLOBCAP RETURNS (N INTEGER) AS
DECLARE VARIABLE B BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  N = 0;
  BEGIN
    SELECT B FROM LP_BLOBS WHERE ID = 1 INTO :B;
    N = 1;
  WHEN ANY DO N = 2;
  END
  SUSPEND;
END`,
		"CREATE TABLE LP_MK_LOG (ID INTEGER)",
		`CREATE PROCEDURE LP_MK_GEN (N INTEGER) RETURNS (I INTEGER) AS
BEGIN
  I = 0;
  WHILE (I < N) DO
  BEGIN
    I = I + 1;
    INSERT INTO LP_MK_LOG VALUES (:I);
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_MK_CALLER RETURNS (I INTEGER, C INTEGER) AS
BEGIN
  FOR SELECT I FROM LP_MK_GEN(5) INTO :I DO
  BEGIN
    SELECT COUNT(*) FROM LP_MK_LOG INTO :C;
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_MK_READ (N INTEGER) RETURNS (I INTEGER, C INTEGER) AS
BEGIN
  I = 0;
  WHILE (I < N) DO
  BEGIN
    I = I + 1;
    SELECT COUNT(*) FROM LP_MK_LOG INTO :C;
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_MK_WCALLER RETURNS (I INTEGER, C INTEGER) AS
BEGIN
  FOR SELECT I, C FROM LP_MK_READ(4) INTO :I, :C DO
  BEGIN
    INSERT INTO LP_MK_LOG VALUES (:I);
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_MK_BAREW RETURNS (I INTEGER) AS
BEGIN
  FOR SELECT I FROM LP_MK_GEN(5) INTO :I DO
    SUSPEND;
END`,
		`CREATE PROCEDURE LP_MK_VIA (N INTEGER) RETURNS (I INTEGER) AS
BEGIN
  FOR SELECT I FROM LP_MK_BAREW INTO :I DO
    IF (I <= N) THEN SUSPEND;
END`,
		`CREATE PROCEDURE LP_MK_VCALLER RETURNS (I INTEGER, C INTEGER) AS
BEGIN
  FOR SELECT I FROM LP_MK_VIA(5) INTO :I DO
  BEGIN
    SELECT COUNT(*) FROM LP_MK_LOG INTO :C;
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_MK_RCALLER RETURNS (I INTEGER, C INTEGER) AS
BEGIN
  FOR SELECT I, C FROM LP_MK_READ(3) INTO :I, :C DO
    SUSPEND;
END`,
		`CREATE PROCEDURE LP_BLOBOUT RETURNS (B BLOB SUB_TYPE TEXT CHARACTER SET NONE) AS
BEGIN
  FOR SELECT B FROM LP_BLOBS INTO :B DO
    SUSPEND;
END`,
		"CREATE TABLE LP_PRICES (ID INTEGER, P4 NUMERIC(15,4), S SMALLINT, N92 NUMERIC(9,2))",
		"CREATE TABLE LP_UNIQ (ID INTEGER NOT NULL PRIMARY KEY)",
		"CREATE TABLE LP_TEXTS (ID INTEGER, T40 VARCHAR(40) CHARACTER SET NONE, W40 VARCHAR(40) CHARACTER SET WIN1252, U40 VARCHAR(40))",
		"CREATE TABLE LP_PADS (ID INTEGER, C3 CHAR(3) CHARACTER SET NONE, C10 CHAR(10) CHARACTER SET NONE, " +
			"U3 CHAR(3), V20 VARCHAR(20) CHARACTER SET NONE, N INTEGER, P4 NUMERIC(15,4))",
		"CREATE EXCEPTION LP_EX 'line profile failure'",
		"CREATE EXCEPTION LP_OTHER 'other failure'",
		"CREATE TRIGGER LP_LOG_AI FOR LP_LOG AFTER INSERT AS BEGIN INSERT INTO LP_AUDIT (ID, MSG) VALUES (NEW.ID, NEW.MSG); END",
		"CREATE PROCEDURE LP_NOOP AS BEGIN EXIT; END",
		`CREATE PROCEDURE LP_RAISE (K INTEGER) AS
BEGIN
  IF (K = 1) THEN EXCEPTION LP_EX;
  IF (K = 2) THEN EXCEPTION LP_OTHER;
END`,
		`CREATE PROCEDURE LP_WHEN (K INTEGER) RETURNS (R VARCHAR(60), CNT INTEGER) AS
DECLARE VARIABLE I INTEGER;
BEGIN
  R = 'start';
  CNT = 0;
  INSERT INTO LP_UNIQ (ID) VALUES (1);
  BEGIN
    INSERT INTO LP_UNIQ (ID) VALUES (2);
    EXCEPTION LP_EX;
    R = 'not reached 1';
  WHEN EXCEPTION LP_OTHER DO R = R || '-other';
  WHEN EXCEPTION LP_EX DO R = R || '-ex';
  END
  SUSPEND;
  BEGIN
    INSERT INTO LP_UNIQ (ID) VALUES (3);
    INSERT INTO LP_UNIQ (ID) VALUES (1);
    R = 'not reached 2';
  WHEN SQLCODE -803 DO R = R || '-dup';
  END
  INSERT INTO LP_UNIQ (ID) VALUES (4);
  SUSPEND;
  BEGIN
    BEGIN
      INSERT INTO LP_UNIQ (ID) VALUES (5);
      EXECUTE PROCEDURE LP_RAISE :K;
      R = R || '-noraise';
    WHEN SQLCODE -803 DO R = R || '-inner';
    END
  WHEN EXCEPTION LP_EX DO R = R || '-outer';
  WHEN ANY DO R = R || '-any';
  END
  SUSPEND;
  BEGIN
    SELECT ID FROM LP_UNIQ INTO :I;
  WHEN GDSCODE sing_select_err DO R = R || '-sing';
  END
  I = 0;
  WHILE (I < 3) DO
  BEGIN
    I = I + 1;
    BEGIN
      INSERT INTO LP_UNIQ (ID) VALUES (10);
    WHEN GDSCODE unique_key_violation DO CNT = CNT + 1;
    END
  END
  SUSPEND;
  SELECT COUNT(*) FROM LP_UNIQ INTO :CNT;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_UNDO RETURNS (R VARCHAR(200), C INTEGER) AS
DECLARE VARIABLE X INTEGER;
BEGIN
  R = 'u';
  BEGIN
    INSERT INTO LP_UNIQ VALUES (99);
    FOR SELECT ID FROM LP_BIG WHERE ID <= 3 ORDER BY ID INTO :X DO
    BEGIN
      INSERT INTO LP_UNIQ VALUES (:X + 100);
      IF (X = 3) THEN EXCEPTION LP_EX;
    END
  WHEN ANY DO R = R || '-for';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (2);
    BEGIN
      INSERT INTO LP_UNIQ VALUES (3);
      IF (1 = 1) THEN
      BEGIN
        INSERT INTO LP_UNIQ VALUES (4);
        EXCEPTION LP_EX;
      END
    END
  WHEN ANY DO R = R || '-nested';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (200);
    BEGIN
      INSERT INTO LP_UNIQ VALUES (300);
      EXCEPTION LP_EX;
    WHEN ANY DO
      BEGIN
        INSERT INTO LP_UNIQ VALUES (301);
        EXCEPTION LP_OTHER;
      END
    END
  WHEN ANY DO R = R || '-handlerfails';
  END
  BEGIN
    EXCEPTION LP_EX;
  WHEN EXCEPTION LP_OTHER DO R = R || '-a';
  WHEN SQLCODE -836 DO R = R || '-b';
  WHEN EXCEPTION LP_EX DO R = R || '-c';
  WHEN ANY DO R = R || '-d';
  WHEN GDSCODE except, ANY DO R = R || '-e';
  END
  FOR SELECT ID FROM LP_UNIQ ORDER BY ID INTO :C DO R = R || ',' || C;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_LOST RETURNS (R VARCHAR(60)) AS
BEGIN
  R = 'h';
  BEGIN
    EXCEPTION LP_EX;
  WHEN ANY DO BEGIN R = R || '-first'; EXIT; END
  WHEN ANY DO R = R || '-second';
  END
  R = R || '-after';
END`,
		`CREATE PROCEDURE LP_EXITH RETURNS (R VARCHAR(60)) AS
BEGIN
  R = 'x';
  BEGIN
    EXCEPTION LP_EX;
  WHEN ANY DO
    BEGIN
      R = R || '-h';
      EXIT;
      R = R || '-noexit';
    END
  END
  R = R || '-after';
END`,
		`CREATE PROCEDURE LP_EXC (K INTEGER) RETURNS (R INTEGER) AS
BEGIN
  R = K;
  SUSPEND;
  IF (K > 0) THEN
    EXCEPTION LP_EX;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_R18 (MAXID INTEGER) RETURNS (V VARCHAR(8) CHARACTER SET NONE, W VARCHAR(8) CHARACTER SET WIN1252,
  U VARCHAR(8), N NUMERIC(15,2)) AS
BEGIN
  FOR SELECT T40 FROM LP_TEXTS WHERE ID <= :MAXID ORDER BY ID INTO :V DO SUSPEND;
  FOR SELECT W40 FROM LP_TEXTS WHERE ID <= :MAXID ORDER BY ID INTO :W DO SUSPEND;
  FOR SELECT U40 FROM LP_TEXTS WHERE ID <= :MAXID ORDER BY ID INTO :U DO SUSPEND;
  FOR SELECT P4 FROM LP_PRICES ORDER BY ID INTO :N DO SUSPEND;
  FOR SELECT DISTINCT P4 FROM LP_PRICES INTO :N DO SUSPEND;
END`,
		`CREATE PROCEDURE LP_R18W (MAXID INTEGER) RETURNS (W VARCHAR(8) CHARACTER SET WIN1252) AS
BEGIN
  FOR SELECT W40 FROM LP_TEXTS WHERE ID <= :MAXID ORDER BY ID INTO :W DO SUSPEND;
END`,
		`CREATE PROCEDURE LP_PAD_INTO (I INTEGER) RETURNS (V2 VARCHAR(2) CHARACTER SET NONE, V6 VARCHAR(6) CHARACTER SET NONE,
  U2 VARCHAR(2), X VARCHAR(20) CHARACTER SET NONE, Y VARCHAR(6) CHARACTER SET NONE, Z VARCHAR(2),
  W NUMERIC(15,2), K VARCHAR(2)) AS
BEGIN
  SELECT C3, C10, U3, UPPER(V20), CAST(C10 AS VARCHAR(10) CHARACTER SET NONE),
    CASE WHEN N > 0 THEN 'P ' ELSE 'N  ' END, P4 AS PP, C3 ` + quotedAlias + `
  FROM LP_PADS WHERE ID = :I
  INTO :V2, :V6, :U2, :X, :Y, :Z, :W, :K;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_PAD_FOR RETURNS (V2 VARCHAR(2) CHARACTER SET NONE, U2 VARCHAR(2), W NUMERIC(15,2)) AS
BEGIN
  FOR SELECT C3, U3, P4 FROM LP_PADS WHERE ID < 3 ORDER BY ID INTO :V2, :U2, :W DO SUSPEND;
END`,
		`CREATE PROCEDURE LP_PM_FIRST RETURNS (W VARCHAR(8) CHARACTER SET WIN1252) AS
BEGIN
  SELECT W40 FROM LP_TEXTS WHERE ID IN (1, 3) ORDER BY ID INTO :W;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_PM_SECOND RETURNS (W VARCHAR(8) CHARACTER SET WIN1252) AS
BEGIN
  SELECT W40 FROM LP_TEXTS WHERE ID IN (1, 3) ORDER BY ID DESC INTO :W;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_PM_SRC (K INTEGER) RETURNS (T VARCHAR(40) CHARACTER SET WIN1252) AS
BEGIN
  INSERT INTO LP_LOG (ID, MSG) VALUES (:K, 'pm');
  T = 'short';
  SUSPEND;
  T = 'far too long here';
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_PM_PROC RETURNS (W VARCHAR(8) CHARACTER SET WIN1252) AS
BEGIN
  SELECT T FROM LP_PM_SRC(900) INTO :W;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_R18U (MAXID INTEGER) RETURNS (U VARCHAR(8)) AS
BEGIN
  FOR SELECT U40 FROM LP_TEXTS WHERE ID <= :MAXID ORDER BY ID INTO :U DO SUSPEND;
END`,
		`CREATE PROCEDURE LP_SCALARS (N INTEGER)
RETURNS (A INTEGER, S VARCHAR(10) CHARACTER SET NONE, C CHAR(3) CHARACTER SET NONE, M NUMERIC(15,2)) AS
DECLARE VARIABLE I INTEGER;
DECLARE VARIABLE T VARCHAR(10);
DECLARE VARIABLE NV INTEGER;
BEGIN
  I = 0;
  A = 0;
  S = 'x';
  WHILE (I < N) DO
  BEGIN
    I = I + 1;
    A = A + I;
    IF (I = 2) THEN
      S = S || 'y';
    ELSE IF (I > 3) THEN
    BEGIN
      IF (A > 100) THEN
        S = 'big';
      ELSE
        S = S || 'z';
    END
  END
  C = 'ab';
  T = 'Q' || S;
  IF (T IS NULL OR C = 'ab ') THEN M = 1.25;
  IF (NV = 1) THEN
    A = -1;
  IF (NOT (NV = 1)) THEN
    A = -2;
  M = M * 2;
  SUSPEND;
  S = NULL;
  A = A + N;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_SELECTS (ID INTEGER) RETURNS (NAME VARCHAR(20) CHARACTER SET NONE, CNT INTEGER) AS
DECLARE VARIABLE X INTEGER;
BEGIN
  NAME = 'init';
  SELECT NAME FROM LP_ITEMS WHERE ID = :ID INTO :NAME;
  SUSPEND;
  SELECT NAME FROM LP_ITEMS WHERE ID = -1 INTO :NAME;
  SUSPEND;
  CNT = 0;
  FOR SELECT ID, NAME FROM LP_ITEMS ORDER BY ID INTO :X, :NAME DO
  BEGIN
    CNT = CNT + 1;
    IF (NAME > 'Cafe') THEN SUSPEND;
  END
  SELECT COUNT(*) FROM LP_ITEMS WHERE D IS NOT NULL AND ` + since + ` > 0 INTO :CNT;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_WRITES (ID INTEGER) RETURNS (AFFECTED INTEGER) AS
BEGIN
  INSERT INTO LP_LOG (ID, MSG) VALUES (:ID, 'hello');
  SELECT COUNT(*) FROM LP_LOG WHERE ID = :ID INTO :AFFECTED;
  SUSPEND;
  UPDATE LP_LOG SET MSG = 'upd' WHERE ID = :ID OR ID = 0;
  SELECT COUNT(*) FROM LP_LOG WHERE ID = :ID INTO :AFFECTED;
  SUSPEND;
  DELETE FROM LP_LOG WHERE ID = :ID;
  SELECT COUNT(*) FROM LP_LOG WHERE ID = :ID INTO :AFFECTED;
  SUSPEND;
  EXECUTE PROCEDURE LP_NOOP;
END`,
		`CREATE PROCEDURE LP_SINGLETON RETURNS (NAME VARCHAR(20) CHARACTER SET NONE) AS
BEGIN
  SELECT NAME FROM LP_ITEMS INTO :NAME;
END`,
		`CREATE PROCEDURE LP_TRUNC RETURNS (V VARCHAR(3) CHARACTER SET NONE) AS
DECLARE VARIABLE W VARCHAR(10) CHARACTER SET NONE;
BEGIN
  W = 'abcde';
  V = W;
END`,
		`CREATE PROCEDURE LP_TRUNC8 RETURNS (V VARCHAR(3)) AS
DECLARE VARIABLE W VARCHAR(10);
BEGIN
  W = 'abcde';
  V = W;
END`,
		`CREATE PROCEDURE LP_NOARGS RETURNS (ID INTEGER) AS
BEGIN
  FOR SELECT ID FROM LP_ITEMS INTO :ID DO SUSPEND;
END`,
		`CREATE PROCEDURE LP_BARE_USER RETURNS (N INTEGER) AS
BEGIN
  SELECT COUNT(*) FROM LP_NOARGS INTO :N;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_SLOW RETURNS (N INTEGER) AS
DECLARE VARIABLE X INTEGER;
BEGIN
  N = 0;
  FOR SELECT A.ID FROM LP_BIG A, LP_BIG B, LP_BIG C INTO :X DO
    N = N + 1;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_NUMS RETURNS (A2 NUMERIC(15,2), B2 NUMERIC(15,2), SM SMALLINT, CNT INTEGER, LIT NUMERIC(15,2)) AS
DECLARE VARIABLE BIG INTEGER;
BEGIN
  SELECT P4 FROM LP_PRICES WHERE ID = 1 INTO :A2;
  SELECT SUM(P4 * S) FROM LP_PRICES INTO :B2;
  SELECT S FROM LP_PRICES WHERE ID = 2 INTO :SM;
  BIG = 100000;
  SELECT COUNT(*) FROM LP_PRICES WHERE S = :BIG OR S < :BIG INTO :CNT;
  LIT = 1.2345;
  SUSPEND;
  FOR SELECT P4, S FROM LP_PRICES ORDER BY ID INTO :A2, :SM DO
    SUSPEND;
END`,
		`CREATE PROCEDURE LP_NARROW (ID INTEGER) RETURNS (N42 NUMERIC(4,2)) AS
BEGIN
  SELECT N92 FROM LP_PRICES WHERE ID = :ID INTO :N42;
  SUSPEND;
END`,
		// ibg-b6o: BLOB parity edges, as probed natively.
		"CREATE TABLE LP_BLOBSW (ID INTEGER, B BLOB SUB_TYPE TEXT CHARACTER SET WIN1252)",
		"CREATE TABLE LP_BLOBSX (ID INTEGER, B BLOB SUB_TYPE TEXT CHARACTER SET NONE)",
		"INSERT INTO LP_BLOBSX VALUES (1, 'Caf\xe9 blob')",
		`CREATE PROCEDURE LP_BRAW (K INTEGER) RETURNS (R VARCHAR(40) CHARACTER SET OCTETS) AS
DECLARE VARIABLE XN BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  SELECT B FROM LP_BLOBSX WHERE ID = 1 INTO :XN;
  IF (K = 1) THEN
  BEGIN
    INSERT INTO LP_BLOBS8 (ID, B) VALUES (63, :XN);
    SELECT CAST(B AS VARCHAR(40) CHARACTER SET OCTETS) FROM LP_BLOBS8 WHERE ID = 63 INTO :R;
  END
  IF (K = 2) THEN
  BEGIN
    INSERT INTO LP_BLOBSW (ID, B) VALUES (64, :XN);
    SELECT CAST(B AS VARCHAR(40) CHARACTER SET OCTETS) FROM LP_BLOBSW WHERE ID = 64 INTO :R;
  END
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_BCHAR (K INTEGER) RETURNS (R VARCHAR(80) CHARACTER SET OCTETS) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  IF (K = 1) THEN X = CAST('ab' AS CHAR(5));
  IF (K = 2) THEN X = CAST('ab' AS CHAR(5) CHARACTER SET UTF8);
  IF (K = 3) THEN X = CAST('ab' AS CHAR(5) CHARACTER SET WIN1252);
  IF (K = 4) THEN SELECT U3 FROM LP_PADS WHERE ID = 2 INTO :X;
  IF (K = 5) THEN SELECT C3 FROM LP_PADS WHERE ID = 2 INTO :X;
  INSERT INTO LP_BLOBSX VALUES (70 + :K, :X);
  SELECT '[' || CAST(B AS VARCHAR(40) CHARACTER SET OCTETS) || ']' FROM LP_BLOBSX WHERE ID = 70 + :K INTO :R;
  SUSPEND;
END`,
		"CREATE PROCEDURE LP_BOUT_T RETURNS (B BLOB SUB_TYPE TEXT CHARACTER SET NONE) AS BEGIN B = 'abcdefghij'; END",
		"CREATE PROCEDURE LP_BSEL_T (K INTEGER) RETURNS (B BLOB SUB_TYPE TEXT CHARACTER SET NONE) AS BEGIN B = 'abcdefghij'; SUSPEND; END",
		`CREATE PROCEDURE LP_BTEXT (K INTEGER) RETURNS (R VARCHAR(40) CHARACTER SET NONE, Q VARCHAR(5) CHARACTER SET NONE) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  X = 'abcdefghij';
  R = 'start';
  SUSPEND;
  IF (K = 1) THEN R = X;
  IF (K = 2) THEN EXECUTE PROCEDURE LP_BOUT_T RETURNING_VALUES :R;
  IF (K = 3) THEN EXECUTE PROCEDURE LP_BOUT_T RETURNING_VALUES :Q;
  IF (K = 4) THEN SELECT B FROM LP_BSEL_T(1) INTO :Q;
  IF (K = 5) THEN FOR SELECT B FROM LP_BSEL_T(1) INTO :R DO SUSPEND;
  IF (K = 6) THEN X = NULL;
  IF (K = 6) THEN R = X;
  IF (X IS NULL) THEN Q = 'null'; ELSE Q = 'set';
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_BISNULL RETURNS (R VARCHAR(10)) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  X = 42;
  IF (X IS NULL) THEN R = 'null'; ELSE R = 'notnull';
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_BDATE (K INTEGER) RETURNS (R VARCHAR(10)) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  R = 'start';
  IF (K = 1) THEN X = CAST('2020-01-02' AS DATE);
  IF (K = 2) THEN X = CAST('2020-01-02 03:04:05.1234' AS TIMESTAMP);
  IF (K = 3) THEN X = CAST('2020-01-02 13:04:05' AS TIMESTAMP);
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_BNUM (K INTEGER) RETURNS (R VARCHAR(40)) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  R = 'start';
  IF (K = 1) THEN X = CAST(5 AS NUMERIC(18,0));
  IF (K = 2) THEN X = CAST(0 AS NUMERIC(18,0));
  IF (K = 3) THEN X = CAST(1.5 AS NUMERIC(18,2));
  IF (K = 4) THEN X = CAST(0 AS NUMERIC(18,2));
  R = 'assigned';
  SUSPEND;
  INSERT INTO LP_BLOBSX VALUES (80 + :K, :X);
  SELECT '[' || CAST(B AS VARCHAR(40)) || ']' FROM LP_BLOBSX WHERE ID = 80 + :K INTO :R;
  SUSPEND;
END`,
		// ibg-85p: WHEN GDSCODE matches the first status code only.
		`CREATE PROCEDURE LP_GDS2 RETURNS (R VARCHAR(20)) AS
BEGIN
  R = 'none';
  BEGIN
    BEGIN
      EXCEPTION LP_EX;
    WHEN GDSCODE random DO R = 'second';
    END
  WHEN GDSCODE except DO R = 'first';
  END
  SUSPEND;
END`,
		"CREATE PROCEDURE LP_WIDE_OUT RETURNS (V " + wide + ") AS BEGIN V = " + wideValue + "; END",
		`CREATE PROCEDURE LP_WIDE_IN (K INTEGER) RETURNS (D DOUBLE PRECISION, T VARCHAR(30), N NUMERIC(15,4)) AS
BEGIN
  EXECUTE PROCEDURE LP_WIDE_OUT RETURNING_VALUES :D;
  EXECUTE PROCEDURE LP_WIDE_OUT RETURNING_VALUES :T;
  SUSPEND;
  IF (K = 1) THEN
    EXECUTE PROCEDURE LP_WIDE_OUT RETURNING_VALUES :N;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_RET (X INTEGER) RETURNS (Y INTEGER, Z SMALLINT) AS
BEGIN
  Y = X * 2;
  Z = X;
END`,
		`CREATE PROCEDURE LP_CALLER RETURNS (R INTEGER, Q SMALLINT) AS
BEGIN
  EXECUTE PROCEDURE LP_RET 21 RETURNING_VALUES :R, :Q;
  SUSPEND;
  EXECUTE PROCEDURE LP_RET :R RETURNING_VALUES :R, :Q;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_CALLER_P RETURNS (R INTEGER, Q SMALLINT) AS
BEGIN
  EXECUTE PROCEDURE LP_RET 4 RETURNING_VALUES (:R, :Q);
  SUSPEND;
  EXECUTE PROCEDURE LP_RET (5) RETURNING_VALUES (Q, R);
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_MANY RETURNS (I INTEGER) AS
BEGIN
  I = 0;
  WHILE (I < 15) DO
  BEGIN
    I = I + 1;
    SUSPEND;
  END
END`,
		"CREATE TABLE LP_TRG (ID INTEGER)",
		"CREATE TRIGGER LP_TRG_BI FOR LP_TRG BEFORE INSERT AS BEGIN IF (NEW.ID < 0) THEN EXCEPTION LP_EX; END",
		"CREATE TABLE LP_BT (ID INTEGER, V VARCHAR(20) CHARACTER SET NONE, C CHAR(5) CHARACTER SET NONE)",
		"INSERT INTO LP_BT VALUES (1, 'vtext', 'ab')",
		"CREATE TABLE LP_BB (ID INTEGER, B BLOB SUB_TYPE TEXT CHARACTER SET NONE)",
		`CREATE PROCEDURE LP_INNER_EXEC (X INTEGER) RETURNS (Y INTEGER, S VARCHAR(10) CHARACTER SET NONE) AS
BEGIN
  Y = X * 2;
  S = 'e' || X;
  INSERT INTO LP_LOG (ID, MSG) VALUES (:X, 'inner');
END`,
		`CREATE PROCEDURE LP_INNER_SEL (N INTEGER) RETURNS (I INTEGER, T VARCHAR(10) CHARACTER SET NONE) AS
DECLARE VARIABLE J INTEGER;
BEGIN
  I = 0;
  WHILE (I < N) DO
  BEGIN
    I = I + 1;
    EXECUTE PROCEDURE LP_INNER_EXEC :I RETURNING_VALUES :J, :T;
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_OUTER (N INTEGER) RETURNS (R INTEGER, Q VARCHAR(10) CHARACTER SET NONE, K INTEGER) AS
DECLARE VARIABLE I INTEGER;
DECLARE VARIABLE J SMALLINT;
BEGIN
  I = 0;
  WHILE (I < 2) DO
  BEGIN
    I = I + 1;
    J = N + I;
    EXECUTE PROCEDURE LP_INNER_EXEC :J RETURNING_VALUES :R, :Q;
  END
  SUSPEND;
  FOR SELECT I, T FROM LP_INNER_SEL(:N) INTO :K, :Q DO
  BEGIN
    R = R + K;
    SUSPEND;
  END
  SELECT I, T FROM LP_INNER_SEL(1) INTO :K, :Q;
  SUSPEND;
  SELECT MAX(I) FROM LP_INNER_SEL(3) INTO :K;
  SUSPEND;
  SELECT COUNT(*) FROM LP_LOG INTO :K;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_REC (N INTEGER) RETURNS (D INTEGER) AS
BEGIN
  D = 0;
END`,
		`ALTER PROCEDURE LP_REC (N INTEGER) RETURNS (D INTEGER) AS
DECLARE VARIABLE M INTEGER;
BEGIN
  D = 0;
  M = N - 1;
  IF (N > 0) THEN
    EXECUTE PROCEDURE LP_REC :M RETURNING_VALUES :D;
  D = D + 1;
END`,
		`CREATE PROCEDURE LP_SEL_W (N INTEGER) RETURNS (I INTEGER) AS
BEGIN
  I = 0;
  WHILE (I < 3) DO
  BEGIN
    I = I + 1;
    INSERT INTO LP_UNIQ VALUES (:I + 500);
    IF (I = N) THEN EXCEPTION LP_EX;
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_OUTER_W (N INTEGER) RETURNS (R VARCHAR(200), C INTEGER) AS
BEGIN
  R = 'w';
  BEGIN
    INSERT INTO LP_UNIQ VALUES (1);
    FOR SELECT I FROM LP_SEL_W(:N) INTO :C DO
      INSERT INTO LP_UNIQ VALUES (:C + 600);
  WHEN ANY DO R = R || '-caught';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (2);
    SELECT I FROM LP_SEL_W(:N - 1) INTO :C;
  WHEN ANY DO R = R || '-caught2';
  END
  FOR SELECT ID FROM LP_UNIQ ORDER BY ID INTO :C DO R = R || ',' || C;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_OUTER_B RETURNS (R VARCHAR(200), C INTEGER) AS
BEGIN
  R = 'b';
  BEGIN
    INSERT INTO LP_UNIQ VALUES (3);
    FOR SELECT I FROM LP_SEL_W(0) INTO :C DO
    BEGIN
      INSERT INTO LP_UNIQ VALUES (:C + 700);
      IF (C = 2) THEN EXCEPTION LP_EX;
    END
  WHEN ANY DO R = R || '-body';
  END
  FOR SELECT ID FROM LP_UNIQ ORDER BY ID INTO :C DO R = R || ',' || C;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_EXEC_W (N INTEGER) AS
BEGIN
  INSERT INTO LP_UNIQ VALUES (:N + 700);
  IF (N > 0) THEN EXCEPTION LP_EX;
END`,
		`CREATE PROCEDURE LP_RAISE0 AS
BEGIN
  EXCEPTION LP_EX;
END`,
		`CREATE PROCEDURE LP_HANDLING (N INTEGER) RETURNS (R VARCHAR(200), C INTEGER) AS
BEGIN
  R = 'h';
  C = 0;
  INSERT INTO LP_UNIQ VALUES (9);
  BEGIN
    INSERT INTO LP_UNIQ VALUES (1);
    EXECUTE PROCEDURE LP_EXEC_W :N;
  WHEN ANY DO R = R || '-exec';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (10);
    BEGIN
      INSERT INTO LP_UNIQ VALUES (11);
      EXECUTE PROCEDURE LP_RAISE0;
    END
  WHEN ANY DO R = R || '-nested';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (20);
    EXECUTE PROCEDURE LP_RAISE0;
  WHEN ANY DO
    BEGIN
      INSERT INTO LP_UNIQ VALUES (21);
    END
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (30);
    SELECT ID FROM LP_UNIQ INTO :C;
  WHEN ANY DO R = R || '-singleton';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (40);
    C = 1 / (C - C);
  WHEN ANY DO R = R || '-assign';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (50);
    IF (1 / (C - C) = 1) THEN R = R || 'x';
  WHEN ANY DO R = R || '-cond';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (60);
    SELECT MAX(I) FROM LP_SEL_W(1) INTO :C;
  WHEN ANY DO R = R || '-max';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (70);
    INSERT INTO LP_TRG VALUES (-1);
  WHEN ANY DO R = R || '-trigger';
  END
  BEGIN
    INSERT INTO LP_UNIQ VALUES (80);
    EXECUTE PROCEDURE LP_EXEC_W 0;
    EXCEPTION LP_EX;
  WHEN ANY DO R = R || '-direct';
  END
  FOR SELECT ID FROM LP_UNIQ ORDER BY ID INTO :C DO R = R || ',' || C;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_TOBLOB RETURNS (A VARCHAR(40), B VARCHAR(40), C VARCHAR(40)) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
DECLARE VARIABLE J INTEGER;
BEGIN
  SELECT V FROM LP_BT WHERE ID = 1 INTO :X;
  INSERT INTO LP_BB VALUES (1, :X);
  SELECT C FROM LP_BT WHERE ID = 1 INTO :X;
  INSERT INTO LP_BB VALUES (2, :X);
  EXECUTE PROCEDURE LP_INNER_EXEC 7 RETURNING_VALUES :J, :X;
  INSERT INTO LP_BB VALUES (3, :X);
  SELECT '[' || CAST(B AS VARCHAR(40)) || ']' FROM LP_BB WHERE ID = 1 INTO :A;
  SELECT '[' || CAST(B AS VARCHAR(40)) || ']' FROM LP_BB WHERE ID = 2 INTO :B;
  SELECT '[' || CAST(B AS VARCHAR(40)) || ']' FROM LP_BB WHERE ID = 3 INTO :C;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_SCAN (K INTEGER) RETURNS (ID INTEGER) AS
BEGIN
  FOR SELECT ID FROM LP_BIG WHERE ID <= :K ORDER BY ID INTO :ID DO
  BEGIN
    INSERT INTO LP_LOG (ID, MSG) VALUES (:ID, 'scan');
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_FIRST RETURNS (ID INTEGER, C INTEGER, L INTEGER) AS
BEGIN
  SELECT ID FROM LP_SCAN(1) INTO :ID;
  C = 0;
  FOR SELECT ID FROM LP_SCAN(100) INTO :ID DO
  BEGIN
    C = C + 1;
    IF (C = 3) THEN
    BEGIN
      SELECT COUNT(*) FROM LP_LOG INTO :L;
      EXIT;
    END
  END
END`,
		`CREATE PROCEDURE LP_BLOBSET (K INTEGER) RETURNS (R VARCHAR(60)) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
DECLARE VARIABLE V VARCHAR(20) CHARACTER SET NONE;
DECLARE VARIABLE I INTEGER;
BEGIN
  I = 42;
  V = 'var';
  X = 'text';
  IF (K = 1) THEN X = V;
  IF (K = 2) THEN X = I;
  IF (K = 3) THEN X = 0;
  IF (K = 4) THEN X = 12.5;
  IF (K = 5) THEN X = CAST('2020-01-02' AS DATE);
  IF (K = 6) THEN X = 'n' || I;
  R = 'assigned';
  SUSPEND;
  INSERT INTO LP_BB VALUES (10, :X);
  SELECT '[' || CAST(B AS VARCHAR(40)) || ']' FROM LP_BB WHERE ID = 10 INTO :R;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_BLOBSET_W (K INTEGER) RETURNS (R VARCHAR(60)) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
DECLARE VARIABLE I INTEGER;
BEGIN
  I = 42;
  R = 'w';
  BEGIN
    IF (K = 4) THEN X = 12.5;
    IF (K = 5) THEN X = CAST('2020-01-02' AS DATE);
    X = I;
    INSERT INTO LP_BB VALUES (11, :X);
  WHEN ANY DO R = R || '-caught';
  END
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_BLOBARG (K INTEGER) RETURNS (V VARCHAR(40), N INTEGER) AS
DECLARE VARIABLE I INTEGER;
BEGIN
  I = 5;
  IF (K = 1) THEN
    EXECUTE PROCEDURE LP_BLOBIN :I RETURNING_VALUES :N;
  ELSE
    EXECUTE PROCEDURE LP_BLOBIN 'txt' RETURNING_VALUES :N;
  SELECT CAST(B AS VARCHAR(40)) FROM LP_BLOBS8 WHERE ID = 100 INTO :V;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_SLOW2 (K INTEGER) RETURNS (N INTEGER) AS
DECLARE VARIABLE X INTEGER;
BEGIN
  N = 0;
  FOR SELECT A.ID FROM LP_BIG A, LP_BIG B, LP_BIG C INTO :X DO
    N = N + K;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_SLOW_CALLER RETURNS (N INTEGER) AS
BEGIN
  FOR SELECT N FROM LP_SLOW2(1) INTO :N DO
    SUSPEND;
END`,
		"INSERT INTO LP_PRICES VALUES (1, 1.2345, 7, 12.34)",
		"INSERT INTO LP_PRICES VALUES (2, 2.5555, 100, 1.5)",
		"INSERT INTO LP_PRICES VALUES (3, 0.0050, -3, 400.25)",
		"INSERT INTO LP_ITEMS VALUES (1, 'abc', 'Caf\xe9', 10.50, '2020-01-01')",
		"INSERT INTO LP_ITEMS VALUES (2, 'de', 'Cafe', 20.25, '2021-06-15')",
		"INSERT INTO LP_ITEMS VALUES (3, NULL, 'Zed', NULL, NULL)",
		"INSERT INTO LP_PADS VALUES (1, 'AB', 'ABCDEF', 'AB', 'mixed Case', 5, 1.2345)",
		"INSERT INTO LP_PADS VALUES (2, 'A', 'ABC', 'A', 'x', -1, 2.5555)",
		"INSERT INTO LP_PADS VALUES (3, 'ABC', 'ABCDEF', 'AB', 'x', 1, 0)",
		"INSERT INTO LP_PADS VALUES (4, 'AB', 'ABCDEFG', 'AB', 'x', 1, 0)",
		"INSERT INTO LP_TEXTS VALUES (1, 'short', 'short', 'short')",
		"INSERT INTO LP_TEXTS VALUES (2, 'eight888', 'eight888', 'eight888')",
		"INSERT INTO LP_TEXTS VALUES (3, 'far too long here', 'far too long here', 'far too long here')",
		"INSERT INTO LP_TEXTS VALUES (4, 'x', 'x', 'abcdefghijklmnopqrstuvwxyz0123456789')",
	}
	for i := 1; i <= 100; i++ {
		ddl = append(ddl, fmt.Sprintf("INSERT INTO LP_BIG VALUES (%d)", i))
	}
	ddl = append(ddl, lpBlobFetchDDL()...)
	ddl = append(ddl, lpDynamicDDL()...)
	for _, statement := range ddl {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(statement, "\n", 2)[0], err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn(): %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, ctx
}

// lpNative runs query natively in a rolled-back read-committed transaction
// and returns its rows as driver values.
func lpNative(t *testing.T, ctx context.Context, conn *sql.Conn, query string) ([][]any, error) {
	t.Helper()
	var rows [][]any
	var runErr error
	err := interbase.WithTransaction(ctx, conn, interbase.TransactionOptions{Isolation: sql.LevelReadCommitted},
		func(tx *interbase.Transaction) error {
			cursor, err := tx.Query(ctx, query)
			if err != nil {
				runErr = err
				return errLineProfileNativeRollback
			}
			defer cursor.Close()
			for {
				hasRow, err := cursor.Next(ctx)
				if err != nil {
					runErr = err
					return errLineProfileNativeRollback
				}
				if !hasRow {
					return errLineProfileNativeRollback
				}
				row, err := cursor.Row()
				if err != nil {
					runErr = err
					return errLineProfileNativeRollback
				}
				values := lpValues(row)
				for i, value := range values {
					if ref, ok := value.(interbase.BlobRef); ok {
						reader, err := tx.OpenBlob(ctx, ref)
						if err != nil {
							runErr = err
							return errLineProfileNativeRollback
						}
						data, err := io.ReadAll(reader)
						_ = reader.Close()
						if err != nil {
							runErr = err
							return errLineProfileNativeRollback
						}
						values[i] = string(data)
					}
				}
				rows = append(rows, values)
			}
		})
	if !errors.Is(err, errLineProfileNativeRollback) {
		t.Fatalf("native run of %q: %v", query, err)
	}
	return rows, runErr
}

func lpValues(row []interbase.Cell) []any {
	values := make([]any, len(row))
	for i, cell := range row {
		if cell.Indicator&0x8000 == 0 {
			values[i] = cell.Value
		}
	}
	return values
}

func lpSQLCode(err error) int {
	var nativeErr *interbase.Error
	if errors.As(err, &nativeErr) {
		return nativeErr.SQLCode
	}
	return 0
}

// lpLine returns the source line of the first occurrence of marker.
func lpLine(t *testing.T, source, marker string) int {
	t.Helper()
	index := strings.Index(source, marker)
	if index < 0 {
		t.Fatalf("marker %q not in source", marker)
	}
	return strings.Count(source[:index], "\n") + 1
}

// lpNodeAt returns the index of the first node starting on marker's line.
func lpNodeAt(t *testing.T, routine interbase.LineProfileRoutine, marker string) int {
	t.Helper()
	line := lpLine(t, routine.Source, marker)
	for i, node := range routine.Nodes {
		if node.Line == line && node.Kind != "block" {
			return i
		}
	}
	t.Fatalf("no node on line %d (%q)", line, marker)
	return -1
}

func lpRelationID(t *testing.T, ctx context.Context, conn *sql.Conn, name string) int {
	t.Helper()
	var id int
	if err := conn.QueryRowContext(ctx,
		"SELECT RDB$RELATION_ID FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = ?", name).Scan(&id); err != nil {
		t.Fatalf("relation id of %s: %v", name, err)
	}
	return id
}

func TestLineProfileDifferential(t *testing.T) {
	for _, dialect := range []int{3, 1} {
		t.Run(fmt.Sprintf("dialect %d", dialect), func(t *testing.T) {
			conn, ctx := lpFixture(t, dialect)
			audit := lpRelationID(t, ctx, conn, "LP_AUDIT")
			tests := []struct {
				name        string
				sql         string
				localLines  []string // markers of statements evaluated locally only
				unexecuted  []string // markers of statements that must not run
				skipHits    bool
				defaultKeep bool     // run with KeepRows 0 (default 10)
				stepInto    []string // procedures to step into
				failAt      string   // marker of the line a failing run must stop at (in the failing routine)
				check       func(t *testing.T, result interbase.LineProfileResult)
			}{
				{
					name:       "scalars",
					sql:        "select * from LP_SCALARS(6)",
					localLines: []string{"I = 0;", "A = 0;", "I = I + 1;", "A = A + I;", "IF (I = 2)", "IF (A > 100)", "IF (NV = 1)", "IF (NOT (NV = 1))", "A = A + N;"},
					unexecuted: []string{"A = -1;", "A = -2;", "S = 'big';"},
				},
				{
					name:       "selects",
					sql:        "select * from LP_SELECTS(1)",
					localLines: []string{"CNT = 0;", "CNT = CNT + 1;"},
					check: func(t *testing.T, result interbase.LineProfileResult) {
						if got := result.KeptRows[0][0].Value; got != "Caf\xe9" {
							t.Fatalf("first row NAME = %q, want CP1252 bytes", got)
						}
					},
				},
				{
					name: "writes",
					sql:  "select * from LP_WRITES(5)",
					check: func(t *testing.T, result interbase.LineProfileResult) {
						routine := result.Routines[0]
						insert := lpLine(t, routine.Source, "INSERT INTO LP_LOG")
						if got := routine.Lines[insert].Tables[audit].Inserts; got != 1 {
							t.Fatalf("INSERT line LP_AUDIT inserts = %d, want 1 (tables %+v)", got, routine.Lines[insert].Tables)
						}
					},
				},
				{name: "execute writes", sql: "execute procedure LP_WRITES 6", skipHits: true},
				{name: "singleton", sql: "execute procedure LP_SINGLETON"},
				// ibg-pm8: a second row is a singleton error even when a
				// row fails the conversion to its target.
				{name: "singleton, second row fails the conversion", sql: "select * from LP_PM_FIRST", failAt: "SELECT W40"},
				{name: "singleton, first row fails the conversion", sql: "select * from LP_PM_SECOND", failAt: "SELECT W40"},
				{name: "singleton from a writing procedure, a row fails the conversion", sql: "select * from LP_PM_PROC", failAt: "SELECT T FROM"},
				{name: "truncation", sql: "execute procedure LP_TRUNC"},
				{name: "UTF8 length in bytes", sql: "execute procedure LP_TRUNC8"},
				{name: "bare procedure in FROM", sql: "select * from LP_BARE_USER"},
				{name: "bare entry", sql: "select ID from LP_NOARGS"},
				{name: "numeric conversions", sql: "select * from LP_NUMS"},
				{name: "BLOB variable into another charset", sql: "select * from LP_BLOBCOPY(1)"},
				{name: "BLOB variable as procedure input", sql: "select * from LP_BLOBCALL(1)"},
				{name: "WHEN GDSCODE matches the first status code", sql: "select * from LP_GDS2", unexecuted: []string{"'second'"}},
				{name: "scaled value wider than its precision", sql: "select * from LP_WIDE_IN(0)", unexecuted: []string{"RETURNING_VALUES :N"}},
				{name: "scaled value wider than its precision, stepped", sql: "select * from LP_WIDE_IN(0)", unexecuted: []string{"RETURNING_VALUES :N"},
					stepInto: []string{"LP_WIDE_OUT"}},
				{name: "scaled value wider than its precision, overflow", sql: "select * from LP_WIDE_IN(1)", failAt: "RETURNING_VALUES :N"},
				{name: "narrowing numeric value", sql: "select * from LP_NARROW(1)"},
				{name: "narrowing numeric overflow", sql: "select * from LP_NARROW(3)"},
				{name: "returning values", sql: "select * from LP_CALLER",
					check: func(t *testing.T, result interbase.LineProfileResult) {
						routine := result.Routines[0]
						call := routine.Nodes[lpNodeAt(t, routine, "EXECUTE PROCEDURE LP_RET 21")]
						if call.Metrics.ServerCalls < 2 || !strings.HasPrefix(call.SQL, "EXECUTE PROCEDURE LP_RET") {
							t.Fatalf("EXECUTE PROCEDURE node %+v", call)
						}
						byVariable := routine.Nodes[lpNodeAt(t, routine, "EXECUTE PROCEDURE LP_RET :R")]
						if byVariable.SQL != "EXECUTE PROCEDURE LP_RET(?)" {
							t.Fatalf("variable argument SQL = %q", byVariable.SQL)
						}
					}},
				{name: "returning values in parentheses", sql: "select * from LP_CALLER_P"},
				{name: "returning values in parentheses, stepped", sql: "select * from LP_CALLER_P", stepInto: []string{"LP_RET"}},
				{name: "WHEN without errors from the callee", sql: "select * from LP_WHEN(0)",
					unexecuted: []string{"'not reached 1'", "'not reached 2'", "'-other'", "'-inner'", "'-outer'", "'-any'"},
					check:      lpCheckSavepoints},
				{name: "WHEN user exception from a native callee", sql: "select * from LP_WHEN(1)",
					unexecuted: []string{"'not reached 1'", "'not reached 2'", "'-other'", "'-inner'", "'-noraise'"},
					check:      lpCheckSavepoints},
				{name: "WHEN ANY", sql: "select * from LP_WHEN(2)",
					unexecuted: []string{"'not reached 1'", "'not reached 2'", "'-other'", "'-inner'", "'-noraise'", "'-outer'"},
					check:      lpCheckSavepoints},
				{name: "block undo and handler order", sql: "select * from LP_UNDO", unexecuted: []string{"'-a'", "'-c'"}},
				{name: "EXIT lost to a later ANY handler", sql: "execute procedure LP_LOST"},
				{name: "EXIT in a handler", sql: "execute procedure LP_EXITH", unexecuted: []string{"'-noexit'", "'-after'"}},
				{name: "step into callees", sql: "select * from LP_OUTER(2)", stepInto: []string{"LP_INNER_EXEC", "LP_INNER_SEL"},
					check: lpCheckStepTree},
				{name: "callees run natively", sql: "select * from LP_OUTER(2)"},
				{name: "step into a recursive routine", sql: "execute procedure LP_REC 5", stepInto: []string{"LP_REC"},
					check: func(t *testing.T, result interbase.LineProfileResult) {
						if len(result.Routines) != 6 || result.Routines[5].Parent != 4 {
							t.Fatalf("routines %d, want the entry and 5 nested frames", len(result.Routines))
						}
						lpCheckInclusive(t, result)
					}},
				{name: "errors from a native selectable callee", sql: "select * from LP_OUTER_W(3)", skipHits: true},
				{name: "errors from a stepped selectable callee", sql: "select * from LP_OUTER_W(3)", skipHits: true, stepInto: []string{"LP_SEL_W"}},
				{name: "singleton errors from a stepped callee", sql: "select * from LP_OUTER_W(2)", skipHits: true, stepInto: []string{"LP_SEL_W"}},
				{name: "statement errors inside a stepped callee", sql: "select * from LP_OUTER_W(0)", skipHits: true, stepInto: []string{"LP_SEL_W"}},
				// Natively the loop body runs between two rows of the callee;
				// a native cursor lets the server run the callee ahead (row
				// prefetch), so only a stepped-into callee matches here.
				{name: "caller error between rows of a stepped callee", sql: "select * from LP_OUTER_B", stepInto: []string{"LP_SEL_W"}, skipHits: true},
				{name: "handling block undo, native callees", sql: "select * from LP_HANDLING(1)", skipHits: true},
				{name: "handling block undo, stepped callees", sql: "select * from LP_HANDLING(1)", skipHits: true,
					stepInto: []string{"LP_EXEC_W", "LP_RAISE0", "LP_SEL_W"}},
				{name: "handling block, callee without error", sql: "select * from LP_HANDLING(0)", skipHits: true,
					stepInto: []string{"LP_EXEC_W", "LP_RAISE0"}},
				{name: "stepped callee closed in its own cursor loop", sql: "execute procedure LP_FIRST", stepInto: []string{"LP_SCAN"}, skipHits: true},
				{name: "text into a BLOB variable", sql: "select * from LP_TOBLOB"},
				{name: "literal text into a BLOB variable", sql: "select * from LP_BLOBSET(0)", skipHits: true},
				{name: "text variable into a BLOB variable", sql: "select * from LP_BLOBSET(1)", skipHits: true},
				{name: "integer into a BLOB variable fails on use", sql: "select * from LP_BLOBSET(2)", skipHits: true, failAt: "INSERT INTO LP_BB VALUES (10"},
				{name: "integer 0 into a BLOB variable", sql: "select * from LP_BLOBSET(3)", skipHits: true},
				{name: "numeric into a BLOB variable", sql: "select * from LP_BLOBSET(4)", skipHits: true, failAt: "X = 12.5"},
				{name: "date into a BLOB variable", sql: "select * from LP_BLOBSET(5)", skipHits: true, failAt: "X = CAST('2020"},
				{name: "concatenation into a BLOB variable", sql: "select * from LP_BLOBSET(6)", skipHits: true},
				{name: "BLOB use error caught", sql: "select * from LP_BLOBSET_W(2)", skipHits: true},
				{name: "numeric into a BLOB caught", sql: "select * from LP_BLOBSET_W(4)", skipHits: true},
				{name: "date into a BLOB caught", sql: "select * from LP_BLOBSET_W(5)", skipHits: true},
				{name: "text argument for a BLOB parameter", sql: "select * from LP_BLOBARG(0)", skipHits: true},
				{name: "text argument for a stepped BLOB parameter", sql: "select * from LP_BLOBARG(0)", skipHits: true, stepInto: []string{"LP_BLOBIN"}},
				{name: "integer argument for a BLOB parameter", sql: "select * from LP_BLOBARG(1)", skipHits: true, failAt: "EXECUTE PROCEDURE LP_BLOBIN :I"},
				{name: "integer argument for a stepped BLOB parameter", sql: "select * from LP_BLOBARG(1)", skipHits: true, stepInto: []string{"LP_BLOBIN"},
					failAt: "INSERT INTO LP_BLOBS8 (ID, B) VALUES (100"},
				{name: "text into a BLOB variable from a stepped callee", sql: "select * from LP_TOBLOB", stepInto: []string{"LP_INNER_EXEC"}},
				{name: "default kept rows", sql: "select * from LP_MANY", defaultKeep: true,
					check: func(t *testing.T, result interbase.LineProfileResult) {
						routine := result.Routines[0]
						loop := routine.Nodes[lpNodeAt(t, routine, "WHILE (I < 15)")]
						if result.Rows != 15 || len(result.KeptRows) != 10 || loop.Metrics.Hits != 16 || result.ServerCalls != 0 {
							t.Fatalf("rows %d kept %d while hits %d; want 15, 10, 16", result.Rows, len(result.KeptRows), loop.Metrics.Hits)
						}
					}},
				{name: "native procedure cursor that writes", sql: "select * from LP_MK_CALLER", check: lpCheckRowByRow(true)},
				{name: "stepped procedure cursor that writes", sql: "select * from LP_MK_CALLER", stepInto: []string{"LP_MK_GEN"}},
				{name: "native procedure cursor under a writing body", sql: "select * from LP_MK_WCALLER", check: lpCheckRowByRow(true)},
				{name: "native procedure cursor writing through a bare procedure", sql: "select * from LP_MK_VCALLER", check: lpCheckRowByRow(true)},
				{name: "native read-only procedure cursor", sql: "select * from LP_MK_RCALLER", check: lpCheckRowByRow(false)},
			}
			for _, c := range lpBlobFetchCases() {
				tests = append(tests, struct {
					name        string
					sql         string
					localLines  []string
					unexecuted  []string
					skipHits    bool
					defaultKeep bool
					stepInto    []string
					failAt      string
					check       func(t *testing.T, result interbase.LineProfileResult)
				}{name: c.name, sql: c.sql, skipHits: true, stepInto: c.stepInto, failAt: c.failAt})
			}
			for _, c := range lpBlobEdgeCases() {
				tests = append(tests, struct {
					name        string
					sql         string
					localLines  []string
					unexecuted  []string
					skipHits    bool
					defaultKeep bool
					stepInto    []string
					failAt      string
					check       func(t *testing.T, result interbase.LineProfileResult)
				}{name: c.name, sql: c.sql, skipHits: true, stepInto: c.stepInto, failAt: c.failAt})
			}
			for _, c := range lpDynamicCases() {
				tests = append(tests, struct {
					name        string
					sql         string
					localLines  []string
					unexecuted  []string
					skipHits    bool
					defaultKeep bool
					stepInto    []string
					failAt      string
					check       func(t *testing.T, result interbase.LineProfileResult)
				}{name: c.name, sql: c.sql, skipHits: true, failAt: c.failAt, check: c.check})
			}
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					nativeRows, nativeErr := lpNative(t, ctx, conn, tt.sql)
					keep := 100
					if tt.defaultKeep {
						keep = 0
					}
					result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{
						SQL: tt.sql, Dialect: dialect, KeepRows: keep, StepInto: tt.stepInto,
					})
					if err != nil {
						t.Fatalf("LineProfile(): %v", err)
					}
					if !result.RolledBack {
						t.Fatal("RolledBack = false")
					}
					if nativeErr != nil {
						if result.Outcome != interbase.LineProfileFailed || lpSQLCode(result.Err) != lpSQLCode(nativeErr) || lpSQLCode(nativeErr) == 0 {
							t.Fatalf("outcome %s err %v (SQLCODE %d); native error %v (SQLCODE %d)",
								result.Outcome, result.Err, lpSQLCode(result.Err), nativeErr, lpSQLCode(nativeErr))
						}
						if result.FailedAt == nil || result.FailedAt.Line == 0 {
							t.Fatalf("FailedAt = %+v", result.FailedAt)
						}
						if tt.failAt != "" {
							for _, r := range result.Routines {
								if r.Name == result.FailedAt.Routine && lpLine(t, r.Source, tt.failAt) != result.FailedAt.Line {
									t.Fatalf("FailedAt %+v, want the line of %q", result.FailedAt, tt.failAt)
								}
							}
							var gotRows [][]any
							for _, row := range result.KeptRows {
								gotRows = append(gotRows, lpValues(row))
							}
							if !reflect.DeepEqual(gotRows, nativeRows) {
								t.Fatalf("rows before the error %#v, native %#v", gotRows, nativeRows)
							}
						}
						lpCheckFirstHits(t, result)
						t.Logf("native and emulated SQLCODE %d at line %d", lpSQLCode(nativeErr), result.FailedAt.Line)
						return
					}
					if len(nativeRows) == 0 {
						t.Fatal("native run returned no rows")
					}
					if result.Outcome != interbase.LineProfileCompleted {
						t.Fatalf("outcome %s: %v at %+v", result.Outcome, result.Err, result.FailedAt)
					}
					var gotRows [][]any
					for _, row := range result.KeptRows {
						gotRows = append(gotRows, lpValues(row))
					}
					wantRows := nativeRows
					if len(wantRows) > len(gotRows) && tt.defaultKeep {
						wantRows = wantRows[:len(gotRows)]
					}
					if !reflect.DeepEqual(gotRows, wantRows) || result.Rows != int64(len(nativeRows)) {
						t.Fatalf("rows = %d %#v, native %#v", result.Rows, gotRows, nativeRows)
					}
					routine := result.Routines[0]
					unexecuted := make(map[int]bool)
					for _, marker := range tt.unexecuted {
						unexecuted[lpLine(t, routine.Source, marker)] = true
					}
					for _, node := range routine.Nodes {
						if tt.skipHits || node.Kind == "block" || node.Kind == "empty" {
							continue
						}
						if unexecuted[node.Line] != (node.Metrics.Hits == 0) {
							t.Fatalf("%s node at line %d hits %d (unexecuted %v)", node.Kind, node.Line, node.Metrics.Hits, unexecuted[node.Line])
						}
					}
					for _, marker := range tt.localLines {
						line := lpLine(t, routine.Source, marker)
						metrics := routine.Lines[line]
						if metrics.Hits == 0 || metrics.ServerCalls != 0 {
							t.Fatalf("local line %d (%q): %+v", line, marker, metrics)
						}
					}
					if result.Before.Samples != 15 || result.After.Samples != 15 {
						t.Fatalf("baselines %+v / %+v, server calls %d", result.Before, result.After, result.ServerCalls)
					}
					if tt.check != nil {
						tt.check(t, result)
					}
					lpCheckFirstHits(t, result)
					t.Logf("rows %d, server calls %d, round trips %d, raw %v, prepare %v, snapshots %v, setup %v",
						result.Rows, result.ServerCalls, result.RoundTrips, result.Raw, result.Prepare, result.Snapshots, result.Setup)
				})
			}
		})
	}
}

// lpCheckRowByRow checks ibg-mkx: the entry's FOR SELECT over a procedure
// is sent with FOR UPDATE (one row per fetch) exactly when it may write, and
// the first-hit times are consistent.
func lpCheckRowByRow(want bool) func(t *testing.T, result interbase.LineProfileResult) {
	return func(t *testing.T, result interbase.LineProfileResult) {
		t.Helper()
		routine := result.Routines[0]
		loop := routine.Nodes[lpNodeAt(t, routine, "FOR SELECT")]
		if got := strings.HasSuffix(loop.SQL, " FOR UPDATE"); got != want {
			t.Fatalf("FOR SELECT sent %q, want FOR UPDATE %v", loop.SQL, want)
		}
		for _, site := range result.NativeCallSites {
			t.Fatalf("unexpected native call site %+v", site)
		}
		t.Logf("FOR SELECT rows %d server calls %d round trips %d raw %v", loop.Metrics.Rows, loop.Metrics.ServerCalls, loop.Metrics.RoundTrips, loop.Metrics.Raw)
		lpCheckFirstHits(t, result)
	}
}

// lpCheckFirstHits checks the first-hit invariants: a node's first hit is
// part of its total (all of it for a single hit), a line's is the sum of its
// nodes', and routine totals have none.
func lpCheckFirstHits(t *testing.T, result interbase.LineProfileResult) {
	t.Helper()
	for _, routine := range result.Routines {
		lines := make(map[int][2]time.Duration)
		for _, node := range routine.Nodes {
			m := node.Metrics
			if m.FirstRaw > m.Raw || m.FirstAdjusted > m.Adjusted || m.FirstAdjusted > m.FirstRaw ||
				m.Hits == 1 && (m.FirstRaw != m.Raw || m.FirstAdjusted != m.Adjusted) ||
				m.Hits == 0 && (m.FirstRaw != 0 || m.FirstAdjusted != 0) {
				t.Fatalf("%s node at line %d: hits %d first %v/%v total %v/%v", node.Kind, node.Line, m.Hits, m.FirstRaw, m.FirstAdjusted, m.Raw, m.Adjusted)
			}
			first := lines[node.Line]
			lines[node.Line] = [2]time.Duration{first[0] + m.FirstRaw, first[1] + m.FirstAdjusted}
		}
		for line, first := range lines {
			if got := routine.Lines[line]; got.FirstRaw != first[0] || got.FirstAdjusted != first[1] {
				t.Fatalf("line %d first %v/%v, want %v/%v", line, got.FirstRaw, got.FirstAdjusted, first[0], first[1])
			}
		}
		if routine.Inclusive.FirstRaw != 0 || routine.Exclusive.FirstRaw != 0 ||
			routine.Inclusive.FirstAdjusted != 0 || routine.Exclusive.FirstAdjusted != 0 {
			t.Fatalf("routine %s totals have first-hit times", routine.Name)
		}
	}
}

func TestLineProfileAnalysis(t *testing.T) {
	conn, ctx := lpFixture(t, 3)
	for _, tt := range []struct {
		sql      string
		mayWrite bool
		problem  string
		callee   string
	}{
		{sql: "select * from LP_SCALARS(1)"},
		{sql: "select * from LP_SELECTS(1)"},
		{sql: "select * from LP_WRITES(1)", mayWrite: true, callee: "LP_NOOP"},
		{sql: "select * from LP_BARE_USER", mayWrite: true, callee: "LP_NOARGS"},
		{sql: "select * from LP_WHEN(0)", mayWrite: true, callee: "LP_RAISE"},
		{sql: "select * from LP_EXC(0)"},
		{sql: "select * from LP_ES_FOR", mayWrite: true},
		{sql: "select * from LP_ITEMS", problem: "table or view"},
		{sql: "select * from LP_MISSING(1)", problem: "does not exist"},
		{sql: "select * from LP_SCALARS", problem: "0 arguments"},
		{sql: "select NOPE from LP_SCALARS(1)", problem: "not an output parameter"},
		{sql: "select * from LP_SCALARS(?)", problem: "parameter"},
	} {
		t.Run(tt.sql, func(t *testing.T) {
			analysis, err := interbase.AnalyzeLineProfile(ctx, conn, tt.sql, 3, nil)
			if err != nil {
				t.Fatalf("AnalyzeLineProfile(): %v", err)
			}
			if tt.problem != "" {
				if len(analysis.Problems) == 0 || !strings.Contains(analysis.Problems[0].Message, tt.problem) {
					t.Fatalf("problems = %+v, want %q", analysis.Problems, tt.problem)
				}
				if _, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{SQL: tt.sql, Dialect: 3}); !errors.Is(err, interbase.ErrLineProfileRefused) {
					t.Fatalf("LineProfile() error = %v, want ErrLineProfileRefused", err)
				}
				return
			}
			if len(analysis.Problems) != 0 || analysis.MayWrite != tt.mayWrite {
				t.Fatalf("analysis = %+v", analysis)
			}
			if tt.callee != "" && (len(analysis.Callees) != 1 || analysis.Callees[0].Name != tt.callee || !analysis.Callees[0].Parses) {
				t.Fatalf("callees = %+v, want %s", analysis.Callees, tt.callee)
			}
		})
	}
}

// TestLineProfileBlobDateMessage checks ibg-b6o: assigning a date or time to
// a BLOB variable fails with the server's message, the value written as the
// server writes it in each dialect.
func TestLineProfileBlobDateMessage(t *testing.T) {
	for _, dialect := range []int{3, 1} {
		t.Run(fmt.Sprintf("dialect %d", dialect), func(t *testing.T) {
			conn, ctx := lpFixture(t, dialect)
			for k := 1; k <= 3; k++ {
				query := fmt.Sprintf("select * from LP_BDATE(%d)", k)
				_, nativeErr := lpNative(t, ctx, conn, query)
				result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{SQL: query, Dialect: dialect})
				var native, emulated *interbase.Error
				if err != nil || !errors.As(nativeErr, &native) || !errors.As(result.Err, &emulated) {
					t.Fatalf("%s: emulated %v (%v), native %v", query, result.Err, err, nativeErr)
				}
				if emulated.SQLCode != native.SQLCode || emulated.Message != native.Message {
					t.Fatalf("%s: emulated %q (SQLCODE %d), native %q (SQLCODE %d)", query, emulated.Message, emulated.SQLCode, native.Message, native.SQLCode)
				}
			}
		})
	}
}

// TestLineProfileSingletonCastRetry checks ibg-pm8 beyond the SQLCODE (see
// the differential cases): a failing in-statement CAST is retried without
// it and counts one error, and a singleton SELECT that cannot run twice is
// never cast in the statement.
func TestLineProfileSingletonCastRetry(t *testing.T) {
	conn, ctx := lpFixture(t, 3)
	for _, tt := range []struct {
		query, marker string
		wantSQL       string
		calls         int64 // execute and fetches: the CAST statement's and the plain one's, or the plain one's only
	}{
		{"select * from LP_PM_FIRST", "SELECT W40", "SELECT W40 FROM LP_TEXTS WHERE ID IN (1, 3) ORDER BY ID", 6},
		{"select * from LP_PM_PROC", "SELECT T FROM", "SELECT T FROM LP_PM_SRC(900)", 3},
	} {
		result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{SQL: tt.query, Dialect: 3})
		if err != nil || lpSQLCode(result.Err) != -811 {
			t.Fatalf("%s: %v / %v", tt.query, result.Err, err)
		}
		routine := result.Routines[0]
		node := routine.Nodes[lpNodeAt(t, routine, tt.marker)]
		if node.SQL != tt.wantSQL || node.Metrics.Errors != 1 || node.Metrics.Rows != 2 || node.Metrics.ServerCalls != tt.calls {
			t.Fatalf("%s: node SQL %q errors %d rows %d calls %d; want the plain statement, one error, two rows, %d calls",
				tt.query, node.SQL, node.Metrics.Errors, node.Metrics.Rows, node.Metrics.ServerCalls, tt.calls)
		}
	}
}

// TestLineProfileExceptionMessageBytes checks ibg-96o: RDB$EXCEPTIONS
// messages are CHARACTER SET NONE, so a CP1252 byte in one cannot be
// transliterated to a UTF8 attachment; the analysis reads the messages as
// bytes and the emulated EXCEPTION raises the same message as the server.
func TestLineProfileExceptionMessageBytes(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3, "")
	setup := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "NONE", 3,
		interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	ctx := readContextWithTimeout(t, 3*time.Minute)
	for _, statement := range []string{
		"CREATE EXCEPTION LP_X8 'Caf\xe9 failure'",
		"CREATE PROCEDURE LP_X8R (K INTEGER) RETURNS (R INTEGER) AS BEGIN R = K; SUSPEND; IF (K > 0) THEN EXCEPTION LP_X8; END",
	} {
		if _, err := setup.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for _, charset := range []string{"UTF8", "NONE"} {
		t.Run(charset, func(t *testing.T) {
			db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, charset, 3,
				interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("db.Conn(): %v", err)
			}
			defer conn.Close()
			const query = "select * from LP_X8R(1)"
			analysis, err := interbase.AnalyzeLineProfile(ctx, conn, query, 3, nil)
			if err != nil || len(analysis.Problems) != 0 {
				t.Fatalf("AnalyzeLineProfile() = %+v, %v", analysis.Problems, err)
			}
			_, nativeErr := lpNative(t, ctx, conn, query)
			result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{SQL: query, Dialect: 3})
			var native, emulated *interbase.Error
			if err != nil || !errors.As(nativeErr, &native) || !errors.As(result.Err, &emulated) {
				t.Fatalf("LineProfile() = %v (%v); native %v", result.Err, err, nativeErr)
			}
			if emulated.SQLCode != native.SQLCode || emulated.Message != native.Message || !strings.Contains(native.Message, "Caf\xe9") {
				t.Fatalf("emulated %q (SQLCODE %d), native %q (SQLCODE %d)", emulated.Message, emulated.SQLCode, native.Message, native.SQLCode)
			}
		})
	}
}

func TestLineProfileCancel(t *testing.T) {
	conn, ctx := lpFixture(t, 3)
	runCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	result, err := interbase.LineProfile(runCtx, conn, interbase.LineProfileOptions{SQL: "select * from LP_SLOW", Dialect: 3})
	if err != nil {
		t.Fatalf("LineProfile(): %v", err)
	}
	if result.Outcome != interbase.LineProfileCanceled || !result.RolledBack {
		t.Fatalf("outcome %s rolled back %v err %v", result.Outcome, result.RolledBack, result.Err)
	}
	routine := result.Routines[0]
	loop := lpLine(t, routine.Source, "FOR SELECT")
	if routine.Lines[loop].ServerCalls == 0 || result.ServerCalls == 0 || result.After.Samples != 15 {
		t.Fatalf("partial metrics missing: line %+v, calls %d, after %+v", routine.Lines[loop], result.ServerCalls, result.After)
	}
	var one int
	if err := conn.QueryRowContext(ctx, "SELECT 1 FROM RDB$DATABASE").Scan(&one); err != nil || one != 1 {
		t.Fatalf("query after cancel = %d, %v", one, err)
	}
}

func TestLineProfileReadOnly(t *testing.T) {
	conn, ctx := lpFixture(t, 3)
	result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{
		SQL: "select * from LP_SELECTS(2)", Dialect: 3, ReadOnly: true,
	})
	if err != nil || result.Outcome != interbase.LineProfileCompleted || !result.ReadOnly || !result.RolledBack {
		t.Fatalf("read-only run: %+v, %v", result, err)
	}
	result, err = interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{
		SQL: "select * from LP_WRITES(1)", Dialect: 3, ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("LineProfile(): %v", err)
	}
	if result.Outcome != interbase.LineProfileFailed || result.FailedAt == nil ||
		result.FailedAt.Line != lpLine(t, result.Routines[0].Source, "INSERT INTO LP_LOG") || !result.RolledBack {
		t.Fatalf("write in read-only transaction: outcome %s at %+v: %v", result.Outcome, result.FailedAt, result.Err)
	}
}

// lpCheckStepTree checks the routine tree of LP_OUTER stepping into
// LP_INNER_EXEC and LP_INNER_SEL.
func lpCheckStepTree(t *testing.T, result interbase.LineProfileResult) {
	t.Helper()
	outer := result.Routines[0].Source
	var inner string
	for _, r := range result.Routines {
		if r.Name == "LP_INNER_SEL" {
			inner = r.Source
		}
	}
	execLine := lpLine(t, outer, "EXECUTE PROCEDURE LP_INNER_EXEC")
	forLine := lpLine(t, outer, "FOR SELECT I, T")
	singleLine := lpLine(t, outer, "SELECT I, T FROM LP_INNER_SEL(1)")
	innerLine := lpLine(t, inner, "EXECUTE PROCEDURE LP_INNER_EXEC")
	want := []struct {
		name         string
		parent, line int
		calls        int64
	}{
		{"LP_OUTER", -1, 0, 1},
		{"LP_INNER_EXEC", 0, execLine, 2},
		{"LP_INNER_SEL", 0, forLine, 1},
		{"LP_INNER_EXEC", 2, innerLine, 2},
		{"LP_INNER_SEL", 0, singleLine, 1},
		{"LP_INNER_EXEC", 4, innerLine, 1},
	}
	if len(result.Routines) != len(want) {
		t.Fatalf("%d routines, want %d", len(result.Routines), len(want))
	}
	for i, w := range want {
		r := result.Routines[i]
		if r.Name != w.name || r.Parent != w.parent || r.CallLine != w.line || r.Calls != w.calls {
			t.Fatalf("routine %d = %s parent %d line %d calls %d; want %+v", i, r.Name, r.Parent, r.CallLine, r.Calls, w)
		}
	}
	maxLine := lpLine(t, outer, "SELECT MAX(I)")
	if sites := result.NativeCallSites; len(sites) != 1 || sites[0].Routine != "LP_OUTER" || sites[0].Line != maxLine ||
		sites[0].Procedure != "LP_INNER_SEL" || sites[0].Reason == "" {
		t.Fatalf("native call sites %+v, want the MAX line %d", sites, maxLine)
	}
	if hits := result.Routines[2].Lines[lpLine(t, inner, "SUSPEND")].Hits; hits != 2 {
		t.Fatalf("LP_INNER_SEL SUSPEND hits %d, want 2", hits)
	}
	lpCheckInclusive(t, result)
}

// lpCheckInclusive checks spec 7: a routine's inclusive metrics are its
// exclusive ones plus its callees' inclusive ones.
func lpCheckInclusive(t *testing.T, result interbase.LineProfileResult) {
	t.Helper()
	for i, r := range result.Routines {
		raw, adjusted, hits := r.Exclusive.Raw, r.Exclusive.Adjusted, r.Exclusive.Hits
		for _, child := range result.Routines {
			if child.Parent == i {
				raw, adjusted, hits = raw+child.Inclusive.Raw, adjusted+child.Inclusive.Adjusted, hits+child.Inclusive.Hits
			}
		}
		if r.Inclusive.Raw != raw || r.Inclusive.Adjusted != adjusted || r.Inclusive.Hits != hits {
			t.Fatalf("routine %d %s inclusive %v/%v/%d, want %v/%v/%d", i, r.Name,
				r.Inclusive.Raw, r.Inclusive.Adjusted, r.Inclusive.Hits, raw, adjusted, hits)
		}
	}
	if result.Raw != result.Routines[0].Inclusive.Raw {
		t.Fatalf("run raw %v, entry inclusive %v", result.Raw, result.Routines[0].Inclusive.Raw)
	}
}

// TestLineProfileStepIntoLimits checks the step-in depth limit and the
// cancellation of a run inside a stepped-into selectable routine.
func TestLineProfileStepIntoLimits(t *testing.T) {
	conn, ctx := lpFixture(t, 3)
	result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{
		SQL: "execute procedure LP_REC 40", Dialect: 3, StepInto: []string{"LP_REC"},
	})
	if err != nil || result.Outcome != interbase.LineProfileFailed || !result.RolledBack ||
		result.Err == nil || !strings.Contains(result.Err.Error(), "depth limit of 32") || len(result.Routines) != 33 {
		t.Fatalf("depth 41: outcome %s err %v routines %d rolled back %v (%v)", result.Outcome, result.Err, len(result.Routines), result.RolledBack, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	result, err = interbase.LineProfile(runCtx, conn, interbase.LineProfileOptions{
		SQL: "select * from LP_SLOW_CALLER", Dialect: 3, StepInto: []string{"LP_SLOW2"},
	})
	if err != nil || result.Outcome != interbase.LineProfileCanceled || !result.RolledBack || len(result.Routines) != 2 {
		t.Fatalf("outcome %s rolled back %v routines %d err %v / %v", result.Outcome, result.RolledBack, len(result.Routines), result.Err, err)
	}
	callee := result.Routines[1]
	if loop := callee.Lines[lpLine(t, callee.Source, "FOR SELECT")]; loop.ServerCalls == 0 {
		t.Fatalf("partial callee metrics missing: %+v", loop)
	}
	var one int
	if err := conn.QueryRowContext(ctx, "SELECT 1 FROM RDB$DATABASE").Scan(&one); err != nil || one != 1 {
		t.Fatalf("query after cancel = %d, %v", one, err)
	}
}

func lpCheckSavepoints(t *testing.T, result interbase.LineProfileResult) {
	t.Helper()
	routine := result.Routines[0]
	duplicate := routine.Nodes[lpNodeAt(t, routine, "INSERT INTO LP_UNIQ (ID) VALUES (1);\n    R =")]
	if result.Savepoints <= 0 || duplicate.Metrics.Errors != 1 {
		t.Fatalf("savepoints %v, duplicate insert %+v", result.Savepoints, duplicate.Metrics)
	}
}

func TestLineProfileUnhandledException(t *testing.T) {
	for _, dialect := range []int{3, 1} {
		t.Run(fmt.Sprintf("dialect %d", dialect), func(t *testing.T) {
			conn, ctx := lpFixture(t, dialect)
			const query = "select * from LP_EXC(1)"
			nativeRows, nativeErr := lpNative(t, ctx, conn, query)
			result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{SQL: query, Dialect: dialect})
			if err != nil {
				t.Fatalf("LineProfile(): %v", err)
			}
			var native, emulated *interbase.Error
			if !errors.As(nativeErr, &native) || !errors.As(result.Err, &emulated) {
				t.Fatalf("native error %v, emulated error %v", nativeErr, result.Err)
			}
			nativeNumber, nativeOK := native.ExceptionNumber()
			number, ok := emulated.ExceptionNumber()
			if result.Outcome != interbase.LineProfileFailed || !ok || !nativeOK || number != nativeNumber ||
				emulated.SQLCode != native.SQLCode || emulated.GDSCode() != native.GDSCode() || emulated.Message != native.Message {
				t.Fatalf("outcome %s, emulated %v (exception %d %v); native %v (exception %d)",
					result.Outcome, result.Err, number, ok, nativeErr, nativeNumber)
			}
			if !strings.Contains(emulated.Error(), "LP_EX") {
				t.Fatalf("emulated error %q does not name the exception", emulated.Error())
			}
			line := lpLine(t, result.Routines[0].Source, "EXCEPTION LP_EX;")
			if result.FailedAt == nil || result.FailedAt.Line != line {
				t.Fatalf("FailedAt = %+v, want line %d", result.FailedAt, line)
			}
			if len(result.KeptRows) != len(nativeRows) || len(nativeRows) != 1 {
				t.Fatalf("rows before the exception: emulated %d, native %d", len(result.KeptRows), len(nativeRows))
			}
		})
	}
}

// TestLineProfileConversionRoundTrips checks R18: SELECT INTO / FOR SELECT
// targets are converted locally or in the statement itself, never with a
// server call per row, unless the statement shape forbids rewriting.
func TestLineProfileConversionRoundTrips(t *testing.T) {
	for _, dialect := range []int{3, 1} {
		t.Run(fmt.Sprintf("dialect %d", dialect), func(t *testing.T) {
			conn, ctx := lpFixture(t, dialect)
			run := func(t *testing.T, query string) (interbase.LineProfileResult, [][]any, error) {
				t.Helper()
				nativeRows, nativeErr := lpNative(t, ctx, conn, query)
				result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{SQL: query, Dialect: dialect, KeepRows: 100})
				if err != nil {
					t.Fatalf("LineProfile(): %v", err)
				}
				var gotRows [][]any
				for _, row := range result.KeptRows {
					gotRows = append(gotRows, lpValues(row))
				}
				if !reflect.DeepEqual(gotRows, nativeRows) {
					t.Fatalf("rows %#v, native %#v", gotRows, nativeRows)
				}
				return result, nativeRows, nativeErr
			}

			t.Run("fitting values", func(t *testing.T) {
				result, _, nativeErr := run(t, "select * from LP_R18(2)")
				if nativeErr != nil || result.Outcome != interbase.LineProfileCompleted {
					t.Fatalf("native %v; outcome %s: %v", nativeErr, result.Outcome, result.Err)
				}
				routine := result.Routines[0]
				for _, marker := range []string{"FOR SELECT T40", "FOR SELECT W40", "FOR SELECT U40", "FOR SELECT P4"} {
					node := routine.Nodes[lpNodeAt(t, routine, marker)]
					if node.Metrics.Rows == 0 || node.Metrics.ServerCalls != node.Metrics.Rows+2 {
						t.Fatalf("%s: server calls %d for %d rows; want execute and fetches only (SQL %q)",
							marker, node.Metrics.ServerCalls, node.Metrics.Rows, node.SQL)
					}
				}
				if node := routine.Nodes[lpNodeAt(t, routine, "FOR SELECT T40")]; strings.Contains(node.SQL, "CAST(T40") || node.Local != node.Metrics.Rows {
					t.Fatalf("NONE text node SQL %q, local %d for %d rows; want local conversions", node.SQL, node.Local, node.Metrics.Rows)
				}
				for _, column := range []string{"W40", "U40", "P4"} {
					if node := routine.Nodes[lpNodeAt(t, routine, "FOR SELECT "+column)]; !strings.Contains(node.SQL, "CAST("+column+" AS") {
						t.Fatalf("%s SQL %q has no in-statement CAST", column, node.SQL)
					}
				}
				distinct := routine.Nodes[lpNodeAt(t, routine, "FOR SELECT DISTINCT")]
				if strings.Contains(distinct.SQL, "CAST(P4") || distinct.Metrics.ServerCalls <= distinct.Metrics.Rows+2 {
					t.Fatalf("DISTINCT node SQL %q, %d calls for %d rows; want per-row fallback",
						distinct.SQL, distinct.Metrics.ServerCalls, distinct.Metrics.Rows)
				}
			})
			// Blank-padded CHAR(n) values into shorter VARCHAR targets (the
			// acceptance routine's shape): no conversion call per value.
			for _, tt := range []struct{ query, marker string }{
				{"select * from LP_PAD_INTO(1)", "SELECT C3, C10"},
				{"select * from LP_PAD_INTO(2)", "SELECT C3, C10"},
				{"select * from LP_PAD_FOR", "FOR SELECT C3"},
			} {
				t.Run(tt.query, func(t *testing.T) {
					result, _, nativeErr := run(t, tt.query)
					if nativeErr != nil || result.Outcome != interbase.LineProfileCompleted {
						t.Fatalf("native %v; outcome %s: %v", nativeErr, result.Outcome, result.Err)
					}
					routine := result.Routines[0]
					node := routine.Nodes[lpNodeAt(t, routine, tt.marker)]
					if node.Metrics.Rows == 0 || node.Metrics.ServerCalls != node.Metrics.Rows+2 {
						t.Fatalf("server calls %d for %d rows; want execute and fetches only (SQL %q)",
							node.Metrics.ServerCalls, node.Metrics.Rows, node.SQL)
					}
					if strings.HasPrefix(tt.query, "select * from LP_PAD_INTO") &&
						(!strings.Contains(node.SQL, "CAST(CASE WHEN") || !strings.Contains(node.SQL, " AS NUMERIC(15,2)) AS PP")) {
						t.Fatalf("CASE and aliased items not cast in the statement: %q", node.SQL)
					}
					t.Logf("SQL %q, local %d", node.SQL, node.Local)
				})
			}
			for _, tt := range []struct{ query, marker string }{
				{"select * from LP_PAD_INTO(3)", "SELECT C3, C10"},
				{"select * from LP_PAD_INTO(4)", "SELECT C3, C10"},
			} {
				t.Run(tt.query, func(t *testing.T) {
					result, _, nativeErr := run(t, tt.query)
					if lpSQLCode(nativeErr) != -802 || lpSQLCode(result.Err) != -802 ||
						result.FailedAt == nil || result.FailedAt.Line != lpLine(t, result.Routines[0].Source, tt.marker) {
						t.Fatalf("native %v; emulated %v at %+v", nativeErr, result.Err, result.FailedAt)
					}
				})
			}
			t.Run("UTF8 length in bytes", func(t *testing.T) {
				if result, _, nativeErr := run(t, "select * from LP_R18U(3)"); nativeErr != nil || result.Outcome != interbase.LineProfileCompleted {
					t.Fatalf("native %v; outcome %s: %v", nativeErr, result.Outcome, result.Err)
				}
			})
			for _, tt := range []struct {
				name, query, marker string
				before              int
			}{
				{"too long, local path", "select * from LP_R18(3)", "FOR SELECT T40", 2},
				{"too long, in-statement CAST", "select * from LP_R18W(3)", "FOR SELECT W40", 2},
				{"too many UTF8 bytes, in-statement CAST", "select * from LP_R18U(4)", "FOR SELECT U40", 3},
			} {
				t.Run(tt.name, func(t *testing.T) {
					result, nativeRows, nativeErr := run(t, tt.query)
					if lpSQLCode(nativeErr) != -802 || lpSQLCode(result.Err) != -802 || result.Outcome != interbase.LineProfileFailed {
						t.Fatalf("native %v; outcome %s: %v", nativeErr, result.Outcome, result.Err)
					}
					if line := lpLine(t, result.Routines[0].Source, tt.marker); result.FailedAt == nil || result.FailedAt.Line != line {
						t.Fatalf("FailedAt %+v, want line %d", result.FailedAt, line)
					}
					if len(nativeRows) != tt.before {
						t.Fatalf("native rows before the error = %d, want %d", len(nativeRows), tt.before)
					}
				})
			}
		})
	}
}

// TestLineProfilePrepareCountersWideTable checks that the catalog reads the
// server does to prepare a statement over a wide table are overhead: the
// singleton select's node counts only its own index read, on every hit.
func TestLineProfilePrepareCountersWideTable(t *testing.T) {
	for _, dialect := range []int{3, 1} {
		t.Run(fmt.Sprintf("dialect %d", dialect), func(t *testing.T) {
			fixture, cfg, cleanup := createFixture(t, dialect, "")
			ctx := readContextWithTimeout(t, 3*time.Minute)
			setup := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "NONE", dialect,
				interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
			columns := make([]string, 0, 300)
			columns = append(columns, "K INTEGER NOT NULL PRIMARY KEY")
			for i := 1; i < 300; i++ {
				columns = append(columns, fmt.Sprintf("C%d INTEGER", i))
			}
			for _, statement := range []string{
				"CREATE TABLE LP_WIDE (" + strings.Join(columns, ", ") + ")",
				"INSERT INTO LP_WIDE (K, C1, C2, C3, C4, C5, C6, C7) VALUES (1, 1, 2, 3, 4, 5, 6, 7)",
				`CREATE PROCEDURE LP_WIDE_GET (K INTEGER)
RETURNS (C1 INTEGER, C2 INTEGER, C3 INTEGER, C4 INTEGER, C5 INTEGER, C6 INTEGER, C7 INTEGER) AS
DECLARE VARIABLE I INTEGER;
BEGIN
  I = 0;
  WHILE (I < 3) DO
  BEGIN
    I = I + 1;
    SELECT C1, C2, C3, C4, C5, C6, C7 FROM LP_WIDE WHERE K = :K INTO :C1, :C2, :C3, :C4, :C5, :C6, :C7;
  END
  SUSPEND;
END`,
			} {
				if _, err := setup.ExecContext(ctx, statement); err != nil {
					t.Fatalf("%s: %v", strings.SplitN(statement, "\n", 2)[0], err)
				}
			}
			// Profile on a fresh attachment, so the statement's prepare
			// loads the table's metadata.
			db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "NONE", dialect,
				interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("db.Conn(): %v", err)
			}
			defer conn.Close()
			wide := lpRelationID(t, ctx, conn, "LP_WIDE")

			result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{
				SQL: "select * from LP_WIDE_GET(1)", Dialect: dialect,
			})
			if err != nil || result.Outcome != interbase.LineProfileCompleted || result.Rows != 1 {
				t.Fatalf("LineProfile() = %s rows %d: %v / %v", result.Outcome, result.Rows, result.Err, err)
			}
			routine := result.Routines[0]
			node := routine.Nodes[lpNodeAt(t, routine, "SELECT C1, C2")]
			// Three hits: every hit reads LP_WIDE once and no hit reads the
			// catalog.
			want := map[int]interbase.TableCounters{wide: {IdxReads: 3}}
			if node.Metrics.Hits != 3 || !reflect.DeepEqual(node.Metrics.Tables, want) {
				t.Fatalf("singleton select hits %d tables = %+v, want only LP_WIDE with one index read per hit %+v (overhead %+v)",
					node.Metrics.Hits,
					node.Metrics.Tables, want, result.OverheadActivity.Tables)
			}
			if len(result.OverheadActivity.Tables) == 0 {
				t.Fatal("OverheadActivity has no catalog reads; the prepare's work was not measured")
			}
			t.Logf("overhead tables %v", result.OverheadActivity.Tables)
		})
	}
}

// TestLineProfileKeptBlobRows checks that a kept BLOB output is read before
// the rollback. (A native SELECT from a selectable procedure with a text
// BLOB output fails in the driver with SQLCODE -205, so this is not a
// differential case.)
func TestLineProfileKeptBlobRows(t *testing.T) {
	conn, ctx := lpFixture(t, 3)
	result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{SQL: "select * from LP_BLOBOUT", Dialect: 3})
	if err != nil || result.Outcome != interbase.LineProfileCompleted {
		t.Fatalf("LineProfile() = %s: %v / %v", result.Outcome, result.Err, err)
	}
	if result.Rows != 1 || len(result.KeptRows) != 1 || result.KeptRows[0][0].Value != "hello blob" {
		t.Fatalf("kept rows %#v (rows %d), want the BLOB text", result.KeptRows, result.Rows)
	}
}

// TestLineProfileBlobCap checks LineProfileOptions.MaxBlobBytes: a BLOB at
// the cap is held whole; a larger one fails the run at its line, even
// inside a WHEN ANY block, and is never truncated.
func TestLineProfileBlobCap(t *testing.T) {
	conn, ctx := lpFixture(t, 3)
	const length = int64(len("hello blob"))
	for _, tt := range []struct {
		max   int64
		fails bool
	}{{length, false}, {length - 1, true}, {0, false}} {
		result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{
			SQL: "select * from LP_BLOBCAP", Dialect: 3, MaxBlobBytes: tt.max,
		})
		if err != nil {
			t.Fatalf("LineProfile(max %d): %v", tt.max, err)
		}
		if !tt.fails {
			if result.Outcome != interbase.LineProfileCompleted || len(result.KeptRows) != 1 || fmt.Sprint(result.KeptRows[0][0].Value) != "1" {
				t.Fatalf("max %d: outcome %s rows %#v err %v", tt.max, result.Outcome, result.KeptRows, result.Err)
			}
			continue
		}
		want := fmt.Sprintf("BLOB too large to emulate (more than %d bytes)", tt.max)
		if result.Outcome != interbase.LineProfileFailed || result.Err == nil || !strings.Contains(result.Err.Error(), want) ||
			result.FailedAt == nil || result.FailedAt.Line != lpLine(t, result.Routines[0].Source, "SELECT B FROM LP_BLOBS") ||
			result.Rows != 0 || !result.RolledBack {
			t.Fatalf("max %d: outcome %s at %+v rows %d err %v; want failed at the SELECT, not caught",
				tt.max, result.Outcome, result.FailedAt, result.Rows, result.Err)
		}
	}
}

// lpBlobShapes are the fetch shapes that assign a value of each source
// column (I integer, Z integer 0, N NUMERIC(15,2), D DATE) to a BLOB
// variable.
var lpBlobShapes = map[string]string{
	"S": "SELECT %[1]s FROM LP_BSRC WHERE ID = 1 INTO :X;",
	"F": "FOR SELECT %[1]s FROM LP_BSRC WHERE ID = 1 INTO :X DO R = R || '-row';",
	"E": "EXECUTE PROCEDURE LP_BOUT_%[1]s RETURNING_VALUES :X;",
}

// lpBlobFetchDDL creates LP_BF_<shape><column> (fetch, then use the BLOB)
// and LP_BFW_<shape><column> (the same with WHEN ANY around each step).
func lpBlobFetchDDL() []string {
	ddl := []string{
		"CREATE TABLE LP_BSRC (ID INTEGER, I INTEGER, Z INTEGER, N NUMERIC(15,2), D DATE)",
		"INSERT INTO LP_BSRC VALUES (1, 42, 0, 12.5, '2020-01-02')",
		"CREATE PROCEDURE LP_BOUT_I RETURNS (V INTEGER) AS BEGIN V = 42; END",
		"CREATE PROCEDURE LP_BOUT_Z RETURNS (V INTEGER) AS BEGIN V = 0; END",
		"CREATE PROCEDURE LP_BOUT_N RETURNS (V NUMERIC(15,2)) AS BEGIN V = 12.5; END",
		"CREATE PROCEDURE LP_BOUT_D RETURNS (V DATE) AS BEGIN V = '2020-01-02'; END",
	}
	for _, shape := range []string{"S", "F", "E"} {
		for _, column := range []string{"I", "Z", "N", "D"} {
			fetch := fmt.Sprintf(lpBlobShapes[shape], column)
			ddl = append(ddl, fmt.Sprintf(`CREATE PROCEDURE LP_BF_%s%s RETURNS (R VARCHAR(60)) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  R = 'start';
  %s
  R = R || '-fetched';
  SUSPEND;
  INSERT INTO LP_BB VALUES (20, :X);
  SELECT '[' || CAST(B AS VARCHAR(40)) || ']' FROM LP_BB WHERE ID = 20 INTO :R;
  SUSPEND;
END`, shape, column, fetch), fmt.Sprintf(`CREATE PROCEDURE LP_BFW_%s%s RETURNS (R VARCHAR(60)) AS
DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;
BEGIN
  R = 'start';
  BEGIN
    %s
    R = R || '-fetched';
  WHEN ANY DO R = R || '-caught';
  END
  SUSPEND;
  BEGIN
    INSERT INTO LP_BB VALUES (21, :X);
    R = R || '-used';
  WHEN ANY DO R = R || '-usecaught';
  END
  SUSPEND;
END`, shape, column, fetch))
		}
	}
	return ddl
}

type lpBlobFetchCase struct {
	name, sql, failAt string
	stepInto          []string
}

// lpBlobFetchCases are the differential cases over lpBlobFetchDDL, as
// probed natively: an integer is accepted and fails with SQLCODE -901 when
// the BLOB is used, 0 is an empty BLOB, NUMERIC fails at the fetch with -902
// and DATE with -413; WHEN ANY catches each. EXECUTE PROCEDURE runs natively
// and stepped into.
func lpBlobFetchCases() []lpBlobFetchCase {
	var cases []lpBlobFetchCase
	for _, shape := range []string{"S", "F", "E"} {
		for _, column := range []string{"I", "Z", "N", "D"} {
			failAt := ""
			switch column {
			case "I":
				failAt = "INSERT INTO LP_BB VALUES (20"
			case "N", "D":
				failAt = strings.TrimSuffix(fmt.Sprintf(lpBlobShapes[shape], column), ";")
			}
			steps := [][]string{nil}
			if shape == "E" {
				steps = append(steps, []string{"LP_BOUT_" + column})
			}
			for _, step := range steps {
				suffix := ""
				if step != nil {
					suffix = " stepped"
				}
				cases = append(cases,
					lpBlobFetchCase{name: "BLOB fetch " + shape + column + suffix, sql: "select * from LP_BF_" + shape + column, failAt: failAt, stepInto: step},
					lpBlobFetchCase{name: "BLOB fetch caught " + shape + column + suffix, sql: "select * from LP_BFW_" + shape + column, stepInto: step})
			}
		}
	}
	return cases
}

// lpBlobEdgeCases are the differential cases of ibg-b6o, as probed
// natively: BLOB content is written without transliteration; a CHAR value
// fills a BLOB with its full width in bytes; a BLOB variable or output
// parameter assigned to a text variable fails with SQLCODE -902, while a
// BLOB column selected into one is cut to its length; IS NULL on a BLOB
// variable holding an integer is false; a date assigned to a BLOB fails
// with SQLCODE -413.
func lpBlobEdgeCases() []lpBlobFetchCase {
	cases := []lpBlobFetchCase{
		{name: "BLOB edge: bad BLOB IS NULL", sql: "select * from LP_BISNULL"},
		{name: "BLOB edge: text BLOB from a stepped procedure cut to a text target", sql: "select * from LP_BTEXT(4)", stepInto: []string{"LP_BSEL_T"}},
		{name: "BLOB edge: text BLOB rows from a stepped procedure", sql: "select * from LP_BTEXT(5)", stepInto: []string{"LP_BSEL_T"}},
		{name: "BLOB edge: BLOB output parameter into text, stepped", sql: "select * from LP_BTEXT(2)", stepInto: []string{"LP_BOUT_T"},
			failAt: "IF (K = 2)"},
	}
	for k := 1; k <= 2; k++ {
		cases = append(cases, lpBlobFetchCase{name: fmt.Sprintf("BLOB edge: raw bytes %d", k), sql: fmt.Sprintf("select * from LP_BRAW(%d)", k)})
	}
	for k := 1; k <= 5; k++ {
		cases = append(cases, lpBlobFetchCase{name: fmt.Sprintf("BLOB edge: CHAR width %d", k), sql: fmt.Sprintf("select * from LP_BCHAR(%d)", k)})
	}
	for k := 0; k <= 6; k++ {
		failAt := ""
		if k >= 1 && k <= 3 {
			failAt = fmt.Sprintf("IF (K = %d)", k)
		}
		cases = append(cases, lpBlobFetchCase{name: fmt.Sprintf("BLOB edge: BLOB into text %d", k), sql: fmt.Sprintf("select * from LP_BTEXT(%d)", k), failAt: failAt})
	}
	for k := 1; k <= 4; k++ {
		cases = append(cases, lpBlobFetchCase{name: fmt.Sprintf("BLOB edge: exact numeric %d", k), sql: fmt.Sprintf("select * from LP_BNUM(%d)", k)})
	}
	for k := 1; k <= 3; k++ {
		cases = append(cases, lpBlobFetchCase{name: fmt.Sprintf("BLOB edge: date %d", k), sql: fmt.Sprintf("select * from LP_BDATE(%d)", k),
			failAt: fmt.Sprintf("IF (K = %d)", k)})
	}
	return cases
}
