//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func readInterfaceTrafficCounters(interfaceName string) (uint64, uint64, error) {
	iface, err := net.InterfaceByName(interfaceName)
	if err != nil {
		return 0, 0, err
	}
	read := func(counter string) (uint64, error) {
		value, err := os.ReadFile(filepath.Join("/sys/class/net", iface.Name, "statistics", counter))
		if err != nil {
			return 0, err
		}
		parsed, err := strconv.ParseUint(strings.TrimSpace(string(value)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse %s %s counter: %w", iface.Name, counter, err)
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
