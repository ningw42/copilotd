package sqlitestore_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

// The literal table_xinfo contract test remains the independent witness of the
// exact schema; this test checks the migrated schema against the declaration.
func TestStoreSchemaConformsToDeclaredNativeCounts(t *testing.T) {
	path, store := openStore(t, io.Discard)
	if report := closeStore(t, store); !report.DriverCleanupCompleted {
		t.Fatalf("Close report = %+v", report)
	}
	db := openExternal(t, path)
	assertDeclaredCountColumns(t, db, sqlitestore.AnthropicTable, usage.AnthropicProjection(),
		"id", "at_ms", "at_utc", "request_id", "message_id", "turn_index", "model", "transport", "requested_model")
	assertDeclaredCountColumns(t, db, sqlitestore.OpenAITable, usage.OpenAIProjection(),
		"id", "at_ms", "at_utc", "request_id", "response_id", "turn_index", "model", "transport", "requested_model", "service_tier")
}

// assertDeclaredCountColumns requires the table's columns to be exactly the
// declared counts plus the writer-owned non-count columns, and every declared
// count to be INTEGER with NOT NULL exactly when it is required.
func assertDeclaredCountColumns[U usage.Usage](t *testing.T, db *sql.DB, table string, projection usage.Projection[U], nonCount ...string) {
	t.Helper()
	type column struct {
		typ     string
		notNull bool
	}
	columns := map[string]column{}
	rows, err := db.Query("SELECT name, type, \"notnull\" FROM pragma_table_xinfo(?)", table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, typ string
		var notNull bool
		if err := rows.Scan(&name, &typ, &notNull); err != nil {
			t.Fatal(err)
		}
		columns[name] = column{typ: typ, notNull: notNull}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := slices.Clone(nonCount)
	for _, count := range projection.All() {
		if slices.Contains(nonCount, count.Name()) {
			t.Errorf("%s declared count %s is also a non-count column", table, count.Name())
		}
		want = append(want, count.Name())
		got, ok := columns[count.Name()]
		if !ok {
			continue
		}
		if got.typ != "INTEGER" || got.notNull != count.Required() {
			t.Errorf("%s.%s = %s notnull=%t, want INTEGER notnull=%t", table, count.Name(), got.typ, got.notNull, count.Required())
		}
	}
	var got []string
	for name := range columns {
		got = append(got, name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s columns = %v, want declared counts plus non-count columns %v", table, got, want)
	}
}

func TestCreateCurrentSchemaMatchesWriterSchemaOnACallerConfiguredConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.db")
	db := openExternal(t, path)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA encoding='UTF-16le'"); err != nil {
		t.Fatal(err)
	}
	if err := sqlitestore.CreateCurrentSchema(ctx, conn); err != nil {
		t.Fatalf("CreateCurrentSchema: %v", err)
	}
	// A second call finds nothing pending, and each call ends its transaction.
	if err := sqlitestore.CreateCurrentSchema(ctx, conn); err != nil {
		t.Fatalf("repeated CreateCurrentSchema: %v", err)
	}
	assertNoOpenTransaction(t, conn)
	var encoding string
	if err := conn.QueryRowContext(ctx, "PRAGMA encoding").Scan(&encoding); err != nil || encoding != "UTF-16le" {
		t.Errorf("encoding = %q, %v; want the caller's UTF-16le", encoding, err)
	}

	freshPath, fresh := openStore(t, io.Discard)
	if report := closeStore(t, fresh); !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
	freshDB := openExternal(t, freshPath)
	for _, query := range []string{"PRAGMA user_version", usageSchemaQuery} {
		if got, want := externalRows(t, db, query), externalRows(t, freshDB, query); !reflect.DeepEqual(got, want) {
			t.Errorf("fixture/writer %s differ: %#v / %#v", query, got, want)
		}
	}
	if got := externalRows(t, db, "PRAGMA user_version"); !reflect.DeepEqual(got, [][]any{{int64(sqlitestore.SchemaVersion())}}) {
		t.Errorf("fixture version = %v, want %d", got, sqlitestore.SchemaVersion())
	}
}

func TestCreateCurrentSchemaRollsBackAFailedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conflict.db")
	db := openExternal(t, path)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE openai_turn (sentinel TEXT) STRICT`); err != nil {
		t.Fatal(err)
	}
	if err := sqlitestore.CreateCurrentSchema(ctx, conn); err == nil || !strings.Contains(err.Error(), "migration 1") {
		t.Fatalf("CreateCurrentSchema error = %v, want migration failure", err)
	}
	assertNoOpenTransaction(t, conn)
	if got := externalRows(t, db, "PRAGMA user_version"); !reflect.DeepEqual(got, [][]any{{int64(0)}}) {
		t.Errorf("user_version = %v, want rollback to 0", got)
	}
	if got := externalRows(t, db, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`); !reflect.DeepEqual(got, [][]any{{"openai_turn"}}) {
		t.Errorf("tables = %v, want only the conflicting table", got)
	}

	future := sqlitestore.SchemaVersion() + 1
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", future)); err != nil {
		t.Fatal(err)
	}
	if err := sqlitestore.CreateCurrentSchema(ctx, conn); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("schema version %d", future)) {
		t.Fatalf("future-version CreateCurrentSchema error = %v", err)
	}
	assertNoOpenTransaction(t, conn)
}

func TestCreateCurrentSchemaLeavesACallersOpenTransactionUntouched(t *testing.T) {
	for _, tc := range []struct {
		name     string
		canceled bool
	}{
		{name: "live context"},
		{name: "canceled context", canceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "caller.db")
			db := openExternal(t, path)
			ctx := context.Background()
			conn, err := db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for _, statement := range []string{"BEGIN", "CREATE TABLE caller_owned (value INTEGER)", "INSERT INTO caller_owned VALUES (17)"} {
				if _, err := conn.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			callCtx := ctx
			if tc.canceled {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				callCtx = canceled
			}
			err = sqlitestore.CreateCurrentSchema(callCtx, conn)
			if err == nil {
				t.Fatal("CreateCurrentSchema inside a caller transaction succeeded, want failure")
			}
			if tc.canceled && !errors.Is(err, context.Canceled) {
				t.Errorf("CreateCurrentSchema error = %v, want cancellation", err)
			}
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				t.Fatalf("caller transaction was ended by CreateCurrentSchema: %v", err)
			}
			if got := externalRows(t, db, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`); !reflect.DeepEqual(got, [][]any{{"caller_owned"}}) {
				t.Errorf("tables = %v, want only the caller's committed table", got)
			}
			if got := externalRows(t, db, "SELECT value FROM caller_owned"); !reflect.DeepEqual(got, [][]any{{int64(17)}}) {
				t.Errorf("caller rows = %v, want the committed value 17", got)
			}
		})
	}
}

func TestCreateCurrentSchemaCancellationLeavesNoTransactionOrSchema(t *testing.T) {
	t.Run("canceled before the call", func(t *testing.T) {
		db := openExternal(t, filepath.Join(t.TempDir(), "canceled.db"))
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := sqlitestore.CreateCurrentSchema(ctx, conn); !errors.Is(err, context.Canceled) {
			t.Fatalf("CreateCurrentSchema error = %v, want cancellation", err)
		}
		assertNoOpenTransaction(t, conn)
		assertNoUsageSchema(t, db)
	})

	t.Run("deadline while waiting for the write lock", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "contended.db")
		db := openExternal(t, path)
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout=500"); err != nil {
			t.Fatal(err)
		}
		// Another connection holds the write lock that BEGIN IMMEDIATE needs.
		blocker, err := openExternal(t, path).Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Close()
		for _, statement := range []string{"BEGIN IMMEDIATE", "CREATE TABLE blocker_owned (value INTEGER)"} {
			if _, err := blocker.ExecContext(context.Background(), statement); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		// The driver may report the deadline or the busy timeout, whichever it
		// observes first; either way acquisition failed.
		if err := sqlitestore.CreateCurrentSchema(ctx, conn); err == nil {
			t.Fatal("CreateCurrentSchema acquired a lock held by another connection")
		}
		assertNoOpenTransaction(t, conn)
		if _, err := blocker.ExecContext(context.Background(), "COMMIT"); err != nil {
			t.Fatalf("other connection's transaction was disturbed: %v", err)
		}
		if got := externalRows(t, db, `SELECT name FROM sqlite_schema WHERE type='table' ORDER BY name`); !reflect.DeepEqual(got, [][]any{{"blocker_owned"}}) {
			t.Errorf("tables = %v, want only the other connection's table", got)
		}
		if got := externalRows(t, db, "PRAGMA user_version"); !reflect.DeepEqual(got, [][]any{{int64(0)}}) {
			t.Errorf("user_version = %v, want 0", got)
		}
	})
}

func assertNoUsageSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	if got := externalRows(t, db, "SELECT count(*) FROM sqlite_schema"); !reflect.DeepEqual(got, [][]any{{int64(0)}}) {
		t.Errorf("schema objects = %v, want none", got)
	}
	if got := externalRows(t, db, "PRAGMA user_version"); !reflect.DeepEqual(got, [][]any{{int64(0)}}) {
		t.Errorf("user_version = %v, want 0", got)
	}
}

func assertNoOpenTransaction(t *testing.T, conn *sql.Conn) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), "BEGIN"); err != nil {
		t.Fatalf("connection is still inside a transaction: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}
