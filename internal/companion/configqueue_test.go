package companion

import (
	"testing"
	"time"
)

func i8(v int8) *int8 { return &v }

func TestConfigQueueRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := NewConfigID()
	if len(id) != 16 {
		t.Fatalf("id len=%d", len(id))
	}
	req := ConfigRequest{
		ID:          id,
		RequestedAt: time.Now().UTC(),
		Region:      "US/Canada (recommended)",
		Radio:       &RadioParams{FreqKHz: 910525, BandwidthHz: 62500, SF: 7, CR: 5},
		TxPowerDbm:  i8(20),
	}
	if err := WriteConfigRequest(dir, req); err != nil {
		t.Fatal(err)
	}
	pending, err := ListPendingConfigRequests(dir)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	got, err := ReadConfigRequest(pending[0])
	if err != nil || got.Region != req.Region || got.Radio == nil || got.Radio.FreqKHz != 910525 {
		t.Fatalf("read req: %+v %v", got, err)
	}
	if got.TxPowerDbm == nil || *got.TxPowerDbm != 20 {
		t.Fatalf("txpower=%v", got.TxPowerDbm)
	}
	exists, err := ConfigRequestExists(dir, id)
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}

	res := ConfigResult{
		ID: id, RequestedAt: req.RequestedAt, CompletedAt: time.Now().UTC(),
		OK: true, Region: req.Region, Applied: req.Radio, TxPowerDbm: req.TxPowerDbm,
		SelfAfter: &SelfInfo{FreqKHz: 910525, BandwidthHz: 62500, SF: 7, CR: 5, TxPower: 20},
	}
	res.AddStep("set_radio_params", true, "910.525 MHz")
	if err := WriteConfigResult(dir, res); err != nil {
		t.Fatal(err)
	}
	if ex, _ := ConfigRequestExists(dir, id); ex {
		t.Fatal("request should be removed after result")
	}
	out, err := ReadConfigResult(dir, id)
	if err != nil || out == nil || !out.OK || out.SelfAfter == nil || out.SelfAfter.SF != 7 {
		t.Fatalf("result: %+v %v", out, err)
	}
	if len(out.Steps) != 1 || out.Steps[0].Name != "set_radio_params" {
		t.Fatalf("steps=%+v", out.Steps)
	}
}

func TestConfigRequestValidate(t *testing.T) {
	// nothing to apply
	if err := (ConfigRequest{ID: "aa"}).Validate(); err == nil {
		t.Fatal("expected error for empty request")
	}
	// bad radio
	bad := ConfigRequest{Radio: &RadioParams{FreqKHz: 1, BandwidthHz: 62500, SF: 7, CR: 5}}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected bad radio rejected")
	}
	// bad tx power
	badTx := ConfigRequest{TxPowerDbm: i8(99)}
	if err := badTx.Validate(); err == nil {
		t.Fatal("expected bad tx power rejected")
	}
	// good tx-power-only
	if err := (ConfigRequest{TxPowerDbm: i8(14)}).Validate(); err != nil {
		t.Fatalf("valid tx-only rejected: %v", err)
	}
	// WriteConfigRequest rejects invalid before writing
	if err := WriteConfigRequest(t.TempDir(), ConfigRequest{ID: NewConfigID()}); err == nil {
		t.Fatal("WriteConfigRequest should reject empty")
	}
}

func TestConfigQueueRejectsBadID(t *testing.T) {
	dir := t.TempDir()
	if err := WriteConfigRequest(dir, ConfigRequest{ID: "../etc", TxPowerDbm: i8(10)}); err == nil {
		t.Fatal("expected reject")
	}
	if _, err := ConfigRequestPath(dir, "not-hex!"); err == nil {
		t.Fatal("expected reject")
	}
}
