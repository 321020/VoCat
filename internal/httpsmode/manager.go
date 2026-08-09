package httpsmode

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"vocat/internal/store"
)

const SettingKey = "transport.self_signed_https"

type State struct {
	Enabled     bool      `json:"enabled"`
	HTTPURL     string    `json:"http_url"`
	HTTPSURL    string    `json:"https_url"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	NotAfter    time.Time `json:"not_after,omitempty"`
}

type Manager struct {
	store   *store.Store
	dir     string
	address string
	enabled atomic.Bool
	mu      sync.RWMutex
	cert    *tls.Certificate
}

func New(ctx context.Context, database *store.Store, dir, address string) (*Manager, error) {
	manager := &Manager{store: database, dir: dir, address: address}
	setting, err := database.AppSetting(ctx, SettingKey)
	if err == nil {
		var document struct {
			Enabled bool `json:"enabled"`
		}
		if json.Unmarshal(setting.Value, &document) == nil && document.Enabled {
			if err := manager.ensureCertificate(); err != nil {
				return nil, err
			}
			manager.enabled.Store(true)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return manager, nil
}

func (manager *Manager) Enabled() bool { return manager != nil && manager.enabled.Load() }

func (manager *Manager) SetEnabled(ctx context.Context, enabled bool) (State, error) {
	if enabled {
		if err := manager.ensureCertificate(); err != nil {
			return State{}, err
		}
	}
	raw, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return State{}, err
	}
	if err := manager.store.UpsertAppSetting(ctx, store.AppSetting{Key: SettingKey, Value: raw}); err != nil {
		return State{}, err
	}
	manager.enabled.Store(enabled)
	return manager.State(""), nil
}

func (manager *Manager) State(host string) State {
	host = strings.TrimSpace(host)
	if host == "" {
		host = manager.address
	}
	state := State{
		Enabled:  manager.Enabled(),
		HTTPURL:  "http://" + host,
		HTTPSURL: "https://" + host,
	}
	manager.mu.RLock()
	if manager.cert != nil && manager.cert.Leaf != nil {
		digest := sha256.Sum256(manager.cert.Leaf.Raw)
		encoded := strings.ToUpper(hex.EncodeToString(digest[:]))
		parts := make([]string, 0, len(encoded)/2)
		for len(encoded) >= 2 {
			parts = append(parts, encoded[:2])
			encoded = encoded[2:]
		}
		state.Fingerprint = strings.Join(parts, ":")
		state.NotAfter = manager.cert.Leaf.NotAfter
	}
	manager.mu.RUnlock()
	return state
}

func (manager *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			manager.mu.RLock()
			defer manager.mu.RUnlock()
			if manager.cert == nil {
				return nil, errors.New("self-signed certificate is unavailable")
			}
			return manager.cert, nil
		},
	}
}

func (manager *Manager) CertificatePEM() ([]byte, error) {
	if err := manager.ensureCertificate(); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(manager.dir, "selfsigned.crt"))
}

func (manager *Manager) ensureCertificate() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.cert != nil && manager.cert.Leaf != nil && time.Until(manager.cert.Leaf.NotAfter) > 30*24*time.Hour {
		return nil
	}
	if err := os.MkdirAll(manager.dir, 0o750); err != nil {
		return fmt.Errorf("create TLS directory: %w", err)
	}
	certPath := filepath.Join(manager.dir, "selfsigned.crt")
	keyPath := filepath.Join(manager.dir, "selfsigned.key")
	if cert, err := loadCertificate(certPath, keyPath); err == nil && time.Until(cert.Leaf.NotAfter) > 30*24*time.Hour {
		manager.cert = cert
		return nil
	}
	certPEM, keyPEM, err := generateCertificate(manager.address)
	if err != nil {
		return err
	}
	if err := writePrivateFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	if err := writePrivateFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	cert, err := loadCertificate(certPath, keyPath)
	if err != nil {
		return err
	}
	manager.cert = cert
	return nil
}

func loadCertificate(certPath, keyPath string) (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("certificate chain is empty")
	}
	cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

func generateCertificate(address string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "VoCat self-signed local certificate", Organization: []string{"VoCat"}},
		NotBefore:    now.Add(-5 * time.Minute), NotAfter: now.AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	if hostname, hostnameErr := os.Hostname(); hostnameErr == nil && strings.TrimSpace(hostname) != "" {
		template.DNSNames = append(template.DNSNames, strings.TrimSpace(hostname))
	}
	if host, _, splitErr := net.SplitHostPort(address); splitErr == nil {
		if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if host != "" && host != "0.0.0.0" && host != "::" {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	if interfaces, interfaceErr := net.InterfaceAddrs(); interfaceErr == nil {
		for _, item := range interfaces {
			text := item.String()
			if slash := strings.IndexByte(text, '/'); slash >= 0 {
				text = text[:slash]
			}
			if ip := net.ParseIP(strings.TrimSpace(text)); ip != nil && !ip.IsUnspecified() {
				template.IPAddresses = append(template.IPAddresses, ip)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func writePrivateFile(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".tls-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}
