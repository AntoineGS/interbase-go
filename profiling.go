package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// TableCounters contains cumulative record activity for one relation.
type TableCounters struct {
	SeqReads, IdxReads, Inserts, Updates, Deletes, Backouts, Purges, Expunges int64
}

// PageCounters contains cumulative database page activity.
type PageCounters struct{ Reads, Writes, Fetches, Marks int64 }

// Activity is a snapshot of the cumulative per-attachment counters.
type Activity struct {
	Tables map[int]TableCounters // keyed by RDB$RELATION_ID
	Pages  *PageCounters         // nil when the server does not expose attachment-scoped page counters
}

// ActivityDelta contains non-zero counter differences between two snapshots.
type ActivityDelta struct {
	Tables map[int]TableCounters // only relations with a non-zero counter
	Pages  *PageCounters
}

// ErrActivityCounterReset reports that a cumulative activity counter decreased
// between snapshots.
var ErrActivityCounterReset = errors.New("interbase: activity counter decreased")

// ReadActivity returns activity counters for the attachment behind conn.
func ReadActivity(ctx context.Context, pooled *sql.Conn) (Activity, error) {
	if pooled == nil {
		return Activity{}, ErrNotInterBaseConn
	}
	var result Activity
	if err := pooled.Raw(func(driverConn any) error {
		connection, ok := driverConn.(*conn)
		if !ok {
			return ErrNotInterBaseConn
		}
		activity, err := connection.readActivity(ctx)
		if err != nil {
			return err
		}
		result = activity
		return nil
	}); err != nil {
		return Activity{}, err
	}
	return result, nil
}

// readActivity reads the cumulative activity counters from the attachment.
// It intentionally does not open a transaction so callers can bracket their
// own execute/fetch steps with short snapshots.
func (c *conn) readActivity(ctx context.Context) (Activity, error) {
	if err := contextError(ctx); err != nil {
		return Activity{}, err
	}
	c.lockDirect()
	defer c.mu.Unlock()
	if c.closed || c.native == nil || c.native.broken() {
		return Activity{}, driver.ErrBadConn
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return Activity{}, err
	}
	defer releaseNative()

	readInfo := func(code byte) (InfoItem, error) {
		if err := contextError(ctx); err != nil {
			return InfoItem{}, err
		}
		response, err := c.native.databaseInfo(code)
		if err != nil {
			return InfoItem{}, fmt.Errorf("interbase: activity counter info %d: %w", code,
				c.sanitizeError("activity counters", err))
		}
		if len(response) == 1 && response[0] == infoEnd {
			if err := contextError(ctx); err != nil {
				return InfoItem{}, err
			}
			return InfoItem{Code: code}, nil
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

	activity := Activity{
		Tables: make(map[int]TableCounters),
		Pages:  &PageCounters{},
	}
	for _, item := range []struct {
		code byte
		set  func(int64)
	}{
		{code: 5, set: func(value int64) { activity.Pages.Reads = value }},
		{code: 6, set: func(value int64) { activity.Pages.Writes = value }},
		{code: 7, set: func(value int64) { activity.Pages.Fetches = value }},
		{code: 8, set: func(value int64) { activity.Pages.Marks = value }},
	} {
		response, err := readInfo(item.code)
		if err != nil {
			return Activity{}, err
		}
		var value uint64
		if len(response.Data) != 0 {
			value, err = response.Uint64()
			if err != nil {
				return Activity{}, err
			}
		}
		if value > math.MaxInt64 {
			return Activity{}, fmt.Errorf("interbase: activity counter %d overflows int64", item.code)
		}
		item.set(int64(value))
	}

	for _, item := range []struct {
		code byte
		set  func(*TableCounters, int64)
	}{
		{code: 23, set: func(counters *TableCounters, value int64) { counters.SeqReads = value }},
		{code: 24, set: func(counters *TableCounters, value int64) { counters.IdxReads = value }},
		{code: 25, set: func(counters *TableCounters, value int64) { counters.Inserts = value }},
		{code: 26, set: func(counters *TableCounters, value int64) { counters.Updates = value }},
		{code: 27, set: func(counters *TableCounters, value int64) { counters.Deletes = value }},
		{code: 28, set: func(counters *TableCounters, value int64) { counters.Backouts = value }},
		{code: 29, set: func(counters *TableCounters, value int64) { counters.Purges = value }},
		{code: 30, set: func(counters *TableCounters, value int64) { counters.Expunges = value }},
	} {
		response, err := readInfo(item.code)
		if err != nil {
			return Activity{}, err
		}
		counts, err := parseRelationCounts(response.Data)
		if err != nil {
			return Activity{}, fmt.Errorf("interbase: decode activity counter %d: %w", item.code, err)
		}
		for relationID, value := range counts {
			counters := activity.Tables[relationID]
			item.set(&counters, value)
			activity.Tables[relationID] = counters
		}
	}
	return activity, nil
}

func parseRelationCounts(item []byte) (map[int]int64, error) {
	if len(item)%6 != 0 {
		return nil, fmt.Errorf("interbase: malformed relation activity payload length %d", len(item))
	}
	counts := make(map[int]int64, len(item)/6)
	for offset := 0; offset < len(item); offset += 6 {
		relationID := int(binary.LittleEndian.Uint16(item[offset : offset+2]))
		count := int64(binary.LittleEndian.Uint32(item[offset+2 : offset+6]))
		counts[relationID] += count
	}
	return counts, nil
}

// Delta returns a - before. A negative counter returns ErrActivityCounterReset.
func (a Activity) Delta(before Activity) (ActivityDelta, error) {
	delta := ActivityDelta{}
	ids := make(map[int]struct{}, len(a.Tables)+len(before.Tables))
	for relationID := range a.Tables {
		ids[relationID] = struct{}{}
	}
	for relationID := range before.Tables {
		ids[relationID] = struct{}{}
	}
	for relationID := range ids {
		counters := subtractTableCounters(a.Tables[relationID], before.Tables[relationID])
		if tableCountersNegative(counters) {
			return ActivityDelta{}, fmt.Errorf("%w: relation %d", ErrActivityCounterReset, relationID)
		}
		if tableCountersNonzero(counters) {
			if delta.Tables == nil {
				delta.Tables = make(map[int]TableCounters)
			}
			delta.Tables[relationID] = counters
		}
	}

	if a.Pages != nil {
		var previous PageCounters
		if before.Pages != nil {
			previous = *before.Pages
		}
		pages := subtractPageCounters(*a.Pages, previous)
		if pageCountersNegative(pages) {
			return ActivityDelta{}, ErrActivityCounterReset
		}
		delta.Pages = &pages
	}
	return delta, nil
}

// Add returns the sum of two activity deltas without modifying either input.
func (d ActivityDelta) Add(other ActivityDelta) ActivityDelta {
	result := ActivityDelta{}
	if len(d.Tables) != 0 || len(other.Tables) != 0 {
		result.Tables = make(map[int]TableCounters, len(d.Tables)+len(other.Tables))
	}
	for relationID, counters := range d.Tables {
		result.Tables[relationID] = counters
	}
	for relationID, counters := range other.Tables {
		if result.Tables == nil {
			result.Tables = make(map[int]TableCounters, len(d.Tables)+len(other.Tables))
		}
		result.Tables[relationID] = addTableCounters(result.Tables[relationID], counters)
	}
	for relationID, counters := range result.Tables {
		if !tableCountersNonzero(counters) {
			delete(result.Tables, relationID)
		}
	}
	if d.Pages != nil || other.Pages != nil {
		var left, right PageCounters
		if d.Pages != nil {
			left = *d.Pages
		}
		if other.Pages != nil {
			right = *other.Pages
		}
		pages := addPageCounters(left, right)
		result.Pages = &pages
	}
	return result
}

// RelationNames resolves relation IDs in one short read-only transaction.
func RelationNames(ctx context.Context, conn *sql.Conn, ids []int) (map[int]string, error) {
	if conn == nil {
		return nil, ErrNotInterBaseConn
	}
	names := make(map[int]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}
	err := WithTransaction(ctx, conn, TransactionOptions{
		ReadOnly:  true,
		Isolation: sql.LevelReadCommitted,
	}, func(tx *Transaction) error {
		for start := 0; start < len(ids); start += 500 {
			end := start + 500
			if end > len(ids) {
				end = len(ids)
			}
			chunk := ids[start:end]
			query := "SELECT RDB$RELATION_ID, RDB$RELATION_NAME FROM RDB$RELATIONS WHERE RDB$RELATION_ID IN (" +
				strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + ")"
			args := make([]any, len(chunk))
			for index, relationID := range chunk {
				args[index] = int64(relationID)
			}
			cursor, err := tx.Query(ctx, query, args...)
			if err != nil {
				return fmt.Errorf("interbase: relation name lookup: %w", err)
			}
			if err := collectRelationNames(ctx, cursor, names); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

func collectRelationNames(ctx context.Context, cursor *Cursor, names map[int]string) (resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr, cursor.Close())
	}()
	for {
		hasRow, err := cursor.Next(ctx)
		if err != nil {
			return fmt.Errorf("interbase: relation name lookup: %w", err)
		}
		if !hasRow {
			return nil
		}
		row, err := cursor.Row()
		if err != nil {
			return fmt.Errorf("interbase: relation name lookup: %w", err)
		}
		if len(row) != 2 {
			return errors.New("interbase: relation name lookup returned an unexpected column count")
		}
		relationID, err := activityCellInt64(row[0])
		if err != nil {
			return fmt.Errorf("interbase: decode relation ID: %w", err)
		}
		name, err := activityCellText(row[1])
		if err != nil {
			return fmt.Errorf("interbase: decode relation name: %w", err)
		}
		names[int(relationID)] = strings.TrimSpace(name)
	}
}

// RoutineKey identifies a procedure or trigger. Relation is populated for
// triggers because trigger names are scoped to their relation.
type RoutineKey struct{ Kind, Name, Relation string }

// RoutineCounters contains cumulative execution and record activity.
type RoutineCounters struct {
	Invocations, PageReads, PageWrites, PageFetches, PageMarks,
	RecordSelects, RecordInserts, RecordUpdates, RecordDeletes,
	RecordPurges, RecordExpunges, RecordBackouts int64
}

// RoutineActivity is a snapshot of database-wide procedure and trigger activity.
type RoutineActivity struct {
	Routines map[RoutineKey]RoutineCounters
}

// ErrRoutineActivityUnavailable reports that routine activity tables cannot be read.
var ErrRoutineActivityUnavailable = errors.New("interbase: routine activity is unavailable")

// ReadRoutineActivity reads TMP$PROCEDURES and TMP$TRIGGERS (database-wide).
func ReadRoutineActivity(ctx context.Context, conn *sql.Conn) (RoutineActivity, error) {
	if conn == nil {
		return RoutineActivity{}, ErrNotInterBaseConn
	}
	activity := RoutineActivity{Routines: make(map[RoutineKey]RoutineCounters)}
	err := WithTransaction(ctx, conn, TransactionOptions{
		ReadOnly:  true,
		Isolation: sql.LevelReadCommitted,
	}, func(tx *Transaction) error {
		procedures, err := readRoutineRows(ctx, tx, "procedure",
			"SELECT TMP$PROCEDURE_NAME, TMP$INVOCATIONS, TMP$PAGE_READS, TMP$PAGE_WRITES, TMP$PAGE_FETCHES, TMP$PAGE_MARKS, TMP$RECORD_SELECTS, TMP$RECORD_INSERTS, TMP$RECORD_UPDATES, TMP$RECORD_DELETES, TMP$RECORD_PURGES, TMP$RECORD_EXPUNGES, TMP$RECORD_BACKOUTS FROM TMP$PROCEDURES")
		if err != nil {
			return err
		}
		triggers, err := readRoutineRows(ctx, tx, "trigger",
			"SELECT TMP$TRIGGER_NAME, TMP$RELATION_NAME, TMP$INVOCATIONS, TMP$PAGE_READS, TMP$PAGE_WRITES, TMP$PAGE_FETCHES, TMP$PAGE_MARKS, TMP$RECORD_SELECTS, TMP$RECORD_INSERTS, TMP$RECORD_UPDATES, TMP$RECORD_DELETES, TMP$RECORD_PURGES, TMP$RECORD_EXPUNGES, TMP$RECORD_BACKOUTS FROM TMP$TRIGGERS")
		if err != nil {
			return err
		}
		for key, counters := range procedures {
			activity.Routines[key] = addRoutineCounters(activity.Routines[key], counters)
		}
		for key, counters := range triggers {
			activity.Routines[key] = addRoutineCounters(activity.Routines[key], counters)
		}
		return nil
	})
	if err != nil {
		return RoutineActivity{}, err
	}
	return activity, nil
}

func readRoutineRows(ctx context.Context, tx *Transaction, kind, query string) (map[RoutineKey]RoutineCounters, error) {
	cursor, err := tx.Query(ctx, query)
	if err != nil {
		return nil, errors.Join(ErrRoutineActivityUnavailable, fmt.Errorf("interbase: read TMP$%s: %w", strings.ToUpper(kind)+"S", err))
	}
	defer func() { _ = cursor.Close() }()

	routines := make(map[RoutineKey]RoutineCounters)
	for {
		hasRow, err := cursor.Next(ctx)
		if err != nil {
			return nil, fmt.Errorf("interbase: read %s activity: %w", kind, err)
		}
		if !hasRow {
			return routines, nil
		}
		row, err := cursor.Row()
		if err != nil {
			return nil, fmt.Errorf("interbase: read %s activity: %w", kind, err)
		}
		key, counters, err := parseRoutineRow(kind, row)
		if err != nil {
			return nil, err
		}
		routines[key] = addRoutineCounters(routines[key], counters)
	}
}

func parseRoutineRow(kind string, row []Cell) (RoutineKey, RoutineCounters, error) {
	nameOffset := 1
	key := RoutineKey{Kind: kind}
	if kind == "trigger" {
		nameOffset = 2
	}
	wantColumns := nameOffset + 12
	if len(row) != wantColumns {
		return RoutineKey{}, RoutineCounters{}, fmt.Errorf("interbase: %s activity returned %d columns, want %d", kind, len(row), wantColumns)
	}
	name, err := activityCellText(row[0])
	if err != nil {
		return RoutineKey{}, RoutineCounters{}, fmt.Errorf("interbase: decode %s name: %w", kind, err)
	}
	key.Name = strings.TrimSpace(name)
	if kind == "trigger" {
		relation, err := activityCellText(row[1])
		if err != nil {
			return RoutineKey{}, RoutineCounters{}, fmt.Errorf("interbase: decode trigger relation: %w", err)
		}
		key.Relation = strings.TrimSpace(relation)
	}
	values := [12]int64{}
	for index := range values {
		values[index], err = activityCellInt64(row[nameOffset+index])
		if err != nil {
			return RoutineKey{}, RoutineCounters{}, fmt.Errorf("interbase: decode %s counter %d: %w", kind, index, err)
		}
	}
	return key, RoutineCounters{
		Invocations: values[0], PageReads: values[1], PageWrites: values[2], PageFetches: values[3], PageMarks: values[4],
		RecordSelects: values[5], RecordInserts: values[6], RecordUpdates: values[7], RecordDeletes: values[8],
		RecordPurges: values[9], RecordExpunges: values[10], RecordBackouts: values[11],
	}, nil
}

func activityCellInt64(cell Cell) (int64, error) {
	switch value := cell.Value.(type) {
	case int64:
		return value, nil
	case []byte:
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return 0, err
		}
		return parsed, nil
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, err
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("unexpected integer value %T", cell.Value)
	}
}

func activityCellText(cell Cell) (string, error) {
	switch value := cell.Value.(type) {
	case string:
		return value, nil
	case []byte:
		return string(value), nil
	default:
		return "", fmt.Errorf("unexpected text value %T", cell.Value)
	}
}

// Delta returns only routines whose counters changed. Routines absent from
// before count from zero because they may have been loaded during the run.
func (r RoutineActivity) Delta(before RoutineActivity) map[RoutineKey]RoutineCounters {
	delta := make(map[RoutineKey]RoutineCounters)
	for key, counters := range r.Routines {
		changed := subtractRoutineCounters(counters, before.Routines[key])
		if routineCountersNonzero(changed) {
			delta[key] = changed
		}
	}
	return delta
}

func subtractTableCounters(after, before TableCounters) TableCounters {
	return TableCounters{
		SeqReads: after.SeqReads - before.SeqReads, IdxReads: after.IdxReads - before.IdxReads,
		Inserts: after.Inserts - before.Inserts, Updates: after.Updates - before.Updates,
		Deletes: after.Deletes - before.Deletes, Backouts: after.Backouts - before.Backouts,
		Purges: after.Purges - before.Purges, Expunges: after.Expunges - before.Expunges,
	}
}

func tableCountersNegative(counters TableCounters) bool {
	return counters.SeqReads < 0 || counters.IdxReads < 0 || counters.Inserts < 0 || counters.Updates < 0 ||
		counters.Deletes < 0 || counters.Backouts < 0 || counters.Purges < 0 || counters.Expunges < 0
}

func tableCountersNonzero(counters TableCounters) bool {
	return counters.SeqReads != 0 || counters.IdxReads != 0 || counters.Inserts != 0 || counters.Updates != 0 ||
		counters.Deletes != 0 || counters.Backouts != 0 || counters.Purges != 0 || counters.Expunges != 0
}

func addTableCounters(left, right TableCounters) TableCounters {
	return TableCounters{
		SeqReads: left.SeqReads + right.SeqReads, IdxReads: left.IdxReads + right.IdxReads,
		Inserts: left.Inserts + right.Inserts, Updates: left.Updates + right.Updates,
		Deletes: left.Deletes + right.Deletes, Backouts: left.Backouts + right.Backouts,
		Purges: left.Purges + right.Purges, Expunges: left.Expunges + right.Expunges,
	}
}

func subtractPageCounters(after, before PageCounters) PageCounters {
	return PageCounters{
		Reads: after.Reads - before.Reads, Writes: after.Writes - before.Writes,
		Fetches: after.Fetches - before.Fetches, Marks: after.Marks - before.Marks,
	}
}

func pageCountersNegative(counters PageCounters) bool {
	return counters.Reads < 0 || counters.Writes < 0 || counters.Fetches < 0 || counters.Marks < 0
}

func addPageCounters(left, right PageCounters) PageCounters {
	return PageCounters{
		Reads: left.Reads + right.Reads, Writes: left.Writes + right.Writes,
		Fetches: left.Fetches + right.Fetches, Marks: left.Marks + right.Marks,
	}
}

func subtractRoutineCounters(after, before RoutineCounters) RoutineCounters {
	return RoutineCounters{
		Invocations: after.Invocations - before.Invocations, PageReads: after.PageReads - before.PageReads,
		PageWrites: after.PageWrites - before.PageWrites, PageFetches: after.PageFetches - before.PageFetches,
		PageMarks: after.PageMarks - before.PageMarks, RecordSelects: after.RecordSelects - before.RecordSelects,
		RecordInserts: after.RecordInserts - before.RecordInserts, RecordUpdates: after.RecordUpdates - before.RecordUpdates,
		RecordDeletes: after.RecordDeletes - before.RecordDeletes, RecordPurges: after.RecordPurges - before.RecordPurges,
		RecordExpunges: after.RecordExpunges - before.RecordExpunges, RecordBackouts: after.RecordBackouts - before.RecordBackouts,
	}
}

func routineCountersNonzero(counters RoutineCounters) bool {
	return counters.Invocations != 0 || counters.PageReads != 0 || counters.PageWrites != 0 ||
		counters.PageFetches != 0 || counters.PageMarks != 0 || counters.RecordSelects != 0 ||
		counters.RecordInserts != 0 || counters.RecordUpdates != 0 || counters.RecordDeletes != 0 ||
		counters.RecordPurges != 0 || counters.RecordExpunges != 0 || counters.RecordBackouts != 0
}

func addRoutineCounters(left, right RoutineCounters) RoutineCounters {
	return RoutineCounters{
		Invocations: left.Invocations + right.Invocations, PageReads: left.PageReads + right.PageReads,
		PageWrites: left.PageWrites + right.PageWrites, PageFetches: left.PageFetches + right.PageFetches,
		PageMarks: left.PageMarks + right.PageMarks, RecordSelects: left.RecordSelects + right.RecordSelects,
		RecordInserts: left.RecordInserts + right.RecordInserts, RecordUpdates: left.RecordUpdates + right.RecordUpdates,
		RecordDeletes: left.RecordDeletes + right.RecordDeletes, RecordPurges: left.RecordPurges + right.RecordPurges,
		RecordExpunges: left.RecordExpunges + right.RecordExpunges, RecordBackouts: left.RecordBackouts + right.RecordBackouts,
	}
}
