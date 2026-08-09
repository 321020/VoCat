//go:build linux

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// netIfCounters reads the interface's cumulative rx/tx byte counters from
// /sys/class/net. The interface briefly disappears while QMI reconnects, in
// which case an error is returned and the caller re-baselines.
func netIfCounters(iface string) (uint64, uint64, error) {
	if strings.TrimSpace(iface) == "" {
		return 0, 0, fmt.Errorf("interface name is empty")
	}
	read := func(counter string) (uint64, error) {
		raw, err := os.ReadFile(filepath.Join("/sys/class/net", iface, "statistics", counter))
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse %s %s counter: %w", iface, counter, err)
		}
		return parsed, nil
	}
	rxBytes, err := read("rx_bytes")
	if err != nil {
		return 0, 0, err
	}
	txBytes, err := read("tx_bytes")
	if err != nil {
		return 0, 0, err
	}
	return rxBytes, txBytes, nil
}
