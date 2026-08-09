package server

import (
	"testing"

	"vocat/internal/modem"
	"vocat/internal/vowifi"
)

func TestParseCLCC(t *testing.T) {
	calls := parseCLCC(modem.Response{Lines: []string{
		`+CLCC: 1,1,4,0,0,"+447700900000",145`,
		`+CLCC: 2,0,0,0,0,"12345",129`,
	}})
	if len(calls) != 2 || calls[0]["number"] != "+447700900000" || calls[1]["state"] != 0 {
		t.Fatalf("parseCLCC = %#v", calls)
	}
}

func TestValidDialNumber(t *testing.T) {
	for _, value := range []string{"+447700900000", "12345", "*100#"} {
		if !validDialNumber(value) {
			t.Errorf("validDialNumber(%q) = false", value)
		}
	}
	for _, value := range []string{"", "+", "12;ATH", "12 34", "abc"} {
		if validDialNumber(value) {
			t.Errorf("validDialNumber(%q) = true", value)
		}
	}
}

func TestCallTransportRequiresIMSReady(t *testing.T) {
	controller := &fakeVoWiFiController{state: vowifi.State{Enabled: true}}
	server := &Server{vowifi: controller}
	if got := server.callTransport("ec20"); got != "cellular" {
		t.Fatalf("callTransport before IMS registration = %q, want cellular", got)
	}
	controller.state.IMSReady = true
	if got := server.callTransport("ec20"); got != "vowifi" {
		t.Fatalf("callTransport with IMS ready = %q, want vowifi", got)
	}
}
