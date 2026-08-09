package httpsmode

import (
	"context"
	"crypto/tls"
	"net"
	"path/filepath"
	"testing"

	"vocat/internal/store"
)

func TestManagerPersistsToggleAndCertificate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dir, "vocat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager, err := New(ctx, database, filepath.Join(dir, "tls"), "0.0.0.0:7575")
	if err != nil {
		t.Fatal(err)
	}
	state, err := manager.SetEnabled(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Enabled || state.Fingerprint == "" || state.NotAfter.IsZero() {
		t.Fatalf("enabled state = %#v", state)
	}
	certificate, err := manager.CertificatePEM()
	if err != nil || len(certificate) == 0 {
		t.Fatalf("certificate = %d bytes, %v", len(certificate), err)
	}
	reloaded, err := New(ctx, database, filepath.Join(dir, "tls"), "0.0.0.0:7575")
	if err != nil || !reloaded.Enabled() {
		t.Fatalf("reloaded manager enabled=%v error=%v", reloaded.Enabled(), err)
	}
	if _, err := reloaded.SetEnabled(ctx, false); err != nil || reloaded.Enabled() {
		t.Fatalf("disable enabled=%v error=%v", reloaded.Enabled(), err)
	}
}

func TestMultiplexerRoutesPlainAndTLS(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(dir, "vocat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager, err := New(ctx, database, filepath.Join(dir, "tls"), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mux := NewMultiplexer(base, manager)
	defer mux.Close()

	plainClient, err := net.Dial("tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer plainClient.Close()
	if _, err := plainClient.Write([]byte("GET / HTTP/1.1\r\nHost: local\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	plainServer, err := mux.Plain().Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer plainServer.Close()

	tlsResult := make(chan error, 1)
	go func() {
		serverConn, acceptErr := mux.TLS().Accept()
		if acceptErr != nil {
			tlsResult <- acceptErr
			return
		}
		defer serverConn.Close()
		tlsResult <- tls.Server(serverConn, manager.TLSConfig()).Handshake()
	}()
	tlsClient, err := tls.Dial("tcp", base.Addr().String(), &tls.Config{InsecureSkipVerify: true}) // test-only local certificate
	if err != nil {
		t.Fatal(err)
	}
	_ = tlsClient.Close()
	if err := <-tlsResult; err != nil {
		t.Fatal(err)
	}
}
