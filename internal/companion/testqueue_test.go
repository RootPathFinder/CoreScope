package companion

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTestQueueRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := NewTestID()
	if len(id) != 16 {
		t.Fatalf("id len=%d", len(id))
	}
	req := TestRequest{ID: id, RequestedAt: time.Now().UTC(), Mode: "usb"}
	if err := WriteTestRequest(dir, req); err != nil {
		t.Fatal(err)
	}
	pending, err := ListPendingTestRequests(dir)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	got, err := ReadTestRequest(pending[0])
	if err != nil || got.ID != id || got.Mode != "usb" {
		t.Fatalf("read req: %+v %v", got, err)
	}
	exists, err := TestRequestExists(dir, id)
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	if r, err := ReadTestResult(dir, id); err != nil || r != nil {
		t.Fatalf("result before write: %v %v", r, err)
	}
	res := TestResult{
		ID: id, RequestedAt: req.RequestedAt, CompletedAt: time.Now().UTC(),
		OK: true, Mode: TestModeUSB, ContactCount: 4, DurationMs: 42,
		Port: "/dev/ttyACM1", Baud: 115200,
	}
	res.AddStep("open", true, "/dev/ttyACM1 @ 115200")
	res.AddStep("app_start", true, "node=\"Hilltop\"")
	res.AddStep("get_contacts", true, "4 contact(s)")
	if err := WriteTestResult(dir, res); err != nil {
		t.Fatal(err)
	}
	exists, _ = TestRequestExists(dir, id)
	if exists {
		t.Fatal("request should be removed after result")
	}
	out, err := ReadTestResult(dir, id)
	if err != nil || out == nil || !out.OK || out.ContactCount != 4 {
		t.Fatalf("result: %+v %v", out, err)
	}
	if len(out.Steps) != 3 || out.Steps[0].Name != "open" || !out.Steps[0].OK {
		t.Fatalf("steps: %+v", out.Steps)
	}
	if _, err := os.Stat(filepath.Join(TestQueueDir(dir), "result-"+id+".json")); err != nil {
		t.Fatal(err)
	}
}

func TestTestQueueRejectsBadID(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTestRequest(dir, TestRequest{ID: "../etc"}); err == nil {
		t.Fatal("expected reject")
	}
	if _, err := TestRequestPath(dir, "not-hex!"); err == nil {
		t.Fatal("expected reject")
	}
}

func TestNormalizeTestMode(t *testing.T) {
	cases := map[string]string{
		"":         TestModeUSB,
		"usb":      TestModeUSB,
		"bogus":    TestModeUSB,
		"advert":   TestModeAdvert,
		"ADVERT":   TestModeAdvert,
		" advert ": TestModeAdvert,
	}
	for in, want := range cases {
		if got := NormalizeTestMode(in); got != want {
			t.Fatalf("NormalizeTestMode(%q)=%q want %q", in, got, want)
		}
	}
}
