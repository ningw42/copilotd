package sqlitestore

// QueuedForTest reports the number of admitted Turns that the writer has not
// yet dequeued. Tests use zero as an observable barrier before filling the
// bounded queue behind a writer blocked in SQLite.
func QueuedForTest(store *Store) int { return len(store.queue) }
