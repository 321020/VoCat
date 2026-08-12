//go:build linux

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestServerInstanceLockRejectsSecondProcess(t *testing.T) {
	database := filepath.Join(t.TempDir(), "vocat.db")
	first, err := lockServerInstance(database)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := lockServerInstance(database)
	if second != nil {
		second.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "already using database") {
		t.Fatalf("second lock error = %v", err)
	}
}
