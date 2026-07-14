package companion

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Companion USB self-test queue (marker files under data/companion-test-requests/).
//
// Same pattern as prunequeue: the read-only server enqueues; companion-poller
// (serial owner) runs OpenSerial → APP_START → GET_CONTACTS and writes a result.
// No RF login — this only checks that the USB companion is acting normally.

const TestQueueDirName = "companion-test-requests"

// Test modes selectable from the UI.
const (
	// TestModeUSB is read-only: open → app_start → device_query → battery →
	// get_contacts. No RF transmit. Proves the USB link + protocol work and
	// surfaces the device's own identity so nothing is assumed.
	TestModeUSB = "usb"
	// TestModeAdvert does everything TestModeUSB does, then emits a single
	// zero-hop self-advert (CMD_SEND_SELF_ADVERT) to test whether RF TX itself
	// drops the USB link — the exact failing path for managed logins.
	TestModeAdvert = "advert"
)

// NormalizeTestMode returns a valid mode, defaulting unknown values to usb.
func NormalizeTestMode(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case TestModeAdvert:
		return TestModeAdvert
	default:
		return TestModeUSB
	}
}

// TestRequest is written by the server when the UI clicks a Test USB button.
type TestRequest struct {
	ID          string    `json:"id"`
	RequestedAt time.Time `json:"requestedAt"`
	Mode        string    `json:"mode,omitempty"` // TestModeUSB (default) or TestModeAdvert
}

// DiagStep records one diagnostic step and the device's actual response, so the
// UI can prove the companion replied (not that the poller assumed success).
type DiagStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// TestResult is written by the poller after the self-test completes.
type TestResult struct {
	ID           string       `json:"id"`
	RequestedAt  time.Time    `json:"requestedAt"`
	CompletedAt  time.Time    `json:"completedAt"`
	Mode         string       `json:"mode,omitempty"`
	OK           bool         `json:"ok"`
	Error        string       `json:"error,omitempty"`
	Port         string       `json:"port,omitempty"`
	Baud         int          `json:"baud,omitempty"`
	ContactCount int          `json:"contactCount,omitempty"`
	DurationMs   int64        `json:"durationMs,omitempty"`
	Steps        []DiagStep   `json:"steps,omitempty"`
	Device       *DeviceInfo  `json:"device,omitempty"`  // from CMD_DEVICE_QUERY
	Self         *SelfInfo    `json:"self,omitempty"`    // from CMD_APP_START
	Battery      *BattStorage `json:"battery,omitempty"` // from CMD_GET_BATT_AND_STORAGE
	AdvertSent   bool         `json:"advertSent,omitempty"`
}

// AddStep appends a diagnostic step to the result.
func (r *TestResult) AddStep(name string, ok bool, detail string) {
	r.Steps = append(r.Steps, DiagStep{Name: name, OK: ok, Detail: detail})
}

// randHexID returns a 16-hex-char random id for marker filenames.
func randHexID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// NewTestID returns a random id for USB self-test marker filenames.
func NewTestID() string { return randHexID() }

// TestQueueDir returns data/companion-test-requests under configDir.
func TestQueueDir(configDir string) string {
	return filepath.Join(configDir, "data", TestQueueDirName)
}

func EnsureTestQueueDir(configDir string) (string, error) {
	dir := TestQueueDir(configDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func validTestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func TestRequestPath(configDir, id string) (string, error) {
	if !validTestID(id) {
		return "", errors.New("invalid companion test request id")
	}
	return filepath.Join(TestQueueDir(configDir), "request-"+id+".json"), nil
}

func TestResultPath(configDir, id string) (string, error) {
	if !validTestID(id) {
		return "", errors.New("invalid companion test request id")
	}
	return filepath.Join(TestQueueDir(configDir), "result-"+id+".json"), nil
}

func WriteTestRequest(configDir string, req TestRequest) error {
	if !validTestID(req.ID) {
		return errors.New("invalid companion test request id")
	}
	if _, err := EnsureTestQueueDir(configDir); err != nil {
		return err
	}
	p, _ := TestRequestPath(configDir, req.ID)
	return writeJSONAtomic(p, req)
}

func WriteTestResult(configDir string, res TestResult) error {
	if !validTestID(res.ID) {
		return errors.New("invalid companion test request id")
	}
	if _, err := EnsureTestQueueDir(configDir); err != nil {
		return err
	}
	p, _ := TestResultPath(configDir, res.ID)
	if err := writeJSONAtomic(p, res); err != nil {
		return err
	}
	reqPath, _ := TestRequestPath(configDir, res.ID)
	_ = os.Remove(reqPath)
	return nil
}

func ReadTestResult(configDir, id string) (*TestResult, error) {
	p, err := TestResultPath(configDir, id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var r TestResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func TestRequestExists(configDir, id string) (bool, error) {
	p, err := TestRequestPath(configDir, id)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func ListPendingTestRequests(configDir string) ([]string, error) {
	dir := TestQueueDir(configDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "request-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out, nil
}

func ReadTestRequest(path string) (*TestRequest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r TestRequest
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}
