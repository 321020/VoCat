//go:build !linux

package server

import "fmt"

// netIfCounters is only meaningful on the Linux deployment target; elsewhere
// there is no cellular /sys interface to read.
func netIfCounters(string) (uint64, uint64, error) {
	return 0, 0, fmt.Errorf("interface counters are only available on Linux")
}
