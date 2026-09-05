package sqliteprobe

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentFreshAdmissionsSerializeVersionCheckInsideBeginImmediate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}

	const openers = 2
	start := make(chan struct{})
	results := make(chan *Admission, openers)
	errors := make(chan error, openers)
	var wait sync.WaitGroup
	for range openers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			admission, err := Admit(context.Background(), path, AdmissionOptions{
				Migrations: []string{`CREATE TABLE evidence(value TEXT NOT NULL)`},
			})
			if err != nil {
				errors <- err
				return
			}
			results <- admission
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Errorf("concurrent admission: %v", err)
	}
	var admitted int
	for admission := range results {
		admitted++
		if admission.UserVersion != 1 {
			t.Errorf("user_version = %d, want 1", admission.UserVersion)
		}
		if err := admission.Close(); err != nil {
			t.Errorf("close admission: %v", err)
		}
	}
	if admitted != openers {
		t.Fatalf("admitted = %d, want %d", admitted, openers)
	}
}

func TestExternalReaderCoexistsWithAdmittedWALWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	ctx := context.Background()
	admission, err := Admit(ctx, path, AdmissionOptions{
		Migrations: []string{`CREATE TABLE evidence(value TEXT NOT NULL)`},
	})
	if err != nil {
		t.Fatalf("admit database: %v", err)
	}
	defer admission.Close()
	if _, err := admission.Conn.ExecContext(ctx, `INSERT INTO evidence(value) VALUES ('before')`); err != nil {
		t.Fatalf("insert initial row: %v", err)
	}

	readerDB, err := sql.Open("sqlite", sqliteDSN(path, StartupContentionBudget))
	if err != nil {
		t.Fatalf("open external reader: %v", err)
	}
	defer readerDB.Close()
	reader, err := readerDB.Conn(ctx)
	if err != nil {
		t.Fatalf("external reader connection: %v", err)
	}
	defer reader.Close()
	if _, err := reader.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		t.Fatalf("make external connection read-only: %v", err)
	}
	if _, err := reader.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("begin external read snapshot: %v", err)
	}
	defer reader.ExecContext(context.Background(), `ROLLBACK`)
	var count int
	if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM evidence`).Scan(&count); err != nil {
		t.Fatalf("read initial snapshot: %v", err)
	}
	if count != 1 {
		t.Fatalf("initial reader count = %d, want 1", count)
	}

	if _, err := admission.Conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("begin admitted writer while reader active: %v", err)
	}
	if _, err := admission.Conn.ExecContext(ctx, `INSERT INTO evidence(value) VALUES ('during-reader')`); err != nil {
		t.Fatalf("insert while reader active: %v", err)
	}
	if _, err := admission.Conn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("commit while reader active: %v", err)
	}
	if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM evidence`).Scan(&count); err != nil {
		t.Fatalf("read stable snapshot: %v", err)
	}
	if count != 1 {
		t.Errorf("snapshot reader count = %d, want stable 1", count)
	}
	if _, err := reader.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("end external read snapshot: %v", err)
	}
	if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM evidence`).Scan(&count); err != nil {
		t.Fatalf("read post-commit rows: %v", err)
	}
	if count != 2 {
		t.Errorf("post-commit reader count = %d, want 2", count)
	}
}

func TestNewerUserVersionIsCheckedInsideTransactionAndFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	newer, err := Admit(context.Background(), path, AdmissionOptions{
		Migrations: []string{
			`CREATE TABLE version_one(value TEXT NOT NULL)`,
			`CREATE TABLE version_two(value TEXT NOT NULL)`,
		},
	})
	if err != nil {
		t.Fatalf("create newer schema: %v", err)
	}
	if err := newer.Close(); err != nil {
		t.Fatalf("close newer schema: %v", err)
	}

	var stages []Stage
	_, err = Admit(context.Background(), path, AdmissionOptions{
		Migrations: []string{`CREATE TABLE version_one(value TEXT NOT NULL)`},
		Trace: func(event StageEvent) {
			if event.Before {
				stages = append(stages, event.Stage)
			}
		},
	})
	if err == nil {
		t.Fatal("newer schema admitted by older migration set")
	}
	if !strings.Contains(err.Error(), "user_version 2 is newer than supported version 1") {
		t.Errorf("newer-version error = %v", err)
	}
	beginIndex, versionIndex := -1, -1
	for index, stage := range stages {
		switch stage {
		case StageBeginImmediate:
			beginIndex = index
		case StageUserVersion:
			versionIndex = index
		}
	}
	if beginIndex < 0 || versionIndex <= beginIndex {
		t.Errorf("stage order = %v, want BEGIN IMMEDIATE before user_version", stages)
	}
}

func TestExistingNonDatabaseIsNotTruncatedOrRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	original := []byte("not an operator database; preserve these bytes")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write invalid existing file: %v", err)
	}
	created, err := PreparePrivateDatabase(path)
	if err != nil {
		t.Fatalf("validate existing regular file: %v", err)
	}
	if created {
		t.Fatal("existing file reported newly created")
	}

	var attempts int
	_, err = Admit(context.Background(), path, AdmissionOptions{
		Trace: func(event StageEvent) {
			if event.Before && event.Stage == StageOpen {
				attempts++
			}
		},
	})
	if err == nil {
		t.Fatal("invalid database admitted")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want one non-contention attempt", attempts)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read existing file after failure: %v", readErr)
	}
	if !bytes.Equal(after, original) {
		t.Errorf("existing file changed: got %q, want %q", after, original)
	}
}

func TestMigrationFailureAfterAcquisitionRollsBackWithoutRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	var beginAttempts int
	_, err := Admit(context.Background(), path, AdmissionOptions{
		Migrations: []string{
			`CREATE TABLE should_rollback(value TEXT NOT NULL)`,
			`THIS IS NOT SQL`,
		},
		Trace: func(event StageEvent) {
			if event.Before && event.Stage == StageBeginImmediate {
				beginAttempts++
			}
		},
	})
	if err == nil {
		t.Fatal("migration succeeded, want syntax error")
	}
	if !strings.Contains(err.Error(), "apply migration 2") {
		t.Errorf("migration error = %v, want migration index", err)
	}
	if beginAttempts != 1 {
		t.Errorf("BEGIN attempts = %d, want no post-acquisition retry", beginAttempts)
	}

	admission, err := Admit(context.Background(), path, AdmissionOptions{})
	if err != nil {
		t.Fatalf("reopen rolled-back database: %v", err)
	}
	defer admission.Close()
	if admission.UserVersion != 0 {
		t.Errorf("user_version = %d, want rolled back 0", admission.UserVersion)
	}
	var tables int
	if err := admission.Conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='should_rollback'`).Scan(&tables); err != nil {
		t.Fatalf("query rolled-back table: %v", err)
	}
	if tables != 0 {
		t.Errorf("rolled-back tables = %d, want 0", tables)
	}
}

func TestAdmitConfiguresDedicatedWALConnectionAndMigratesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "usage.db")
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare database: %v", err)
	}

	admission, err := Admit(context.Background(), path, AdmissionOptions{
		Migrations: []string{`CREATE TABLE evidence(value TEXT NOT NULL)`},
	})
	if err != nil {
		t.Fatalf("admit database: %v", err)
	}
	t.Cleanup(func() {
		if err := admission.Close(); err != nil {
			t.Errorf("close admission: %v", err)
		}
	})

	if got := admission.SQLiteVersion; got != "3.53.4" {
		t.Errorf("SQLite version = %q, want 3.53.4", got)
	}
	if got := admission.JournalMode; got != "wal" {
		t.Errorf("journal mode = %q, want wal", got)
	}
	if got := admission.Synchronous; got != 1 {
		t.Errorf("synchronous = %d, want 1 (NORMAL)", got)
	}
	if got := admission.UserVersion; got != 1 {
		t.Errorf("user_version = %d, want 1", got)
	}
	var runtimeBusyTimeout int
	if err := admission.Conn.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&runtimeBusyTimeout); err != nil {
		t.Fatalf("read admitted busy timeout: %v", err)
	}
	if runtimeBusyTimeout != 5000 {
		t.Errorf("admitted busy_timeout = %d, want full 5000ms runtime policy", runtimeBusyTimeout)
	}
	var table string
	if err := admission.Conn.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name='evidence'`).Scan(&table); err != nil {
		t.Fatalf("query migration result: %v", err)
	}
	if table != "evidence" {
		t.Errorf("table = %q, want evidence", table)
	}
}
