package sqliteprobe

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const processMigration = `CREATE TABLE process_evidence(value TEXT NOT NULL)`

func TestConcurrentProcessesCreateAndAdmitFreshDatabase(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("create private parent: %v", err)
	}
	path := filepath.Join(parent, "usage.db")
	startPath := filepath.Join(parent, "start")

	commands := make([]*exec.Cmd, 2)
	for index := range commands {
		command := exec.Command(os.Args[0], "-test.run=^TestAdmissionProcessHelper$")
		command.Env = append(os.Environ(),
			"COPILOTD_SQLITE_PROBE_HELPER=1",
			"COPILOTD_SQLITE_PROBE_PATH="+path,
			"COPILOTD_SQLITE_PROBE_START="+startPath,
		)
		commands[index] = command
		if err := commands[index].Start(); err != nil {
			t.Fatalf("start helper %d: %v", index, err)
		}
	}
	if err := os.WriteFile(startPath, nil, 0o600); err != nil {
		t.Fatalf("release process barrier: %v", err)
	}
	for index := range commands {
		if err := commands[index].Wait(); err != nil {
			t.Errorf("helper %d: %v", index, err)
		}
	}

	admission, err := Admit(context.Background(), path, AdmissionOptions{Migrations: []string{processMigration}})
	if err != nil {
		t.Fatalf("inspect process-created database: %v", err)
	}
	defer admission.Close()
	if admission.UserVersion != 1 {
		t.Errorf("user_version = %d, want 1", admission.UserVersion)
	}
}

func TestAdmissionProcessHelper(t *testing.T) {
	if os.Getenv("COPILOTD_SQLITE_PROBE_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	path := os.Getenv("COPILOTD_SQLITE_PROBE_PATH")
	startPath := os.Getenv("COPILOTD_SQLITE_PROBE_START")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(startPath); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatalf("inspect process barrier: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for process barrier")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := PreparePrivateDatabase(path); err != nil {
		t.Fatalf("prepare from helper process: %v", err)
	}
	admission, err := Admit(context.Background(), path, AdmissionOptions{Migrations: []string{processMigration}})
	if err != nil {
		t.Fatalf("admit from helper process: %v", err)
	}
	if err := admission.Close(); err != nil {
		t.Fatalf("close helper admission: %v", err)
	}
	fmt.Fprintln(os.Stdout, "helper admitted")
}
