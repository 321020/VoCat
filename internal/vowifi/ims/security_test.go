package ims

import (
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
)

func TestParseSecurityAgreementSelectsSupportedIPSec(t *testing.T) {
	proposal := securityProposal{
		spiClient:  1001,
		spiServer:  1002,
		portClient: 40666,
		portServer: 55610,
	}
	selected := "ipsec-3gpp;q=0.100;alg=hmac-sha-1-96;prot=esp;mod=trans;" +
		"ealg=aes-cbc;spi-c=2001;spi-s=2002;port-c=50601;port-s=50600"
	unsupported := "digest;q=0.900"
	agreement, err := parseSecurityAgreement(
		[]string{unsupported + ", " + selected},
		proposal,
	)
	if err != nil {
		t.Fatalf("parseSecurityAgreement() error = %v", err)
	}
	if agreement.selected.spiClient != 2001 ||
		agreement.selected.spiServer != 2002 ||
		agreement.selected.portClient != 50601 ||
		agreement.selected.portServer != 50600 {
		t.Fatalf("selected mechanism = %#v", agreement.selected)
	}
	if agreement.verifyValue != unsupported+", "+selected {
		t.Fatalf("Security-Verify = %q", agreement.verifyValue)
	}
}

func TestParseSecurityAgreementSkipsIncompleteCarrierAlternatives(t *testing.T) {
	proposal := securityProposal{
		spiClient:  1001,
		spiServer:  1002,
		portClient: 40666,
		portServer: 55610,
	}
	incomplete := []string{
		"ipsec-3gpp;q=0.100;alg=hmac-md5-96;mod=trans",
		"ipsec-3gpp;q=0.200;alg=hmac-sha-1-96;ealg=des-ede3-cbc;mod=trans",
		"ipsec-3gpp;q=0.300;alg=hmac-sha-1-96;ealg=aes-cbc;mod=trans",
	}
	selected := "ipsec-3gpp;q=1.000;alg=hmac-sha-1-96;prot=esp;mod=trans;" +
		"ealg=aes-cbc;spi-c=2001;spi-s=2002;port-c=50601;port-s=50600"
	values := []string{strings.Join(append(incomplete, selected), ", ")}

	agreement, err := parseSecurityAgreement(values, proposal)
	if err != nil {
		t.Fatalf("parseSecurityAgreement() error = %v", err)
	}
	if agreement.selected.spiClient != 2001 || agreement.selected.spiServer != 2002 {
		t.Fatalf("selected mechanism = %#v", agreement.selected)
	}
}

func TestParseSecurityAgreementFailsClosed(t *testing.T) {
	proposal := securityProposal{
		spiClient:  1001,
		spiServer:  1002,
		portClient: 40666,
		portServer: 55610,
	}
	valid := "ipsec-3gpp;q=0.100;alg=hmac-sha-1-96;prot=esp;mod=trans;" +
		"ealg=aes-cbc;spi-c=2001;spi-s=2002;port-c=50601;port-s=50600"
	for _, test := range []struct {
		name   string
		values []string
	}{
		{
			name: "unsupported integrity algorithm",
			values: []string{
				strings.Replace(valid, "hmac-sha-1-96", "hmac-md5-96", 1),
			},
		},
		{
			name: "server SPI collides with UE SPI",
			values: []string{
				strings.Replace(valid, "spi-c=2001", "spi-c=1001", 1),
			},
		},
		{
			name: "server SPIs collide",
			values: []string{
				strings.Replace(valid, "spi-s=2002", "spi-s=2001", 1),
			},
		},
		{
			name: "malformed ipsec offer poisons otherwise valid list",
			values: []string{
				valid + ", ipsec-3gpp;q=0.200;alg=hmac-sha-1-96;alg=hmac-sha-1-96",
			},
		},
		{
			name: "partially specified SA parameters poison otherwise valid list",
			values: []string{
				valid + ", ipsec-3gpp;q=0.200;alg=hmac-sha-1-96;spi-c=3001",
			},
		},
		{
			name:   "no offer",
			values: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseSecurityAgreement(test.values, proposal)
			if err == nil {
				t.Fatal("parseSecurityAgreement() error = nil")
			}
		})
	}
}

func TestExpandIPSecKeys(t *testing.T) {
	ck := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	ik := []byte{16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31}
	encryption, integrity, err := expandIPSecKeys(ck, ik)
	if err != nil {
		t.Fatalf("expandIPSecKeys() error = %v", err)
	}
	if !reflect.DeepEqual(encryption, ck) {
		t.Fatalf("encryption key = %v, want %v", encryption, ck)
	}
	wantIntegrity := append(append([]byte(nil), ik...), 0, 0, 0, 0)
	if !reflect.DeepEqual(integrity, wantIntegrity) {
		t.Fatalf("integrity key = %v, want %v", integrity, wantIntegrity)
	}
	encryption[0] ^= 0xff
	integrity[0] ^= 0xff
	if ck[0] != 0 || ik[0] != 16 {
		t.Fatal("expanded keys alias AKA key material")
	}
}

func TestXFRMPlanContainsFourStatesAndProtocolSpecificPolicies(t *testing.T) {
	config := testIPSecSAConfig()
	install, err := buildXFRMInstallPlan(config)
	if err != nil {
		t.Fatalf("buildXFRMInstallPlan() error = %v", err)
	}
	if len(install) != 10 {
		t.Fatalf("install operation count = %d, want 10", len(install))
	}
	for index, operation := range install[:4] {
		if !containsArguments(operation.arguments, "xfrm", "state", "add") {
			t.Fatalf("operation %d is not a state add: %v", index, operation.arguments)
		}
	}
	for index, operation := range install[4:] {
		if !containsArguments(operation.arguments, "xfrm", "policy", "add") {
			t.Fatalf("operation %d is not a policy add: %v", index+4, operation.arguments)
		}
	}

	clientReqID := argumentAfter(t, install[0].arguments, "reqid")
	if got := argumentAfter(t, install[1].arguments, "reqid"); got != clientReqID {
		t.Fatalf("client SA pair reqids = %q and %q", clientReqID, got)
	}
	serverReqID := argumentAfter(t, install[2].arguments, "reqid")
	if got := argumentAfter(t, install[3].arguments, "reqid"); got != serverReqID {
		t.Fatalf("server SA pair reqids = %q and %q", serverReqID, got)
	}
	if clientReqID == serverReqID {
		t.Fatalf("SA pair reqids both equal %q", clientReqID)
	}

	wantPolicies := map[string]bool{
		"tcp 40666 50600 out": false,
		"udp 40666 50600 out": false,
		"tcp 50600 40666 in":  false,
		"tcp 50601 55610 in":  false,
		"udp 50601 55610 in":  false,
		"tcp 55610 50601 out": false,
	}
	for _, operation := range install[4:] {
		key := strings.Join([]string{
			argumentAfter(t, operation.arguments, "proto"),
			argumentAfter(t, operation.arguments, "sport"),
			argumentAfter(t, operation.arguments, "dport"),
			argumentAfter(t, operation.arguments, "dir"),
		}, " ")
		if _, expected := wantPolicies[key]; !expected {
			t.Fatalf("unexpected policy %q: %v", key, operation.arguments)
		}
		wantPolicies[key] = true
	}
	for policy, found := range wantPolicies {
		if !found {
			t.Errorf("missing policy %q", policy)
		}
	}

	cleanup := buildXFRMCleanupPlan(config)
	if len(cleanup) != 10 {
		t.Fatalf("cleanup operation count = %d, want 10", len(cleanup))
	}
	keyHex := "0x" + strings.Repeat("11", 16)
	for _, operation := range cleanup {
		if strings.Contains(strings.Join(operation.arguments, " "), keyHex) {
			t.Fatalf("cleanup operation retained encryption key: %v", operation.arguments)
		}
	}
}

func TestValidateIPSecSAConfigRejectsDuplicateSPI(t *testing.T) {
	config := testIPSecSAConfig()
	config.PCSCFServerSPI = config.UEClientSPI
	if err := validateIPSecSAConfig(config); err == nil {
		t.Fatal("validateIPSecSAConfig() error = nil")
	}
}

func testIPSecSAConfig() IPSecSAConfig {
	return IPSecSAConfig{
		LocalIP:         net.ParseIP("10.0.0.2"),
		RemoteIP:        net.ParseIP("10.0.0.3"),
		UEClientSPI:     0x10000001,
		UEServerSPI:     0x10000002,
		PCSCFClientSPI:  0x20000001,
		PCSCFServerSPI:  0x20000002,
		UEClientPort:    40666,
		UEServerPort:    55610,
		PCSCFClientPort: 50601,
		PCSCFServerPort: 50600,
		EncryptionKey:   []byte(strings.Repeat("\x11", 16)),
		IntegrityKey:    []byte(strings.Repeat("\x22", 20)),
	}
}

func argumentAfter(t *testing.T, arguments []string, name string) string {
	t.Helper()
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	t.Fatalf("arguments %v omit %q", arguments, name)
	return ""
}

func containsArguments(arguments []string, sequence ...string) bool {
	if len(sequence) == 0 || len(sequence) > len(arguments) {
		return false
	}
	for start := 0; start+len(sequence) <= len(arguments); start++ {
		if reflect.DeepEqual(arguments[start:start+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func TestErrorsExposeAgreementSentinel(t *testing.T) {
	_, err := parseSecurityAgreement(nil, securityProposal{})
	if !errors.Is(err, ErrIPSecAgreementRequired) {
		t.Fatalf("error = %v, want ErrIPSecAgreementRequired", err)
	}
}
