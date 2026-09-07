package sqlitestore_test

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

// Build an old database through external SQL, never by downgrading a current
// store. Migration 1 is the historical contract and must remain unchanged.
func createUsageV1(t *testing.T) (string, *sql.DB) {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "usage.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db := openExternal(t, path)
	initial, err := os.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		string(initial),
		`INSERT INTO anthropic_turn (id,at_ms,request_id,message_id,turn_index,model,transport,input_tokens,output_tokens,cache_creation_input_tokens,cache_read_input_tokens,ephemeral_5m_input_tokens,ephemeral_1h_input_tokens,thinking_tokens)
		 VALUES (7,1700000000123,'reused','message',2,'claude-reported','sse',12,9,2000,6000,750,1250,4)`,
		`INSERT INTO openai_turn (id,at_ms,request_id,response_id,turn_index,model,transport,input_tokens,cached_tokens,cache_write_tokens,output_tokens,reasoning_tokens,total_tokens)
		 VALUES (9,1700000000123,'reused','response',3,'gpt-reported','websocket',8012,6000,0,9,NULL,8021)`,
		`PRAGMA user_version=1`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path, db
}

func externalRows(t *testing.T, db *sql.DB, query string) [][]any {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

const usageSchemaQuery = `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`

func TestStoreRequestedModelUpgradePreservesHistoryAndMatchesFreshSchema(t *testing.T) {
	path, db := createUsageV1(t)
	history := map[string][][]any{}
	for _, table := range []string{"anthropic_turn", "openai_turn"} {
		history[table] = externalRows(t, db, "SELECT * FROM "+table+" ORDER BY id")
	}
	_ = db.Close()
	upgraded, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	if report := closeStore(t, upgraded); !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
	db = openExternal(t, path)
	for table, oldRows := range history {
		got := externalRows(t, db, "SELECT * FROM "+table+" ORDER BY id")
		if len(got) != len(oldRows) {
			t.Fatalf("%s history rows = %d, want %d", table, len(got), len(oldRows))
		}
		for i, old := range oldRows {
			want := append(append([]any(nil), old...), nil)
			if !reflect.DeepEqual(got[i], want) {
				t.Errorf("%s upgraded row = %#v, want unchanged history plus NULL: %#v", table, got[i], want)
			}
		}
	}
	freshPath, fresh := openStore(t, io.Discard)
	if report := closeStore(t, fresh); !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
	freshDB := openExternal(t, freshPath)
	for _, query := range []string{"PRAGMA user_version", usageSchemaQuery} {
		got, want := externalRows(t, db, query), externalRows(t, freshDB, query)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("upgraded/fresh %s differ: %#v / %#v", query, got, want)
		}
	}
	if got := externalRows(t, db, "PRAGMA user_version"); !reflect.DeepEqual(got, [][]any{{int64(2)}}) {
		t.Fatalf("upgraded version = %v, want 2", got)
	}
	before := externalRows(t, db, usageSchemaQuery)
	_ = db.Close()
	reopened, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	if report := closeStore(t, reopened); !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
	db = openExternal(t, path)
	if got := externalRows(t, db, usageSchemaQuery); !reflect.DeepEqual(got, before) {
		t.Errorf("reopening changed schema: %#v", got)
	}
	for table, oldRows := range history {
		want := [][]any{append(append([]any(nil), oldRows[0]...), nil)}
		if got := externalRows(t, db, "SELECT * FROM "+table+" ORDER BY id"); !reflect.DeepEqual(got, want) {
			t.Errorf("reopening changed %s history: %#v", table, got)
		}
	}
}

func TestStoreRequestedModelMigrationRollsBackFirstAlterWhenSecondFails(t *testing.T) {
	path, db := createUsageV1(t)
	// An external schema conflict makes the second statement fail after the
	// first ALTER. Startup must leave every preexisting value and schema intact.
	if _, err := db.Exec(`ALTER TABLE openai_turn ADD COLUMN requested_model TEXT; UPDATE openai_turn SET requested_model='sentinel'`); err != nil {
		t.Fatal(err)
	}
	queries := []string{usageSchemaQuery, "PRAGMA user_version", "SELECT * FROM anthropic_turn", "SELECT * FROM openai_turn"}
	before := make([][][]any, len(queries))
	for i, query := range queries {
		before[i] = externalRows(t, db, query)
	}
	_ = db.Close()
	store, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
	if err == nil {
		closeStore(t, store)
		t.Fatal("Open accepted conflicting pending migration")
	}
	if !strings.Contains(err.Error(), "migration 2") {
		t.Fatalf("Open = %v, want migration 2 failure", err)
	}
	db = openExternal(t, path)
	for i, query := range queries {
		if got := externalRows(t, db, query); !reflect.DeepEqual(got, before[i]) {
			t.Errorf("failed migration changed %s: %#v, want %#v", query, got, before[i])
		}
	}
}
