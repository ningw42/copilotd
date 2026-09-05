// Command driverprobe links and exercises the disposable SQLite feasibility
// module. Every database it creates lives under a private temporary directory
// and is removed; it must never be pointed at an operator database.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	sqliteprobe "github.com/ningw42/copilotd/docs/research/sqlite-feasibility"
)

func main() {
	parent, err := os.MkdirTemp("", "copilotd-sqlite-driver-probe-")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(parent)
	if err := os.Chmod(parent, 0o700); err != nil {
		fatal(err)
	}
	path := filepath.Join(parent, "usage.db")
	if _, err := sqliteprobe.PreparePrivateDatabase(path); err != nil {
		fatal(err)
	}
	admission, err := sqliteprobe.Admit(context.Background(), path, sqliteprobe.AdmissionOptions{
		Migrations: []string{`CREATE TABLE link_probe(value TEXT NOT NULL)`},
	})
	if err != nil {
		fatal(err)
	}
	defer admission.Close()
	if _, err := admission.Conn.ExecContext(context.Background(), `INSERT INTO link_probe(value) VALUES ('linked')`); err != nil {
		fatal(err)
	}
	var value string
	if err := admission.Conn.QueryRowContext(context.Background(), `SELECT value FROM link_probe`).Scan(&value); err != nil {
		fatal(err)
	}
	fmt.Printf("driver=%s sqlite=%s journal_mode=%s synchronous=%d value=%s\n",
		sqliteprobe.CandidateDriverVersion,
		admission.SQLiteVersion,
		admission.JournalMode,
		admission.Synchronous,
		value,
	)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
