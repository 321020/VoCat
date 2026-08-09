package server

import (
	"net/http"
	"strconv"
)

func (s *Server) handleHTTPSSettings(w http.ResponseWriter, r *http.Request) {
	if !s.developerEnabled {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if s.https == nil {
		writeError(w, http.StatusServiceUnavailable, "https_unavailable", "self-signed HTTPS is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"data": s.https.State(r.Host)})
	case http.MethodPut:
		var request struct {
			Enabled bool `json:"enabled"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		state, err := s.https.SetEnabled(r.Context(), request.Enabled)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "https_update_failed", err.Error())
			return
		}
		state = s.https.State(r.Host)
		s.recordAudit(r.Context(), "admin", "settings.https.update", "settings", "https", "success", map[bool]string{true: "enabled", false: "disabled"}[request.Enabled])
		writeJSON(w, http.StatusOK, map[string]any{"data": state})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (s *Server) handleHTTPSCertificate(w http.ResponseWriter, r *http.Request) {
	if !s.developerEnabled {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.https == nil {
		writeError(w, http.StatusServiceUnavailable, "https_unavailable", "self-signed HTTPS is unavailable")
		return
	}
	certificate, err := s.https.CertificatePEM()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "certificate_unavailable", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="vocat-selfsigned.crt"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(certificate)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(certificate)
}
