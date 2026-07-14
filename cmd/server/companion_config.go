package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/meshcore-analyzer/companion"
)

// CompanionConfigRequestBody is the JSON body for POST /api/companion/config.
// Radio and txPowerDbm are independently optional — apply either or both.
type CompanionConfigRequestBody struct {
	Region     string                 `json:"region,omitempty"`
	Radio      *companion.RadioParams `json:"radio,omitempty"`
	TxPowerDbm *int8                  `json:"txPowerDbm,omitempty"`
}

// CompanionConfigEnqueueResponse is returned by POST /api/companion/config (202).
type CompanionConfigEnqueueResponse struct {
	ID        string `json:"id"`
	StatusURL string `json:"statusUrl"`
}

// CompanionConfigStatusResponse is returned by GET /api/companion/config/status.
type CompanionConfigStatusResponse struct {
	Status string                  `json:"status"` // "pending" | "done"
	ID     string                  `json:"id,omitempty"`
	Result *companion.ConfigResult `json:"result,omitempty"`
}

// handleCompanionConfig enqueues a radio-config change for the companion-poller
// (serial owner) to apply. The server only writes a marker file; it never opens
// the serial port (read/write separation invariant).
func (s *Server) handleCompanionConfig(w http.ResponseWriter, r *http.Request) {
	if s.configDir == "" {
		writeError(w, http.StatusServiceUnavailable, "config dir unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var body CompanionConfigRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	id := companion.NewConfigID()
	req := companion.ConfigRequest{
		ID:          id,
		RequestedAt: time.Now().UTC(),
		Region:      body.Region,
		Radio:       body.Radio,
		TxPowerDbm:  body.TxPowerDbm,
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := companion.WriteConfigRequest(s.configDir, req); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue companion config")
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, CompanionConfigEnqueueResponse{
		ID:        id,
		StatusURL: "/api/companion/config/status?id=" + id,
	})
}

// handleCompanionConfigStatus reports pending / completed radio-config result.
func (s *Server) handleCompanionConfigStatus(w http.ResponseWriter, r *http.Request) {
	if s.configDir == "" {
		writeError(w, http.StatusServiceUnavailable, "config dir unavailable")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	res, err := companion.ReadConfigResult(s.configDir, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if res != nil {
		writeJSON(w, CompanionConfigStatusResponse{Status: "done", ID: id, Result: res})
		return
	}
	pending, err := companion.ConfigRequestExists(s.configDir, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if pending {
		writeJSON(w, CompanionConfigStatusResponse{Status: "pending", ID: id})
		return
	}
	writeError(w, http.StatusNotFound, "companion config request not found")
}
