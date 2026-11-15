package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The coordinator directory defaults to ~/coordinator when it exists, else
// to ~ as before.
func TestDefaultCoordDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := defaultCoordDir(); got != "~" {
		t.Fatalf("no ~/coordinator: %q", got)
	}
	os.Mkdir(filepath.Join(os.Getenv("HOME"), "coordinator"), 0o755)
	if got := defaultCoordDir(); got != "~/coordinator" {
		t.Fatalf("with ~/coordinator: %q", got)
	}
}
