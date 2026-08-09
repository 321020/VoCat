package exportproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const ipInfoURL = "https://ipinfo.io/json"

type PublicIPInfo struct {
	IP           string `json:"ip"`
	CountryCode  string `json:"country_code"`
	Region       string `json:"region"`
	City         string `json:"city"`
	Organization string `json:"organization,omitempty"`
}

// LookupPublicIP sends the lookup through the same marked, interface-bound
// dialer and isolated DNS resolver as Export Proxy. It therefore reports the
// modem's roaming exit rather than the host or browser's default connection.
func LookupPublicIP(ctx context.Context, networkInterface string) (PublicIPInfo, error) {
	networkInterface = strings.TrimSpace(networkInterface)
	if networkInterface == "" {
		return PublicIPInfo{}, errors.New("cellular network interface is required")
	}
	if err := platformSupported(); err != nil {
		return PublicIPInfo{}, err
	}
	dialer := boundDialer(networkInterface)
	resolver := boundResolver(networkInterface)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return dialTarget(ctx, address, &dialer, resolver)
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 12 * time.Second,
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ipInfoURL, nil)
	if err != nil {
		return PublicIPInfo{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "VoCat/1.0")
	response, err := transport.RoundTrip(request)
	if err != nil {
		return PublicIPInfo{}, fmt.Errorf("query ipinfo.io through %s: %w", networkInterface, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return PublicIPInfo{}, fmt.Errorf("ipinfo.io returned HTTP %d", response.StatusCode)
	}
	return decodePublicIPInfo(io.LimitReader(response.Body, 64<<10))
}

func decodePublicIPInfo(reader io.Reader) (PublicIPInfo, error) {
	var response struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
		Region  string `json:"region"`
		City    string `json:"city"`
		Org     string `json:"org"`
	}
	if err := json.NewDecoder(reader).Decode(&response); err != nil {
		return PublicIPInfo{}, fmt.Errorf("decode ipinfo.io response: %w", err)
	}
	response.IP = strings.TrimSpace(response.IP)
	response.Country = strings.ToUpper(strings.TrimSpace(response.Country))
	if net.ParseIP(response.IP) == nil {
		return PublicIPInfo{}, errors.New("ipinfo.io response contained no valid IP address")
	}
	if len(response.Country) != 2 {
		return PublicIPInfo{}, errors.New("ipinfo.io response contained no valid country code")
	}
	return PublicIPInfo{
		IP: response.IP, CountryCode: response.Country,
		Region: strings.TrimSpace(response.Region), City: strings.TrimSpace(response.City),
		Organization: strings.TrimSpace(response.Org),
	}, nil
}
