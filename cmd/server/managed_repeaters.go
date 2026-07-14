package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/repeatervault"
)

// ManagedRepeatersListResponse is the GET /api/managed-repeaters payload.
type ManagedRepeatersListResponse struct {
	Repeaters []repeatervault.PublicView `json:"repeaters"`
	VaultPath string                     `json:"vaultPath,omitempty"`
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
	writeJSON(w, ManagedRepeatersListResponse{
		Repeaters: list,
		VaultPath: s.repeaterVault.Path(),
	})
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
	_ = json.NewEncoder(w).Encode(view)
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
	writeJSON(w, view)
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
