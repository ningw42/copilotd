package sqlitestore

import (
	"net/url"
	"strconv"
	"time"
)

// LiteralFileURL encodes an absolute filesystem path using the writer-owned
// platform policy. Usage readers may add their own connection-local parameters;
// filename punctuation never becomes a driver option.
func LiteralFileURL(path string) *url.URL { return sqliteFileURL(path) }

// SchemaVersion is the current writer-owned migration version.
func SchemaVersion() int { return len(migrationNames) }

// sqliteDSN encodes an already-resolved filesystem destination as a SQLite file
// URI. Filename punctuation is escaped into the URI path; the query contains
// only store-owned, driver-validated connection parameters.
func sqliteDSN(path string, timeout time.Duration) string {
	uri := sqliteFileURL(path)
	query := url.Values{}
	query.Set("_busy_timeout", strconv.FormatInt(busyTimeoutMilliseconds(timeout), 10))
	uri.RawQuery = query.Encode()
	return uri.String()
}

func busyTimeoutMilliseconds(timeout time.Duration) int64 {
	milliseconds := timeout.Milliseconds()
	if milliseconds < 1 {
		milliseconds = 1
	}
	if milliseconds > runtimeBusyTimeout.Milliseconds() {
		milliseconds = runtimeBusyTimeout.Milliseconds()
	}
	return milliseconds
}
