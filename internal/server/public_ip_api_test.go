package server

import (
	"testing"

	"vocat/internal/exportproxy"
)

func TestPublicIPCacheFollowsCurrentICCID(t *testing.T) {
	server := &Server{publicIPs: make(map[string]cachedPublicIP)}
	want := exportproxy.PublicIPInfo{IP: "203.0.113.8", CountryCode: "GB"}
	server.savePublicIP("ec20", "8944100001", want)

	got, ok := server.loadPublicIP("ec20", "8944100001")
	if !ok || got != want {
		t.Fatalf("loadPublicIP() = (%+v, %v), want (%+v, true)", got, ok, want)
	}

	if _, ok := server.loadPublicIP("ec20", "8944100002"); ok {
		t.Fatal("cache survived an ICCID change")
	}
	if _, ok := server.loadPublicIP("ec20", "8944100001"); ok {
		t.Fatal("stale cache was not deleted after an ICCID change")
	}
}

func TestPublicIPCacheClearsWhileModemIsResetting(t *testing.T) {
	server := &Server{publicIPs: make(map[string]cachedPublicIP)}
	server.savePublicIP("ec20", "8944100001", exportproxy.PublicIPInfo{IP: "203.0.113.8", CountryCode: "GB"})
	if _, ok := server.loadPublicIP("ec20", ""); ok {
		t.Fatal("cache survived a missing live ICCID")
	}
}
