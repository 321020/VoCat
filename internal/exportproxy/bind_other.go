//go:build !linux

package exportproxy

import (
	"errors"
	"net"
)

func platformSupported() error           { return errors.New("built-in export proxy is only available on Linux") }
func boundDialer(string) net.Dialer      { return net.Dialer{} }
func boundResolver(string) *net.Resolver { return net.DefaultResolver }
