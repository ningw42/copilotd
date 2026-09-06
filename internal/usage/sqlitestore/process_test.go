package sqlitestore_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ningw42/copilotd/internal/usage"
	"github.com/ningw42/copilotd/internal/usage/sqlitestore"
)

func TestStoreConcurrentProcessesOpenRecordAndClose(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	var results []<-chan error
	var outputs []*bytes.Buffer
	t.Cleanup(func() {
		// Also kill and reap children on a failed barrier/assertion. Each Wait
		// runs once, and WaitDelay bounds any pipe-copy cleanup after killing.
		cancel()
		for _, result := range results {
			<-result
		}
	})
	ids := []string{"process-a", "process-b"}
	for _, id := range ids {
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStoreProcessHelper$", "-test.timeout=15s")
		command.Env = append(os.Environ(), "COPILOTD_USAGE_PROCESS_ID="+id, "COPILOTD_USAGE_PROCESS_DIR="+parent)
		command.WaitDelay = time.Second
		output := new(bytes.Buffer)
		command.Stdout, command.Stderr = output, output
		if err := command.Start(); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		result := make(chan error, 1)
		results = append(results, result)
		outputs = append(outputs, output)
		go func() {
			result <- command.Wait()
			close(result)
		}()
	}
	// Both OS processes must reach the pre-Open barrier before either may
	// touch the database. This attempts concurrent fresh opens; it does not
	// claim a measured overlap inside SQLite or count O_EXCL creation winners.
	for _, id := range ids {
		waitForProcessFile(t, ctx, filepath.Join(parent, "ready-"+id))
	}
	path := filepath.Join(parent, "usage.db")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("database exists before releasing fresh-open barrier: %v", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "start"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for index, result := range results {
		if err := <-result; err != nil {
			t.Errorf("%s: %v\n%s", ids[index], err, outputs[index].String())
		}
	}
	if t.Failed() {
		return
	}

	db := openExternal(t, path)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("shared user_version = %d, %v; want 1", version, err)
	}
	rows, err := db.Query("SELECT response_id, input_tokens, output_tokens FROM openai_turn ORDER BY response_id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		var input, output int64
		if err := rows.Scan(&id, &input, &output); err != nil {
			t.Fatal(err)
		}
		if input != 2 || output != 3 {
			t.Errorf("%s counts = %d/%d, want 2/3", id, input, output)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, ids) {
		t.Fatalf("shared rows = %v, want one committed Turn from each process %v", got, ids)
	}
	t.Logf("both ready before fresh-open release; shared schema=%d rows=%v", version, got)
}

func TestStoreProcessHelper(t *testing.T) {
	id := os.Getenv("COPILOTD_USAGE_PROCESS_ID")
	if id == "" {
		t.Skip("subprocess helper")
	}
	parent := os.Getenv("COPILOTD_USAGE_PROCESS_DIR")
	if err := os.WriteFile(filepath.Join(parent, "ready-"+id), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	waitForProcessFile(t, ctx, filepath.Join(parent, "start"))
	store, err := sqlitestore.Open(filepath.Join(parent, "usage.db"), testStoreLogger(io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	store.Record(usage.Turn{
		At: time.UnixMilli(1), ResponseID: id, Model: "m", Transport: usage.TransportBuffered,
		Usage: usage.OpenAIUsage{InputTokens: 2, OutputTokens: 3},
	})
	if report := closeStore(t, store); report != (sqlitestore.Report{DriverCleanupCompleted: true}) {
		t.Fatalf("bounded Close report = %+v, want no loss and completed cleanup", report)
	}
}

func waitForProcessFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for process barrier %q: %v", path, ctx.Err())
		case <-poll.C:
		}
	}
}
