//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func lockServerInstance(databasePath string) (*os.File, error) {
	directory := filepath.Dir(databasePath)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create data directory for instance lock: %w", err)
	}
	path := filepath.Join(directory, ".vocat.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open server instance lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("another vocat server is already using database %s", databasePath)
		}
		return nil, fmt.Errorf("lock server instance: %w", err)
	}
	return file, nil
}
