package sqlitestore

import (
	"net/url"
	"strconv"
	"time"
)

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
