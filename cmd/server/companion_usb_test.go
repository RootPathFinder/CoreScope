package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/meshcore-analyzer/companion"
)

func TestCompanionUSBTestEnqueueAndStatus(t *testing.T) {
	const apiKey = "companion-usb-test-key-32chars!!"
	_, router, dir := setupManagedRepeatersServer(t, apiKey)

	req := httptest.NewRequest("POST", "/api/companion/test", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("enqueue code=%d body=%s", w.Code, w.Body.String())
	}
	var enq CompanionUSBTestEnqueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &enq); err != nil {
		t.Fatal(err)
	}
	if enq.ID == "" || enq.Mode != "usb" || enq.StatusURL == "" {
		t.Fatalf("enqueue payload: %+v", enq)
	}
	exists, err := companion.TestRequestExists(dir, enq.ID)
	if err != nil || !exists {
		t.Fatalf("request marker missing: exists=%v err=%v", exists, err)
	}

	// Pending
	stReq := httptest.NewRequest("GET", "/api/companion/test/status?id="+enq.ID, nil)
	stReq.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, stReq)
	if w.Code != http.StatusOK {
		t.Fatalf("pending status code=%d body=%s", w.Code, w.Body.String())
	}
	var pending CompanionUSBTestStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" {
		t.Fatalf("want pending got %+v", pending)
	}

	// Simulate poller writing a result
	res := companion.TestResult{
		ID: enq.ID, RequestedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
		OK: true, Mode: companion.TestModeUSB, ContactCount: 3, DurationMs: 100,
		Port: "/dev/ttyACM1", Baud: 115200,
	}
	res.AddStep("open", true, "")
	res.AddStep("app_start", true, "node=\"X\"")
	res.AddStep("get_contacts", true, "3 contact(s)")
	if err := companion.WriteTestResult(dir, res); err != nil {
		t.Fatal(err)
	}

	w = httptest.NewRecorder()
	router.ServeHTTP(w, stReq)
	if w.Code != http.StatusOK {
		t.Fatalf("done status code=%d body=%s", w.Code, w.Body.String())
	}
	var done CompanionUSBTestStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != "done" || done.Result == nil || !done.Result.OK || done.Result.ContactCount != 3 {
		t.Fatalf("done payload: %+v", done)
	}

	// Bad id
	bad := httptest.NewRequest("GET", "/api/companion/test/status?id=../etc", nil)
	bad.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, bad)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad id code=%d", w.Code)
	}

	// Unknown id
	unk := httptest.NewRequest("GET", "/api/companion/test/status?id=deadbeefdeadbeef", nil)
	unk.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, unk)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown id code=%d", w.Code)
	}

	// Auth required
	noAuth := httptest.NewRequest("POST", "/api/companion/test", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, noAuth)
	if w.Code == http.StatusAccepted {
		t.Fatal("expected auth failure")
	}

	// Marker landed under data/
	entries, _ := os.ReadDir(filepath.Join(dir, "data", companion.TestQueueDirName))
	if len(entries) == 0 {
		t.Fatal("expected result file in queue dir")
	}
}

func TestCompanionUSBTestAdvertMode(t *testing.T) {
	const apiKey = "companion-usb-test-key-32chars!!"
	_, router, dir := setupManagedRepeatersServer(t, apiKey)

	req := httptest.NewRequest("POST", "/api/companion/test?mode=advert", nil)
	req.Header.Set("X-API-Key", apiKey)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("enqueue code=%d body=%s", w.Code, w.Body.String())
	}
	var enq CompanionUSBTestEnqueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &enq); err != nil {
		t.Fatal(err)
	}
	if enq.Mode != companion.TestModeAdvert {
		t.Fatalf("mode=%q want advert", enq.Mode)
	}
	got, err := companion.ReadTestRequest(filepath.Join(companion.TestQueueDir(dir), "request-"+enq.ID+".json"))
	if err != nil || got.Mode != companion.TestModeAdvert {
		t.Fatalf("request mode=%+v err=%v", got, err)
	}
}
