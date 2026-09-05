package sqlitestore_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/logging"
	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func testStoreLogger(output io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func openStore(t *testing.T, output io.Writer) (string, *sqlitestore.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	store, err := sqlitestore.Open(path, testStoreLogger(output))
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	return path, store
}

func closeStore(t *testing.T, store *sqlitestore.Store) sqlitestore.Report {
	t.Helper()
	store.StopAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return store.Close(ctx)
}

func openExternal(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("external sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func ptr(value int64) *int64 { return &value }

func TestStoreCreatesMigrationOneAndRoundTripsBothNativeTables(t *testing.T) {
	path, store := openStore(t, io.Discard)
	at := time.UnixMilli(1_700_000_000_123)
	store.Record(usage.Turn{
		At: at, RequestID: "same-correlation", ResponseID: "msg-reused", TurnIndex: 0,
		Model: "claude-reported", Transport: usage.TransportBuffered,
		Usage: usage.AnthropicUsage{
			InputTokens: 12, OutputTokens: 9, CacheCreationInputTokens: ptr(0),
			CacheReadInputTokens: nil, Ephemeral5mInputTokens: ptr(0),
			Ephemeral1hInputTokens: nil, ThinkingTokens: ptr(4),
		},
	})
	store.Record(usage.Turn{
		At: at, RequestID: "same-correlation", ResponseID: "resp-reused", TurnIndex: 0,
		Model: "gpt-reported", Transport: usage.TransportBuffered,
		Usage: usage.OpenAIUsage{
			InputTokens: 8012, OutputTokens: 9, CachedTokens: ptr(6000),
			CacheWriteTokens: ptr(2000), ReasoningTokens: ptr(0), TotalTokens: nil,
		},
	})
	// Reused correlation, object identity, and ordinal remain admissible.
	store.Record(usage.Turn{
		At: at, RequestID: "same-correlation", ResponseID: "resp-reused", TurnIndex: 0,
		Model: "gpt-reported", Transport: usage.TransportBuffered,
		Usage: usage.OpenAIUsage{InputTokens: 0, OutputTokens: 0},
	})

	report := closeStore(t, store)
	if report != (sqlitestore.Report{DriverCleanupCompleted: true}) {
		t.Fatalf("Close report = %+v, want clean completed cleanup", report)
	}
	db := openExternal(t, path)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("user_version = %d, %v; want 1", version, err)
	}
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || !strings.EqualFold(journal, "wal") {
		t.Fatalf("journal_mode = %q, %v; want wal", journal, err)
	}

	var (
		anthropicUTC, anthropicRequest, messageID, anthropicModel, anthropicTransport string
		anthropicInput, anthropicOutput, cacheCreation, ephemeral5m, thinking         int64
		cacheRead, ephemeral1h                                                        sql.NullInt64
	)
	err := db.QueryRow(`SELECT at_utc, request_id, message_id, model, transport,
		input_tokens, output_tokens, cache_creation_input_tokens, cache_read_input_tokens,
		ephemeral_5m_input_tokens, ephemeral_1h_input_tokens, thinking_tokens
		FROM anthropic_turn`).Scan(
		&anthropicUTC, &anthropicRequest, &messageID, &anthropicModel, &anthropicTransport,
		&anthropicInput, &anthropicOutput, &cacheCreation, &cacheRead, &ephemeral5m, &ephemeral1h, &thinking,
	)
	if err != nil {
		t.Fatal(err)
	}
	if anthropicUTC != "2023-11-14T22:13:20.123Z" || anthropicRequest != "same-correlation" || messageID != "msg-reused" ||
		anthropicModel != "claude-reported" || anthropicTransport != "buffered" || anthropicInput != 12 || anthropicOutput != 9 ||
		cacheCreation != 0 || cacheRead.Valid || ephemeral5m != 0 || ephemeral1h.Valid || thinking != 4 {
		t.Errorf("Anthropic row = utc:%q request:%q message:%q model:%q transport:%q usage:[%d %d %d %#v %d %#v %d]",
			anthropicUTC, anthropicRequest, messageID, anthropicModel, anthropicTransport,
			anthropicInput, anthropicOutput, cacheCreation, cacheRead, ephemeral5m, ephemeral1h, thinking)
	}

	rows, err := db.Query(`SELECT response_id, turn_index, input_tokens, cached_tokens,
		cache_write_tokens, output_tokens, reasoning_tokens, total_tokens
		FROM openai_turn ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type openAIRow struct {
		responseID                 string
		turnIndex                  int
		input, output              int64
		cached, cacheWrite, reason sql.NullInt64
		total                      sql.NullInt64
	}
	var got []openAIRow
	for rows.Next() {
		var row openAIRow
		if err := rows.Scan(&row.responseID, &row.turnIndex, &row.input, &row.cached, &row.cacheWrite, &row.output, &row.reason, &row.total); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	want := []openAIRow{
		{responseID: "resp-reused", input: 8012, cached: sql.NullInt64{Int64: 6000, Valid: true}, cacheWrite: sql.NullInt64{Int64: 2000, Valid: true}, output: 9, reason: sql.NullInt64{Int64: 0, Valid: true}},
		{responseID: "resp-reused", input: 0, output: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OpenAI rows = %#v, want %#v", got, want)
	}
}

func TestStoreMigrationOneSchemaMatchesFrozenPublicContract(t *testing.T) {
	path, store := openStore(t, io.Discard)
	if report := closeStore(t, store); !report.DriverCleanupCompleted {
		t.Fatalf("Close report = %+v", report)
	}
	db := openExternal(t, path)

	type column struct {
		name, typ string
		notNull   int
		hidden    int
	}
	assertColumns := func(table string, want []column) {
		t.Helper()
		rows, err := db.Query("PRAGMA table_xinfo(" + table + ")")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var got []column
		for rows.Next() {
			var cid, pk int
			var name, typ string
			var notNull, hidden int
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk, &hidden); err != nil {
				t.Fatal(err)
			}
			got = append(got, column{name: name, typ: typ, notNull: notNull, hidden: hidden})
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s columns = %#v, want %#v", table, got, want)
		}
	}
	assertColumns("anthropic_turn", []column{
		{name: "id", typ: "INTEGER"}, {name: "at_ms", typ: "INTEGER", notNull: 1},
		{name: "at_utc", typ: "TEXT", hidden: 2}, {name: "request_id", typ: "TEXT", notNull: 1},
		{name: "message_id", typ: "TEXT", notNull: 1}, {name: "turn_index", typ: "INTEGER", notNull: 1},
		{name: "model", typ: "TEXT", notNull: 1}, {name: "transport", typ: "TEXT", notNull: 1},
		{name: "input_tokens", typ: "INTEGER", notNull: 1}, {name: "output_tokens", typ: "INTEGER", notNull: 1},
		{name: "cache_creation_input_tokens", typ: "INTEGER"}, {name: "cache_read_input_tokens", typ: "INTEGER"},
		{name: "ephemeral_5m_input_tokens", typ: "INTEGER"}, {name: "ephemeral_1h_input_tokens", typ: "INTEGER"},
		{name: "thinking_tokens", typ: "INTEGER"},
	})
	assertColumns("openai_turn", []column{
		{name: "id", typ: "INTEGER"}, {name: "at_ms", typ: "INTEGER", notNull: 1},
		{name: "at_utc", typ: "TEXT", hidden: 2}, {name: "request_id", typ: "TEXT", notNull: 1},
		{name: "response_id", typ: "TEXT", notNull: 1}, {name: "turn_index", typ: "INTEGER", notNull: 1},
		{name: "model", typ: "TEXT", notNull: 1}, {name: "transport", typ: "TEXT", notNull: 1},
		{name: "input_tokens", typ: "INTEGER", notNull: 1}, {name: "cached_tokens", typ: "INTEGER"},
		{name: "cache_write_tokens", typ: "INTEGER"}, {name: "output_tokens", typ: "INTEGER", notNull: 1},
		{name: "reasoning_tokens", typ: "INTEGER"}, {name: "total_tokens", typ: "INTEGER"},
	})

	for table, fragments := range map[string][]string{
		"anthropic_turn": {"STRICT", "strftime('%Y-%m-%dT%H:%M:%fZ', at_ms/1000.0, 'unixepoch')", "transport IN ('buffered','sse')"},
		"openai_turn":    {"STRICT", "strftime('%Y-%m-%dT%H:%M:%fZ', at_ms/1000.0, 'unixepoch')", "transport IN ('buffered','sse','websocket')"},
	} {
		var ddl string
		if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&ddl); err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !strings.Contains(ddl, fragment) {
				t.Errorf("%s DDL missing %q:\n%s", table, fragment, ddl)
			}
		}
	}
	for name, table := range map[string]string{"anthropic_turn_at": "anthropic_turn", "openai_turn_at": "openai_turn"} {
		var ddl string
		if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='index' AND name=? AND tbl_name=?`, name, table).Scan(&ddl); err != nil {
			t.Fatalf("index %s: %v", name, err)
		}
		if ddl != "CREATE INDEX "+name+" ON "+table+"(at_ms)" {
			t.Errorf("index %s DDL = %q", name, ddl)
		}
	}

	for _, statement := range []string{
		`INSERT INTO anthropic_turn (at_ms,request_id,message_id,turn_index,model,transport,input_tokens,output_tokens) VALUES (1,'r','m',0,'x','websocket',1,1)`,
		`INSERT INTO openai_turn (at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES (1,'r','r',0,'x','bogus',1,1)`,
		`INSERT INTO openai_turn (at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES (1,'r','r',0,'x','buffered','not-an-int',1)`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Errorf("constraint accepted invalid SQL: %s", statement)
		}
	}
	if _, err := db.Exec(`INSERT INTO openai_turn (at_ms,request_id,response_id,turn_index,model,transport,input_tokens,output_tokens) VALUES (2,'r','numeric-text',0,'x','buffered','12','3')`); err != nil {
		t.Errorf("STRICT rejected losslessly convertible numeric text: %v", err)
	}
	var inputType string
	if err := db.QueryRow(`SELECT typeof(input_tokens) FROM openai_turn WHERE response_id='numeric-text'`).Scan(&inputType); err != nil || inputType != "integer" {
		t.Errorf("numeric text stored type = %q, %v; want integer", inputType, err)
	}
}

func TestStoreConcurrentFreshOpenersShareOneMigratedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	stores := make([]*sqlitestore.Store, 2)
	errorsByOpener := make([]error, 2)
	start := make(chan struct{})
	var openers sync.WaitGroup
	for index := range stores {
		openers.Add(1)
		go func() {
			defer openers.Done()
			<-start
			stores[index], errorsByOpener[index] = sqlitestore.Open(path, testStoreLogger(io.Discard))
		}()
	}
	close(start)
	openers.Wait()
	for index, err := range errorsByOpener {
		if err != nil {
			t.Fatalf("opener %d: %v", index, err)
		}
		stores[index].Record(usage.Turn{At: time.UnixMilli(int64(index + 1)), ResponseID: fmt.Sprintf("opener-%d", index), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	}
	for index, store := range stores {
		if report := closeStore(t, store); !report.DriverCleanupCompleted || report.RuntimeWriteLosses != 0 || report.FinalFlushLosses != 0 {
			t.Errorf("opener %d Close report = %+v", index, report)
		}
	}
	db := openExternal(t, path)
	var version, count int
	_ = db.QueryRow("PRAGMA user_version").Scan(&version)
	_ = db.QueryRow("SELECT count(*) FROM openai_turn").Scan(&count)
	if version != 1 || count != 2 {
		t.Errorf("shared database version/count = %d/%d, want 1/2", version, count)
	}
}

func TestStoreRetriesImmediateWALBusyWithinOneStartupBudget(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "usage.db")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	locker := openExternal(t, path)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	type opened struct {
		store *sqlitestore.Store
		err   error
	}
	result := make(chan opened, 1)
	started := time.Now()
	go func() {
		store, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
		result <- opened{store: store, err: err}
	}()
	time.Sleep(150 * time.Millisecond)
	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil {
		t.Fatalf("Open after immediate WAL SQLITE_BUSY: %v", got.err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond || elapsed >= 5*time.Second {
		t.Errorf("Open elapsed = %v, want retry delay inside one five-second budget", elapsed)
	}
	if report := closeStore(t, got.store); !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
}

func TestStoreStartupContentionBudgetExhaustionAndNonContentionFailure(t *testing.T) {
	t.Run("contention exhaustion", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "usage.db")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
		locker := openExternal(t, path)
		if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		_, err = sqlitestore.Open(path, testStoreLogger(io.Discard))
		elapsed := time.Since(started)
		_, _ = locker.Exec("ROLLBACK")
		if err == nil || !strings.Contains(err.Error(), "startup contention budget exhausted") || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Open error = %v, want exhausted budget wrapping deadline", err)
		}
		if elapsed < 4500*time.Millisecond || elapsed > 6*time.Second {
			t.Errorf("exhausted startup elapsed = %v, want one approximately five-second budget", elapsed)
		}
	})

	t.Run("non-contention is not retried or truncated", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "private")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "usage.db")
		sentinel := []byte("not a sqlite database sentinel")
		if err := os.WriteFile(path, sentinel, 0o600); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		_, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
		if err == nil {
			t.Fatal("Open succeeded for non-database file")
		}
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("non-contention failure took %v, looks retried", elapsed)
		}
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, sentinel) {
			t.Errorf("existing bytes = %q, %v; want unchanged %q", got, readErr, sentinel)
		}
	})
}

func TestStoreReopenIsNoOpAndFutureSchemaFailsClosed(t *testing.T) {
	path, first := openStore(t, io.Discard)
	first.Record(usage.Turn{At: time.UnixMilli(1), ResponseID: "preserved", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2}})
	if report := closeStore(t, first); !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
	second, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
	if err != nil {
		t.Fatalf("reopen v1: %v", err)
	}
	if report := closeStore(t, second); !report.DriverCleanupCompleted {
		t.Fatal(report)
	}
	db := openExternal(t, path)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM openai_turn WHERE response_id='preserved'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("preserved rows = %d, %v; want 1", count, err)
	}
	if _, err := db.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	_, err = sqlitestore.Open(path, testStoreLogger(io.Discard))
	if err == nil || !strings.Contains(err.Error(), "schema version 2") || !strings.Contains(err.Error(), "supported version 1") {
		t.Fatalf("future-version Open error = %v, want both versions", err)
	}
}

func TestStoreMigrationFailureRollsBackPendingDDLAndVersion(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "usage.db")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	db := openExternal(t, path)
	if _, err := db.Exec(`CREATE TABLE openai_turn (sentinel TEXT) STRICT`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	_, err = sqlitestore.Open(path, testStoreLogger(io.Discard))
	if err == nil || !strings.Contains(err.Error(), "migration 1") {
		t.Fatalf("Open error = %v, want migration failure", err)
	}
	db = openExternal(t, path)
	var version int
	_ = db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 0 {
		t.Errorf("user_version = %d, want rollback to 0", version)
	}
	var anthropicTables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='anthropic_turn'`).Scan(&anthropicTables); err != nil || anthropicTables != 0 {
		t.Errorf("anthropic table count = %d, %v; want rolled back", anthropicTables, err)
	}
	var sentinel string
	if err := db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name='openai_turn'`).Scan(&sentinel); err != nil || !strings.Contains(sentinel, "sentinel TEXT") {
		t.Errorf("preexisting schema = %q, %v; want preserved sentinel", sentinel, err)
	}
}

func TestStoreFlushesIndependentlyOnTimerBatchFillAndShutdown(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		wait  bool
	}{
		{name: "timer", count: 1, wait: true},
		{name: "batch fill", count: 128, wait: true},
		{name: "shutdown", count: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, store := openStore(t, io.Discard)
			for i := range tc.count {
				store.Record(usage.Turn{At: time.UnixMilli(int64(i + 1)), ResponseID: fmt.Sprintf("resp-%d", i), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 2}})
			}
			if tc.wait {
				db := openExternal(t, path)
				deadline := time.Now().Add(2 * time.Second)
				for {
					var count int
					err := db.QueryRow("SELECT count(*) FROM openai_turn").Scan(&count)
					if err == nil && count == tc.count {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("externally visible rows = %d, %v; want %d before Close", count, err, tc.count)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if report := closeStore(t, store); report != (sqlitestore.Report{DriverCleanupCompleted: true}) {
				t.Fatalf("Close report = %+v", report)
			}
			db := openExternal(t, path)
			var count int
			if err := db.QueryRow("SELECT count(*) FROM openai_turn").Scan(&count); err != nil || count != tc.count {
				t.Fatalf("rows after Close = %d, %v; want %d", count, err, tc.count)
			}
		})
	}
}

func TestStoreWALAllowsExternalReadSnapshotWhileWriterCommits(t *testing.T) {
	path, store := openStore(t, io.Discard)
	store.Record(usage.Turn{At: time.UnixMilli(1), ResponseID: "first", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	time.Sleep(1100 * time.Millisecond)
	reader := openExternal(t, path)
	if _, err := reader.Exec("BEGIN"); err != nil {
		t.Fatal(err)
	}
	var snapshotCount int
	if err := reader.QueryRow("SELECT count(*) FROM openai_turn").Scan(&snapshotCount); err != nil || snapshotCount != 1 {
		t.Fatalf("initial reader snapshot = %d, %v; want 1", snapshotCount, err)
	}
	for i := range 128 {
		store.Record(usage.Turn{At: time.UnixMilli(int64(i + 2)), ResponseID: fmt.Sprintf("later-%d", i), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	}
	observer := openExternal(t, path)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var count int
		err := observer.QueryRow("SELECT count(*) FROM openai_turn").Scan(&count)
		if err == nil && count == 129 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("concurrent writer rows = %d, %v; want 129", count, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := reader.QueryRow("SELECT count(*) FROM openai_turn").Scan(&snapshotCount); err != nil || snapshotCount != 1 {
		t.Errorf("held reader snapshot = %d, %v; want stable 1", snapshotCount, err)
	}
	if _, err := reader.Exec("COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := reader.QueryRow("SELECT count(*) FROM openai_turn").Scan(&snapshotCount); err != nil || snapshotCount != 129 {
		t.Errorf("reader after commit = %d, %v; want 129", snapshotCount, err)
	}
	if report := closeStore(t, store); !report.DriverCleanupCompleted || report.RuntimeWriteLosses != 0 {
		t.Fatal(report)
	}
}

func TestStoreRuntimeContentionLosesOneBatchThenRecovers(t *testing.T) {
	path, store := openStore(t, io.Discard)
	locker := openExternal(t, path)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	for i := range 128 {
		store.Record(usage.Turn{At: time.UnixMilli(int64(i + 1)), ResponseID: fmt.Sprintf("lost-%d", i), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	}
	// The admitted connection's runtime busy timeout is five seconds. Let that
	// bounded attempt exhaust before releasing the external writer.
	time.Sleep(5300 * time.Millisecond)
	if _, err := locker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	otherWriter := openExternal(t, path)
	if _, err := otherWriter.Exec("PRAGMA busy_timeout=250"); err != nil {
		t.Fatal(err)
	}
	if _, err := otherWriter.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("other writer did not recover after deadline-edge BEGIN cleanup: %v", err)
	}
	if _, err := otherWriter.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	store.Record(usage.Turn{At: time.UnixMilli(1000), ResponseID: "recovered-after-contention", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 2, OutputTokens: 3}})
	time.Sleep(1100 * time.Millisecond)
	report := closeStore(t, store)
	if report.RuntimeWriteLosses != 128 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("Close report = %+v, want exactly one exhausted runtime batch", report)
	}
	db := openExternal(t, path)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM openai_turn WHERE response_id='recovered-after-contention'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery rows = %d, %v; want 1", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM openai_turn WHERE response_id LIKE 'lost-%'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("replayed failed rows = %d, %v; want 0", count, err)
	}
}

func TestStoreDeadlineEdgeBeginCleanupAllowsLaterBatchAndOtherWriter(t *testing.T) {
	path, store := openStore(t, io.Discard)
	locker := openExternal(t, path)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	time.AfterFunc(4999*time.Millisecond, func() {
		_, err := locker.Exec("ROLLBACK")
		released <- err
	})
	for index := range 128 {
		store.Record(usage.Turn{At: time.UnixMilli(int64(index + 1)), ResponseID: fmt.Sprintf("edge-%d", index), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	}
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	// Allow the runtime attempt to settle on either side of the real deadline.
	time.Sleep(300 * time.Millisecond)
	otherWriter := openExternal(t, path)
	if _, err := otherWriter.Exec("PRAGMA busy_timeout=250"); err != nil {
		t.Fatal(err)
	}
	if _, err := otherWriter.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("other writer remained blocked after ambiguous BEGIN cleanup: %v", err)
	}
	if _, err := otherWriter.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}

	store.Record(usage.Turn{At: time.UnixMilli(1000), ResponseID: "edge-recovered", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 2, OutputTokens: 3}})
	time.Sleep(1100 * time.Millisecond)
	report := closeStore(t, store)
	if report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted || (report.RuntimeWriteLosses != 0 && report.RuntimeWriteLosses != 128) {
		t.Fatalf("deadline-edge report = %+v", report)
	}
	db := openExternal(t, path)
	var firstCount, recoveredCount int
	if err := db.QueryRow(`SELECT count(*) FROM openai_turn WHERE response_id GLOB 'edge-[0-9]*'`).Scan(&firstCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM openai_turn WHERE response_id='edge-recovered'`).Scan(&recoveredCount); err != nil {
		t.Fatal(err)
	}
	if uint64(firstCount)+report.RuntimeWriteLosses != 128 || recoveredCount != 1 {
		t.Errorf("deadline-edge committed/lost/recovered = %d/%d/%d, want atomic first batch accounting and one later row", firstCount, report.RuntimeWriteLosses, recoveredCount)
	}
}

func TestStoreFullQueueDropsPromptlyWithoutSynchronousLogging(t *testing.T) {
	var logs bytes.Buffer
	path, store := openStore(t, &logs)
	locker := openExternal(t, path)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("hold external write lock: %v", err)
	}
	for i := range 128 {
		store.Record(usage.Turn{At: time.UnixMilli(int64(i + 1)), ResponseID: fmt.Sprintf("initial-%d", i), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	}
	time.Sleep(100 * time.Millisecond)
	for i := range 1024 {
		store.Record(usage.Turn{At: time.UnixMilli(int64(i + 1000)), ResponseID: fmt.Sprintf("queued-%d", i), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	}
	started := time.Now()
	store.Record(usage.Turn{ResponseID: "must-drop", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Errorf("full-queue Record took %v, want prompt non-blocking return", elapsed)
	}
	if logs.Len() != 0 {
		t.Errorf("Record synchronously logged on the hook path: %s", logs.String())
	}
	if _, err := locker.Exec("COMMIT"); err != nil {
		t.Fatalf("release external write lock: %v", err)
	}
	report := closeStore(t, store)
	if report.QueueFullDrops != 1 || report.RuntimeWriteLosses != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("Close report = %+v, want one queue-full drop", report)
	}
}

func TestStoreFailedBatchIsLostAndWriterContinuesWithoutReplay(t *testing.T) {
	path, store := openStore(t, io.Discard)
	// One invalid transport poisons this complete transaction. The valid row in
	// the same batch must not be replayed later.
	store.Record(usage.Turn{At: time.UnixMilli(1), ResponseID: "failed-valid", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	store.Record(usage.Turn{At: time.UnixMilli(2), ResponseID: "failed-invalid", Model: "m", Transport: usage.Transport("invalid"), Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	time.Sleep(1200 * time.Millisecond)
	store.Record(usage.Turn{At: time.UnixMilli(3), ResponseID: "recovered", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 2, OutputTokens: 3}})
	time.Sleep(1200 * time.Millisecond)
	report := closeStore(t, store)
	if report.RuntimeWriteLosses != 2 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("Close report = %+v, want two runtime losses and completed cleanup", report)
	}
	db := openExternal(t, path)
	rows, err := db.Query("SELECT response_id FROM openai_turn ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	if !reflect.DeepEqual(ids, []string{"recovered"}) {
		t.Errorf("persisted IDs = %v, want only subsequent recovered batch", ids)
	}
}

func TestStoreRecordRacingWithCloseIsSafe(t *testing.T) {
	_, store := openStore(t, io.Discard)
	start := make(chan struct{})
	var producers sync.WaitGroup
	for worker := range 16 {
		producers.Add(1)
		go func() {
			defer producers.Done()
			<-start
			for sequence := range 200 {
				store.Record(usage.Turn{At: time.Now(), ResponseID: fmt.Sprintf("race-%d-%d", worker, sequence), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
			}
		}()
	}
	close(start)
	store.StopAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	report := store.Close(ctx)
	cancel()
	producers.Wait()
	if !report.DriverCleanupCompleted {
		t.Fatalf("Close report = %+v", report)
	}
	// A post-close call remains a prompt counted drop and cannot panic.
	store.Record(usage.Turn{})
}

func TestStoreConcurrentSettlementNeverDoubleCountsInFlightAdmissions(t *testing.T) {
	const calls = 512
	for attempt := range 10 {
		_, store := openStore(t, io.Discard)
		start := make(chan struct{})
		var producers sync.WaitGroup
		for index := range calls {
			producers.Add(1)
			go func() {
				defer producers.Done()
				<-start
				store.Record(usage.Turn{At: time.UnixMilli(int64(index + 1)), ResponseID: fmt.Sprintf("overtake-%d", index), Model: "m", Transport: usage.Transport("invalid"), Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
			}()
		}
		close(start)
		closeResult := make(chan sqlitestore.Report, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			closeResult <- store.Close(ctx)
		}()
		producers.Wait()
		report := <-closeResult
		losses := report.QueueFullDrops + report.RuntimeWriteLosses + report.LateAfterCutoffDrops + report.FinalFlushLosses
		if losses > calls {
			t.Fatalf("attempt %d reported %d outcomes for %d Record calls: %+v", attempt, losses, calls, report)
		}
		againCtx, againCancel := context.WithTimeout(context.Background(), time.Second)
		again := store.Close(againCtx)
		againCancel()
		if again != report {
			t.Fatalf("attempt %d changed terminal settlement from %+v to %+v", attempt, report, again)
		}
	}
}

func TestStoreFinalSettlementCountsEveryCompletedRecordExactlyOnce(t *testing.T) {
	for attempt := range 5 {
		t.Run(fmt.Sprintf("deadline-edge-%d", attempt), func(t *testing.T) {
			path, store := openStore(t, io.Discard)
			locker := openExternal(t, path)
			if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			const admitted = 17
			for index := range admitted {
				store.Record(usage.Turn{At: time.UnixMilli(int64(index + 1)), ResponseID: fmt.Sprintf("held-%d", index), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
			}
			store.StopAdmission()
			const late = 3
			for range late {
				store.Record(usage.Turn{})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
			report := store.Close(ctx)
			cancel()
			_, _ = locker.Exec("ROLLBACK")
			if report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 || report.LateAfterCutoffDrops != late || report.FinalFlushLosses != admitted {
				t.Fatalf("settled report = %+v, want exactly %d admitted unconfirmed plus %d late", report, admitted, late)
			}
			// A worker finishing after the published unconfirmed settlement cannot
			// settle the same Turns again or change a later Close result.
			againCtx, againCancel := context.WithTimeout(context.Background(), time.Second)
			again := store.Close(againCtx)
			againCancel()
			if again != report {
				t.Fatalf("later Close report = %+v, want terminal one-way settlement %+v", again, report)
			}
		})
	}
}

func TestStoreFinalFlushUsesNativeTimeoutAndBoundsCoordinatorWait(t *testing.T) {
	path, store := openStore(t, io.Discard)
	locker := openExternal(t, path)
	if _, err := locker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	store.Record(usage.Turn{At: time.Now(), ResponseID: "contended-final", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	store.StopAdmission()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	started := time.Now()
	report := store.Close(ctx)
	elapsed := time.Since(started)
	cancel()
	if elapsed > 350*time.Millisecond {
		t.Fatalf("Close took %v, want coordinator bounded near 100ms despite native busy wait", elapsed)
	}
	if report.FinalFlushLosses == 0 || report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 {
		t.Fatalf("Close report = %+v, want contended final Turn counted lost", report)
	}
	_, _ = locker.Exec("ROLLBACK")
}

func TestStoreCopiesOptionalValuesAndHandlesConcurrentProducersAndCloseRaces(t *testing.T) {
	path, store := openStore(t, io.Discard)
	optional := int64(7)
	store.Record(usage.Turn{At: time.UnixMilli(1), ResponseID: "immutable", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1, CachedTokens: &optional}})
	optional = 99

	var producers sync.WaitGroup
	for worker := range 8 {
		producers.Add(1)
		go func() {
			defer producers.Done()
			for sequence := range 50 {
				store.Record(usage.Turn{At: time.UnixMilli(int64(sequence + 2)), ResponseID: fmt.Sprintf("worker-%d-%d", worker, sequence), Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
			}
		}()
	}
	producers.Wait()
	store.StopAdmission()
	// Late concurrent calls must be prompt and panic-free; none can be sent to a
	// closed channel because the Store never closes its admission queue.
	for range 20 {
		store.Record(usage.Turn{ResponseID: "late", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
	}
	report := closeStore(t, store)
	if report.LateAfterCutoffDrops != 20 || report.QueueFullDrops != 0 || report.RuntimeWriteLosses != 0 || report.FinalFlushLosses != 0 || !report.DriverCleanupCompleted {
		t.Fatalf("Close report = %+v", report)
	}
	db := openExternal(t, path)
	var cached int64
	if err := db.QueryRow(`SELECT cached_tokens FROM openai_turn WHERE response_id='immutable'`).Scan(&cached); err != nil || cached != 7 {
		t.Fatalf("immutable cached_tokens = %d, %v; want snapshot 7", cached, err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM openai_turn").Scan(&count); err != nil || count != 401 {
		t.Fatalf("rows = %d, %v; want 401 admitted Turns", count, err)
	}
}

func TestStoreCreatesPrivateArtifactsAndRejectsUnsafeDestinations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode contract; Windows is best effort and covered by cross-build")
	}
	t.Run("private under permissive umask", func(t *testing.T) {
		old := setUmask(0)
		defer setUmask(old)
		path := filepath.Join(t.TempDir(), "private", "usage.db")
		store, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
		if err != nil {
			t.Fatal(err)
		}
		store.Record(usage.Turn{At: time.Now(), ResponseID: "sidecars", Model: "m", Transport: usage.TransportBuffered, Usage: usage.OpenAIUsage{InputTokens: 1, OutputTokens: 1}})
		time.Sleep(1100 * time.Millisecond)
		for _, sidecar := range []string{path + "-wal", path + "-shm"} {
			info, err := os.Stat(sidecar)
			if err != nil || !info.Mode().IsRegular() || filepath.Dir(sidecar) != filepath.Dir(path) {
				t.Errorf("live sidecar %q = %#v, %v; want regular file beside main database", sidecar, info, err)
			}
		}
		if report := closeStore(t, store); !report.DriverCleanupCompleted {
			t.Fatal(report)
		}
		for artifact, want := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600} {
			info, err := os.Stat(artifact)
			if err != nil {
				t.Errorf("stat %s: %v", artifact, err)
				continue
			}
			if info.Mode().Perm() != want {
				t.Errorf("%s mode = %v, want %04o", artifact, info.Mode().Perm(), want)
			}
		}
	})

	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, parent, path string)
	}{
		{name: "shared parent", setup: func(t *testing.T, parent, _ string) {
			if err := os.Mkdir(parent, 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "public existing file", setup: func(t *testing.T, parent, path string) {
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("sentinel"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink destination", setup: func(t *testing.T, parent, path string) {
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(target, []byte("sentinel"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory destination", setup: func(t *testing.T, parent, path string) {
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			parent, path := filepath.Join(root, "parent"), filepath.Join(root, "parent", "usage.db")
			tc.setup(t, parent, path)
			before, _ := os.ReadFile(path)
			_, err := sqlitestore.Open(path, testStoreLogger(io.Discard))
			if err == nil {
				t.Fatal("Open succeeded for unsafe destination")
			}
			after, _ := os.ReadFile(path)
			if before != nil && !bytes.Equal(before, after) {
				t.Errorf("destination bytes changed from %q to %q", before, after)
			}
		})
	}
}

func TestStoreFinalAggregateUsesGovernedBoundedLossFields(t *testing.T) {
	var logs bytes.Buffer
	_, store := openStore(t, &logs)
	store.StopAdmission()
	store.Record(usage.Turn{})
	report := closeStore(t, store)
	if report.LateAfterCutoffDrops != 1 {
		t.Fatal(report)
	}
	line := logs.String()
	for _, want := range []string{
		"usage store finalized", logging.QueueFullDropsKey + "=0", logging.RuntimeWriteLossesKey + "=0",
		logging.LateAfterCutoffDropsKey + "=1", logging.FinalFlushLossesKey + "=0",
		logging.DriverCleanupCompletedKey + "=true",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("final log missing %q: %s", want, line)
		}
	}
}
