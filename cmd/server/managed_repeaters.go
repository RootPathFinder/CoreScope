package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/companion"
	"github.com/meshcore-analyzer/repeatervault"
)

// ManagedRepeaterView is the API projection for one vault entry + optional poll.
type ManagedRepeaterView struct {
	ID               string                  `json:"id"`
	PublicKey        string                  `json:"publicKey"`
	Name             string                  `json:"name,omitempty"`
	HasAdminPassword bool                    `json:"hasAdminPassword"`
	CreatedAt        string                  `json:"createdAt"`
	UpdatedAt        string                  `json:"updatedAt"`
	Poll             *ManagedRepeaterPollView `json:"poll,omitempty"`
}

// ManagedRepeaterPollView is the latest companion-poller snapshot (no secrets).
type ManagedRepeaterPollView struct {
	PolledAt   string                  `json:"polledAt,omitempty"`
	OK         bool                    `json:"ok"`
	Error      string                  `json:"error,omitempty"`
	IsAdmin    bool                    `json:"isAdmin,omitempty"`
	DurationMs int64                   `json:"durationMs,omitempty"`
	Stats      *companion.RepeaterStats `json:"stats,omitempty"`
}

// ManagedRepeatersListResponse is the GET /api/managed-repeaters payload.
type ManagedRepeatersListResponse struct {
	Repeaters []ManagedRepeaterView      `json:"repeaters"`
	VaultPath string                     `json:"vaultPath,omitempty"`
	Companion *companion.CompanionInfo   `json:"companion,omitempty"`
	StatusAt  string                     `json:"statusUpdatedAt,omitempty"`
}

// ManagedRepeaterWriteRequest is the POST/PUT body.
type ManagedRepeaterWriteRequest struct {
	PublicKey     string `json:"publicKey"`
	Name          string `json:"name"`
	AdminPassword string `json:"adminPassword"`
}

func (s *Server) initRepeaterVault() {
	if s.cfg == nil || s.configDir == "" {
		return
	}
	key, err := repeatervault.DeriveKey(os.Getenv("CORESCOPE_VAULT_KEY"), s.cfg.APIKey)
	if err != nil {
		return
	}
	store, err := repeatervault.Open(s.configDir, key)
	if err != nil {
		log.Printf("managed-repeaters vault open failed: %v", err)
		return
	}
	s.repeaterVault = store
	log.Printf("managed-repeaters vault ready at %s", store.Path())
}

func (s *Server) handleListManagedRepeaters(w http.ResponseWriter, r *http.Request) {
	if s.repeaterVault == nil {
		writeError(w, http.StatusServiceUnavailable, "managed repeater vault unavailable — set apiKey or CORESCOPE_VAULT_KEY")
		return
	}
	list, err := s.repeaterVault.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list managed repeaters")
		return
	}
	statusDoc, _ := companion.OpenStatusStore(s.configDir).Load()
	out := make([]ManagedRepeaterView, 0, len(list))
	for _, v := range list {
		row := ManagedRepeaterView{
			ID:               v.ID,
			PublicKey:        v.PublicKey,
			Name:             v.Name,
			HasAdminPassword: v.HasAdminPassword,
			CreatedAt:        v.CreatedAt,
			UpdatedAt:        v.UpdatedAt,
		}
		if snap, ok := statusDoc.Repeaters[v.PublicKey]; ok {
			row.Poll = pollViewFromSnapshot(snap)
		}
		out = append(out, row)
	}
	resp := ManagedRepeatersListResponse{
		Repeaters: out,
		VaultPath: s.repeaterVault.Path(),
	}
	if !statusDoc.UpdatedAt.IsZero() {
		resp.StatusAt = statusDoc.UpdatedAt.UTC().Format(timeRFC3339)
		c := statusDoc.Companion
		resp.Companion = &c
	}
	writeJSON(w, resp)
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"

func pollViewFromSnapshot(snap companion.PollSnapshot) *ManagedRepeaterPollView {
	pv := &ManagedRepeaterPollView{
		OK:         snap.OK,
		Error:      snap.Error,
		IsAdmin:    snap.IsAdmin,
		DurationMs: snap.DurationMs,
		Stats:      snap.Stats,
	}
	if !snap.PolledAt.IsZero() {
		pv.PolledAt = snap.PolledAt.UTC().Format(timeRFC3339)
	}
	return pv
}

func (s *Server) handleCreateManagedRepeater(w http.ResponseWriter, r *http.Request) {
	if s.repeaterVault == nil {
		writeError(w, http.StatusServiceUnavailable, "managed repeater vault unavailable — set apiKey or CORESCOPE_VAULT_KEY")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body ManagedRepeaterWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	view, err := s.repeaterVault.Add(body.PublicKey, body.Name, body.AdminPassword)
	if err != nil {
		writeManagedRepeaterErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toManagedView(view, nil))
}

func (s *Server) handleUpdateManagedRepeater(w http.ResponseWriter, r *http.Request) {
	if s.repeaterVault == nil {
		writeError(w, http.StatusServiceUnavailable, "managed repeater vault unavailable — set apiKey or CORESCOPE_VAULT_KEY")
		return
	}
	id := mux.Vars(r)["id"]
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body ManagedRepeaterWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	setPassword := strings.TrimSpace(body.AdminPassword) != ""
	view, err := s.repeaterVault.Update(id, body.Name, body.AdminPassword, true, setPassword)
	if err != nil {
		writeManagedRepeaterErr(w, err)
		return
	}
	writeJSON(w, toManagedView(view, nil))
}

func (s *Server) handleDeleteManagedRepeater(w http.ResponseWriter, r *http.Request) {
	if s.repeaterVault == nil {
		writeError(w, http.StatusServiceUnavailable, "managed repeater vault unavailable — set apiKey or CORESCOPE_VAULT_KEY")
		return
	}
	id := mux.Vars(r)["id"]
	if err := s.repeaterVault.Delete(id); err != nil {
		writeManagedRepeaterErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func toManagedView(v repeatervault.PublicView, poll *ManagedRepeaterPollView) ManagedRepeaterView {
	return ManagedRepeaterView{
		ID:               v.ID,
		PublicKey:        v.PublicKey,
		Name:             v.Name,
		HasAdminPassword: v.HasAdminPassword,
		CreatedAt:        v.CreatedAt,
		UpdatedAt:        v.UpdatedAt,
		Poll:             poll,
	}
}

func writeManagedRepeaterErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repeatervault.ErrInvalidKey):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, repeatervault.ErrInvalidName):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, repeatervault.ErrInvalidPass):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, repeatervault.ErrDuplicateKey):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, repeatervault.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "managed repeater vault error")
	}
}
