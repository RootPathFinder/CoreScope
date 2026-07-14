package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/meshcore-analyzer/companion"
)

func setupManagedRepeatersServer(t *testing.T, apiKey string) (*Server, *mux.Router, string) {
	t.Helper()
	dir := t.TempDir()
	cfgJSON := `{"port":3000,"apiKey":"` + apiKey + `"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Port: 3000, APIKey: apiKey}
	srv := NewServer(nil, cfg, NewHub())
	srv.configDir = dir
	srv.initRepeaterVault()
	if srv.repeaterVault == nil {
		t.Fatal("expected repeater vault to initialize")
	}
	router := mux.NewRouter()
	srv.RegisterRoutes(router)
	return srv, router, dir
}

func TestManagedRepeatersCRUD(t *testing.T) {
	const apiKey = "a-strong-api-key-for-testing"
	_, router, _ := setupManagedRepeatersServer(t, apiKey)
	pk := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"

	createBody := `{"publicKey":"` + pk + `","name":"Hilltop","adminPassword":"s3cret"}`
	req := httptest.NewRequest("POST", "/api/managed-repeaters", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: code=%d body=%s", w.Code, w.Body.String())
	}
	var created struct {
		ID               string `json:"id"`
		PublicKey        string `json:"publicKey"`
		Name             string `json:"name"`
		HasAdminPassword bool   `json:"hasAdminPassword"`
		AdminPassword    string `json:"adminPassword"`
		AdminPasswordEnc string `json:"adminPasswordEnc"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.PublicKey != pk || !created.HasAdminPassword {
		t.Fatalf("unexpected create payload: %+v", created)
	}
	if created.AdminPassword != "" || created.AdminPasswordEnc != "" {
		t.Fatalf("password leaked in create response: %+v", created)
	}

	// List
	req = httptest.NewRequest("GET", "/api/managed-repeaters", nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: code=%d body=%s", w.Code, w.Body.String())
	}
	var list ManagedRepeatersListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Repeaters) != 1 {
		t.Fatalf("list len=%d", len(list.Repeaters))
	}
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatal("plaintext password leaked in list response")
	}

	// Update name only
	upd := `{"name":"Renamed","adminPassword":""}`
	req = httptest.NewRequest("PUT", "/api/managed-repeaters/"+created.ID, strings.NewReader(upd))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("update: code=%d body=%s", w.Code, w.Body.String())
	}

	// Delete
	req = httptest.NewRequest("DELETE", "/api/managed-repeaters/"+created.ID, nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestManagedRepeatersListIncludesPollStatus(t *testing.T) {
	const apiKey = "a-strong-api-key-for-testing"
	_, router, dir := setupManagedRepeatersServer(t, apiKey)
	pk := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"

	createBody := `{"publicKey":"` + pk + `","name":"Hilltop","adminPassword":"s3cret"}`
	req := httptest.NewRequest("POST", "/api/managed-repeaters", strings.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}

	store := companion.OpenStatusStore(dir)
	bat := companion.RepeaterStats{BatteryMv: 3810, UptimeSecs: 120}
	if err := store.Upsert(companion.CompanionInfo{Port: "/dev/ttyACM1", Baud: 115200, OK: true}, companion.PollSnapshot{
		PublicKey: pk,
		Name:      "Hilltop",
		PolledAt:  time.Now().UTC(),
		OK:        true,
		IsAdmin:   true,
		Stats:     &bat,
	}); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest("GET", "/api/managed-repeaters", nil)
	req.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var list ManagedRepeatersListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Companion == nil || !list.Companion.OK || list.Companion.Port != "/dev/ttyACM1" {
		t.Fatalf("companion=%+v", list.Companion)
	}
	if len(list.Repeaters) != 1 || list.Repeaters[0].Poll == nil || !list.Repeaters[0].Poll.OK {
		t.Fatalf("repeaters=%+v", list.Repeaters)
	}
	if list.Repeaters[0].Poll.Stats == nil || list.Repeaters[0].Poll.Stats.BatteryMv != 3810 {
		t.Fatalf("stats=%+v", list.Repeaters[0].Poll.Stats)
	}
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Fatal("password leaked")
	}
}

func TestManagedRepeatersRequiresAPIKey(t *testing.T) {
	const apiKey = "a-strong-api-key-for-testing"
	_, router, _ := setupManagedRepeatersServer(t, apiKey)
	req := httptest.NewRequest("GET", "/api/managed-repeaters", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without key, got %d", w.Code)
	}
}

func TestManagedRepeatersRejectsInvalidPubkey(t *testing.T) {
	const apiKey = "a-strong-api-key-for-testing"
	_, router, _ := setupManagedRepeatersServer(t, apiKey)
	body := bytes.NewBufferString(`{"publicKey":"nope","adminPassword":"x"}`)
	req := httptest.NewRequest("POST", "/api/managed-repeaters", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}
