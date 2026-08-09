//go:build !linux

package main

import "errors"

func readInterfaceTrafficCounters(string) (uint64, uint64, error) {
	return 0, 0, errors.New("interface traffic counters are only available on Linux")
}
