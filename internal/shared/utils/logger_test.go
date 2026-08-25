package utils

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitFileLoggerWritesToTheGivenPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "service.log")

	if err := InitFileLogger(path, false); err != nil {
		t.Fatalf("InitFileLogger() unexpected error: %v", err)
	}
	t.Cleanup(func() { logger = nil })

	GetLogger().Info("tunnel connected")
	Sync()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the log file: %v", err)
	}
	if !strings.Contains(string(data), "tunnel connected") {
		t.Fatalf("log file = %q, want it to contain the logged message", data)
	}
}

func TestRotateLogFileKeepsOneGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "service.log")

	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("failed to seed the log file: %v", err)
	}

	if err := rotateLogFile(path, 1024); err != nil {
		t.Fatalf("rotateLogFile() unexpected error: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a log file below the limit should not be rotated: %v", err)
	}

	if err := rotateLogFile(path, 1); err != nil {
		t.Fatalf("rotateLogFile() unexpected error: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the oversized log file should have been renamed, stat err = %v", err)
	}

	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("failed to read the rotated log file: %v", err)
	}
	if string(rotated) != "first" {
		t.Fatalf("rotated log = %q, want %q", rotated, "first")
	}

	// A second rotation overwrites the previous generation rather than piling up.
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("failed to write the log file: %v", err)
	}
	if err := rotateLogFile(path, 1); err != nil {
		t.Fatalf("rotateLogFile() unexpected error: %v", err)
	}

	rotated, err = os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("failed to read the rotated log file: %v", err)
	}
	if string(rotated) != "second" {
		t.Fatalf("rotated log = %q, want %q", rotated, "second")
	}
}

func TestRotateLogFileIgnoresAMissingFile(t *testing.T) {
	t.Parallel()

	if err := rotateLogFile(filepath.Join(t.TempDir(), "absent.log"), 1); err != nil {
		t.Fatalf("rotateLogFile() unexpected error: %v", err)
	}
}
