package main

import (
	"net/http"
	"time"

	"github.com/meshcore-analyzer/companion"
)

// CompanionUSBTestEnqueueResponse is returned by POST /api/companion/test (202).
type CompanionUSBTestEnqueueResponse struct {
	ID        string `json:"id"`
	StatusURL string `json:"statusUrl"`
	Mode      string `json:"mode"`
}

// CompanionUSBTestStatusResponse is returned by GET /api/companion/test/status.
type CompanionUSBTestStatusResponse struct {
	Status string                `json:"status"` // "pending" | "done"
	ID     string                `json:"id,omitempty"`
	Result *companion.TestResult `json:"result,omitempty"`
}

// handleCompanionUSBTest enqueues an on-demand USB companion self-test.
// The companion-poller (serial owner) runs the diagnostic sequence and writes a
// result marker. mode=usb (default) is read-only; mode=advert adds a zero-hop
// self-advert to test RF TX in isolation.
func (s *Server) handleCompanionUSBTest(w http.ResponseWriter, r *http.Request) {
	if s.configDir == "" {
		writeError(w, http.StatusServiceUnavailable, "config dir unavailable")
		return
	}
	mode := companion.NormalizeTestMode(r.URL.Query().Get("mode"))
	id := companion.NewTestID()
	req := companion.TestRequest{
		ID:          id,
		RequestedAt: time.Now().UTC(),
		Mode:        mode,
	}
	if err := companion.WriteTestRequest(s.configDir, req); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue companion USB test")
		return
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, CompanionUSBTestEnqueueResponse{
		ID:        id,
		StatusURL: "/api/companion/test/status?id=" + id,
		Mode:      mode,
	})
}

// handleCompanionUSBTestStatus reports pending / completed USB self-test result.
func (s *Server) handleCompanionUSBTestStatus(w http.ResponseWriter, r *http.Request) {
	if s.configDir == "" {
		writeError(w, http.StatusServiceUnavailable, "config dir unavailable")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	res, err := companion.ReadTestResult(s.configDir, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if res != nil {
		writeJSON(w, CompanionUSBTestStatusResponse{Status: "done", ID: id, Result: res})
		return
	}
	pending, err := companion.TestRequestExists(s.configDir, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if pending {
		writeJSON(w, CompanionUSBTestStatusResponse{Status: "pending", ID: id})
		return
	}
	writeError(w, http.StatusNotFound, "companion USB test not found")
}
