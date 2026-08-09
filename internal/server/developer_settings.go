package server

import (
	"net/http"

	"vocat/internal/developer"
)

func (s *Server) handleDeveloperSettings(w http.ResponseWriter, r *http.Request) {
	if !s.developerEnabled {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"device_limit":         developer.DeviceLimit(r.Context(), s.store, true),
			"default_device_limit": developer.DefaultDeviceLimit,
			"max_device_limit":     developer.MaxDeviceLimit,
		}})
	case http.MethodPut:
		var request struct {
			DeviceLimit int `json:"device_limit"`
		}
		if err := s.decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if err := developer.SetDeviceLimit(r.Context(), s.store, request.DeviceLimit); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_device_limit", err.Error())
			return
		}
		s.recordAudit(r.Context(), "admin", "settings.developer.device_limit", "settings", "developer", "success", "device limit updated")
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"device_limit":         request.DeviceLimit,
			"default_device_limit": developer.DefaultDeviceLimit,
			"max_device_limit":     developer.MaxDeviceLimit,
		}})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}
