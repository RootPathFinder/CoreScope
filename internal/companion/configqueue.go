package companion

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Companion radio-config queue (marker files under data/companion-config-requests/).
//
// Same pattern as the USB self-test queue: the read-only server enqueues a
// request; companion-poller (the serial owner) applies CMD_SET_RADIO_PARAMS /
// CMD_SET_RADIO_TX_POWER, re-reads self-info to confirm, and writes a result.
// The server never touches the serial port.

const ConfigQueueDirName = "companion-config-requests"

// NewConfigID returns a random id for radio-config marker filenames.
func NewConfigID() string { return randHexID() }

// ConfigRequest is written by the server when the UI applies radio settings.
// Radio and TxPower are independently optional: apply either or both.
type ConfigRequest struct {
	ID          string       `json:"id"`
	RequestedAt time.Time    `json:"requestedAt"`
	Region      string       `json:"region,omitempty"` // preset label for logging/UI only
	Radio       *RadioParams `json:"radio,omitempty"`
	TxPowerDbm  *int8        `json:"txPowerDbm,omitempty"`
}

// ConfigResult is written by the poller after applying the config.
type ConfigResult struct {
	ID          string       `json:"id"`
	RequestedAt time.Time    `json:"requestedAt"`
	CompletedAt time.Time    `json:"completedAt"`
	OK          bool         `json:"ok"`
	Error       string       `json:"error,omitempty"`
	Region      string       `json:"region,omitempty"`
	Applied     *RadioParams `json:"applied,omitempty"`    // radio params sent (if any)
	TxPowerDbm  *int8        `json:"txPowerDbm,omitempty"` // tx power sent (if any)
	SelfAfter   *SelfInfo    `json:"selfAfter,omitempty"`  // self-info re-read after apply (proof)
	DurationMs  int64        `json:"durationMs,omitempty"`
	Steps       []DiagStep   `json:"steps,omitempty"`
}

// AddStep appends a step to the config result.
func (r *ConfigResult) AddStep(name string, ok bool, detail string) {
	r.Steps = append(r.Steps, DiagStep{Name: name, OK: ok, Detail: detail})
}

// Validate ensures a request carries at least one actionable change with valid values.
func (r ConfigRequest) Validate() error {
	if r.Radio == nil && r.TxPowerDbm == nil {
		return errors.New("config request has nothing to apply (radio and txPower both empty)")
	}
	if r.Radio != nil {
		if err := ValidateRadioParams(*r.Radio); err != nil {
			return err
		}
	}
	if r.TxPowerDbm != nil {
		if _, err := BuildSetTxPower(*r.TxPowerDbm); err != nil {
			return err
		}
	}
	return nil
}

// ConfigQueueDir returns data/companion-config-requests under configDir.
func ConfigQueueDir(configDir string) string {
	return filepath.Join(configDir, "data", ConfigQueueDirName)
}

func EnsureConfigQueueDir(configDir string) (string, error) {
	dir := ConfigQueueDir(configDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func ConfigRequestPath(configDir, id string) (string, error) {
	if !validTestID(id) {
		return "", errors.New("invalid companion config request id")
	}
	return filepath.Join(ConfigQueueDir(configDir), "request-"+id+".json"), nil
}

func ConfigResultPath(configDir, id string) (string, error) {
	if !validTestID(id) {
		return "", errors.New("invalid companion config request id")
	}
	return filepath.Join(ConfigQueueDir(configDir), "result-"+id+".json"), nil
}

func WriteConfigRequest(configDir string, req ConfigRequest) error {
	if !validTestID(req.ID) {
		return errors.New("invalid companion config request id")
	}
	if err := req.Validate(); err != nil {
		return err
	}
	if _, err := EnsureConfigQueueDir(configDir); err != nil {
		return err
	}
	p, _ := ConfigRequestPath(configDir, req.ID)
	return writeJSONAtomic(p, req)
}

func WriteConfigResult(configDir string, res ConfigResult) error {
	if !validTestID(res.ID) {
		return errors.New("invalid companion config request id")
	}
	if _, err := EnsureConfigQueueDir(configDir); err != nil {
		return err
	}
	p, _ := ConfigResultPath(configDir, res.ID)
	if err := writeJSONAtomic(p, res); err != nil {
		return err
	}
	reqPath, _ := ConfigRequestPath(configDir, res.ID)
	_ = os.Remove(reqPath)
	return nil
}

func ReadConfigResult(configDir, id string) (*ConfigResult, error) {
	p, err := ConfigResultPath(configDir, id)
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
	var r ConfigResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func ConfigRequestExists(configDir, id string) (bool, error) {
	p, err := ConfigRequestPath(configDir, id)
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

func ListPendingConfigRequests(configDir string) ([]string, error) {
	dir := ConfigQueueDir(configDir)
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

func ReadConfigRequest(path string) (*ConfigRequest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r ConfigRequest
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// writeJSONAtomic marshals v and writes it to path via a temp file + rename.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
