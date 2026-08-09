package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeveloperOnlySettingsAreHiddenWhenModeIsOff(t *testing.T) {
	server := &Server{developerEnabled: false}
	for _, handler := range []func(http.ResponseWriter, *http.Request){
		server.handleDeveloperSettings,
		server.handleHTTPSSettings,
		server.handleHTTPSCertificate,
	} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/api/settings/developer", nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("developer-only endpoint status = %d, want 404", response.Code)
		}
	}
}
