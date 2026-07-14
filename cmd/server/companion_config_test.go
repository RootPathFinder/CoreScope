package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/meshcore-analyzer/companion"
)

func TestCompanionConfigEnqueueAndStatus(t *testing.T) {
	const apiKey = "companion-cfg-test-key-32chars!!!"
	_, router, dir := setupManagedRepeatersServer(t, apiKey)

	body := `{"region":"US/Canada (recommended)","radio":{"freqKHz":910525,"bandwidthHz":62500,"sf":7,"cr":5},"txPowerDbm":20}`
	req := httptest.NewRequest("POST", "/api/companion/config", strings.NewReader(body))
	req.Header.Set("X-API-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("enqueue code=%d body=%s", w.Code, w.Body.String())
	}
	var enq CompanionConfigEnqueueResponse
	if err := json.Unmarshal(w.Body.Bytes(), &enq); err != nil {
		t.Fatal(err)
	}
	if enq.ID == "" || enq.StatusURL == "" {
		t.Fatalf("enqueue payload: %+v", enq)
	}

	// marker landed with the right values
	got, err := companion.ReadConfigRequest(mustConfigReqPath(t, dir, enq.ID))
	if err != nil || got.Radio == nil || got.Radio.FreqKHz != 910525 || got.Radio.SF != 7 {
		t.Fatalf("request: %+v err=%v", got, err)
	}
	if got.TxPowerDbm == nil || *got.TxPowerDbm != 20 {
		t.Fatalf("txpower=%v", got.TxPowerDbm)
	}

	// pending
	stReq := httptest.NewRequest("GET", "/api/companion/config/status?id="+enq.ID, nil)
	stReq.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, stReq)
	var pending CompanionConfigStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" {
		t.Fatalf("want pending got %+v", pending)
	}

	// simulate poller writing a result
	res := companion.ConfigResult{
		ID: enq.ID, RequestedAt: time.Now().UTC(), CompletedAt: time.Now().UTC(),
		OK: true, Applied: got.Radio, TxPowerDbm: got.TxPowerDbm,
		SelfAfter: &companion.SelfInfo{FreqKHz: 910525, BandwidthHz: 62500, SF: 7, CR: 5, TxPower: 20},
	}
	if err := companion.WriteConfigResult(dir, res); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, stReq)
	var done CompanionConfigStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &done); err != nil {
		t.Fatal(err)
	}
	if done.Status != "done" || done.Result == nil || !done.Result.OK || done.Result.SelfAfter == nil || done.Result.SelfAfter.SF != 7 {
		t.Fatalf("done payload: %+v", done)
	}

	// invalid params rejected with 400
	badBody := `{"radio":{"freqKHz":1,"bandwidthHz":62500,"sf":7,"cr":5}}`
	bad := httptest.NewRequest("POST", "/api/companion/config", strings.NewReader(badBody))
	bad.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, bad)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad params code=%d body=%s", w.Code, w.Body.String())
	}

	// empty request rejected
	empty := httptest.NewRequest("POST", "/api/companion/config", strings.NewReader(`{}`))
	empty.Header.Set("X-API-Key", apiKey)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, empty)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty req code=%d", w.Code)
	}

	// auth required
	noAuth := httptest.NewRequest("POST", "/api/companion/config", strings.NewReader(body))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, noAuth)
	if w.Code == http.StatusAccepted {
		t.Fatal("expected auth failure")
	}
}

func mustConfigReqPath(t *testing.T, dir, id string) string {
	t.Helper()
	p, err := companion.ConfigRequestPath(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
