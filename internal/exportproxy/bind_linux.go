//go:build linux

package exportproxy

import (
	"bufio"
	"context"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func platformSupported() error { return nil }

func boundDialer(networkInterface string) net.Dialer {
	return net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
		var bindError error
		err := raw.Control(func(fd uintptr) {
			if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, int(exportRouteMark(networkInterface))); err != nil {
				bindError = err
				return
			}
			bindError = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, networkInterface)
		})
		if err != nil {
			return err
		}
		return bindError
	}}
}

func exportRouteMark(networkInterface string) uint32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(networkInterface))
	return 0x56000000 | (hash.Sum32() & 0x00ffffff)
}

func boundResolver(networkInterface string) *net.Resolver {
	dialer := boundDialer(networkInterface)
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var lastError error
		for _, server := range exportRouteDNSServers(networkInterface) {
			connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(server, "53"))
			if err == nil {
				return connection, nil
			}
			lastError = err
		}
		return nil, lastError
	}}
}

func exportRouteDNSServers(networkInterface string) []string {
	safeName := strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			return character
		}
		return '_'
	}, networkInterface)
	file, err := os.Open(filepath.Join("/run/vocat", "cellular-"+safeName+".dns"))
	if err != nil {
		return []string{"1.1.1.1", "8.8.8.8"}
	}
	defer file.Close()
	servers := make([]string, 0, 2)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if value := strings.TrimSpace(scanner.Text()); net.ParseIP(value) != nil {
			servers = append(servers, value)
		}
	}
	if len(servers) == 0 {
		return []string{"1.1.1.1", "8.8.8.8"}
	}
	return servers
}
