package companion

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Companion protocol command / response codes (MeshCore companion radio).
const (
	CmdAppStart         byte = 0x01
	CmdGetContacts      byte = 0x04
	CmdSendSelfAdvert   byte = 0x07 // CMD_SEND_SELF_ADVERT (byte1: 1=flood, 0/absent=zero-hop)
	CmdAddUpdateContact byte = 0x09
	CmdSetRadioParams   byte = 0x0B // CMD_SET_RADIO_PARAMS (freq/bw/sf/cr)
	CmdSetRadioTxPower  byte = 0x0C // CMD_SET_RADIO_TX_POWER (int8 dBm)
	CmdGetBattStorage   byte = 0x14 // CMD_GET_BATT_AND_STORAGE
	CmdDeviceQuery      byte = 0x16
	CmdSendLogin        byte = 0x1A
	CmdSendStatusReq    byte = 0x1B
	CmdLogout           byte = 0x1D
	CmdSendTelemetry    byte = 0x27

	RespOK            byte = 0x00
	RespError         byte = 0x01
	RespContactsStart byte = 0x02
	RespContact       byte = 0x03
	RespEndOfContacts byte = 0x04
	RespSelfInfo      byte = 0x05
	RespMsgSent       byte = 0x06
	RespBattStorage   byte = 0x0C // RESP_CODE_BATT_AND_STORAGE
	RespDeviceInfo    byte = 0x0D

	PushLoginSuccess   byte = 0x85
	PushLoginFail      byte = 0x86
	PushStatusResponse byte = 0x87
	PushTelemetryResp  byte = 0x8B

	PubKeySize     = 32
	MaxPathSize    = 64
	MaxPasswordLen = 15
	// ContactFrameMin is the on-wire size of RESP_CODE_CONTACT (excl. framing).
	// code(1)+pubkey(32)+type+flags+pathLen(3)+path(64)+name(32)+timestamps/latlon(16)=148
	ContactFrameMin = 148

	AdvTypeNone     = 0
	AdvTypeChat     = 1
	AdvTypeRepeater = 2
	AdvTypeRoom     = 3
	AdvTypeSensor   = 4

	// OutPathUnknown is firmware OUT_PATH_UNKNOWN — login uses flood routing.
	OutPathUnknown = 0xFF
	// OutPathZeroHop is an empty direct path — login uses zero-hop TX (less energy than flood).
	OutPathZeroHop = 0
)

var (
	ErrNotFound       = errors.New("contact not found on companion")
	ErrLoginFailed    = errors.New("repeater login failed or timed out")
	ErrBadPubkey      = errors.New("public key must be 64 hex chars (32 bytes)")
	ErrPasswordLong   = errors.New("admin password exceeds 15-byte companion limit")
	ErrProtocol       = errors.New("unexpected companion response")
	ErrBadRadioParams = errors.New("invalid radio parameters")
	// ErrDisconnected means the USB CDC link dropped mid-command (often RF TX brownout).
	ErrDisconnected = errors.New("companion serial disconnected")
)

// IsDisconnected reports whether err is (or wraps) a serial disconnect / EOF.
func IsDisconnected(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrDisconnected) || errors.Is(err, io.EOF) {
		return true
	}
	// Some platforms surface "file already closed" / "input/output error" on yank.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "i/o error") ||
		strings.Contains(msg, "input/output error") ||
		strings.Contains(msg, "no such device") ||
		strings.Contains(msg, "device not configured")
}

// WrapSerialErr maps raw port errors to ErrDisconnected when appropriate.
func WrapSerialErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrTimeout) || errors.Is(err, ErrDisconnected) {
		return err
	}
	if IsDisconnected(err) {
		return fmt.Errorf("%w: %v", ErrDisconnected, err)
	}
	return err
}

// DecodePubKey parses a 64-char hex MeshCore public key.
func DecodePubKey(s string) ([]byte, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if len(s) != 64 {
		return nil, ErrBadPubkey
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != PubKeySize {
		return nil, ErrBadPubkey
	}
	return b, nil
}

// BuildAppStart builds CMD_APP_START with an optional app name.
func BuildAppStart(appName string) []byte {
	frame := make([]byte, 8, 8+len(appName))
	frame[0] = CmdAppStart
	if appName != "" {
		frame = append(frame, []byte(appName)...)
	}
	return frame
}

// BuildDeviceQuery builds CMD_DEVICE_QUERY.
func BuildDeviceQuery() []byte {
	return []byte{CmdDeviceQuery, 0x03}
}

// BuildGetContacts builds CMD_GET_CONTACTS (optional since filter omitted = full list).
func BuildGetContacts() []byte {
	return []byte{CmdGetContacts}
}

// BuildGetBattStorage builds CMD_GET_BATT_AND_STORAGE.
func BuildGetBattStorage() []byte {
	return []byte{CmdGetBattStorage}
}

// BuildSelfAdvert builds CMD_SEND_SELF_ADVERT.
//
// flood=false emits a zero-hop advert (single TX, no rebroadcast) — the minimal
// RF transmit for diagnosing whether TX brownouts the USB link. flood=true asks
// neighbours to rebroadcast (avoid for diagnostics; it spams the mesh).
func BuildSelfAdvert(flood bool) []byte {
	b := byte(0)
	if flood {
		b = 1
	}
	return []byte{CmdSendSelfAdvert, b}
}

// DeviceInfo is the parsed RESP_CODE_DEVICE_INFO (reply to CMD_DEVICE_QUERY).
// It proves the companion is a real MeshCore device speaking the protocol —
// the firmware version/build come straight off the wire, not assumed by us.
type DeviceInfo struct {
	FirmwareVerCode uint8  `json:"firmwareVerCode"`
	MaxContacts     int    `json:"maxContacts,omitempty"`
	MaxChannels     int    `json:"maxChannels,omitempty"`
	FirmwareBuild   string `json:"firmwareBuild,omitempty"`
	Manufacturer    string `json:"manufacturer,omitempty"`
	FirmwareVersion string `json:"firmwareVersion,omitempty"`
}

// ParseDeviceInfo parses RESP_CODE_DEVICE_INFO. Layout (companion firmware):
//
//	code + ver_code + max_contacts/2 + max_channels + ble_pin(4)
//	+ build_date(12) + manufacturer(40) + firmware_version(20) + ... (v9/v10 trailer)
func ParseDeviceInfo(frame []byte) (DeviceInfo, error) {
	var d DeviceInfo
	if len(frame) < 4 || frame[0] != RespDeviceInfo {
		return d, ErrProtocol
	}
	d.FirmwareVerCode = frame[1]
	d.MaxContacts = int(frame[2]) * 2
	d.MaxChannels = int(frame[3])
	if len(frame) >= 20 {
		d.FirmwareBuild = trimZ(frame[8:20])
	}
	if len(frame) >= 60 {
		d.Manufacturer = trimZ(frame[20:60])
	}
	if len(frame) >= 80 {
		d.FirmwareVersion = trimZ(frame[60:80])
	}
	return d, nil
}

// SelfInfo is the parsed RESP_CODE_SELF_INFO (reply to CMD_APP_START). It carries
// the node's own public key, name and radio params as reported by the device.
// Firmware emits freq as MHz×1000 (kHz) and bw as kHz×1000 (Hz) — see MyMesh.cpp.
type SelfInfo struct {
	AdvType     uint8  `json:"advType"`
	TxPower     uint8  `json:"txPower"`
	MaxTxPower  uint8  `json:"maxTxPower"`
	PublicKey   string `json:"publicKey"`
	FreqKHz     uint32 `json:"freqKHz,omitempty"`     // frequency in kHz (910525 = 910.525 MHz)
	BandwidthHz uint32 `json:"bandwidthHz,omitempty"` // bandwidth in Hz (250000 = 250 kHz)
	SF          uint8  `json:"sf,omitempty"`
	CR          uint8  `json:"cr,omitempty"`
	NodeName    string `json:"nodeName,omitempty"`
}

// ParseSelfInfo parses RESP_CODE_SELF_INFO. Layout (companion firmware):
//
//	code + adv_type + tx_power + max_tx_power + pubkey(32) + lat(4) + lon(4)
//	+ multi_acks + advert_loc_policy + telemetry + manual_add + freq(4) + bw(4)
//	+ sf + cr + node_name(rest)
func ParseSelfInfo(frame []byte) (SelfInfo, error) {
	var s SelfInfo
	if len(frame) < 4+PubKeySize || frame[0] != RespSelfInfo {
		return s, ErrProtocol
	}
	s.AdvType = frame[1]
	s.TxPower = frame[2]
	s.MaxTxPower = frame[3]
	s.PublicKey = hex.EncodeToString(frame[4 : 4+PubKeySize])
	// After pubkey(32): lat(4)+lon(4)+multi_acks+advert_loc_policy+telemetry+manual_add = 12
	radioOff := 4 + PubKeySize + 12
	if len(frame) >= radioOff+10 {
		s.FreqKHz = binary.LittleEndian.Uint32(frame[radioOff : radioOff+4])
		s.BandwidthHz = binary.LittleEndian.Uint32(frame[radioOff+4 : radioOff+8])
		s.SF = frame[radioOff+8]
		s.CR = frame[radioOff+9]
		if len(frame) > radioOff+10 {
			s.NodeName = trimZ(frame[radioOff+10:])
		}
	}
	return s, nil
}

// RadioParams are the LoRa radio settings written via CMD_SET_RADIO_PARAMS.
// Units match the companion wire format: freq in kHz, bandwidth in Hz.
type RadioParams struct {
	FreqKHz     uint32 `json:"freqKHz"`     // e.g. 910525 for 910.525 MHz
	BandwidthHz uint32 `json:"bandwidthHz"` // e.g. 62500 for 62.5 kHz
	SF          uint8  `json:"sf"`          // spreading factor 5–12
	CR          uint8  `json:"cr"`          // coding rate 5–8
}

// ValidateRadioParams enforces the same ranges the companion firmware checks in
// CMD_SET_RADIO_PARAMS, so we reject bad values before touching the radio.
func ValidateRadioParams(p RadioParams) error {
	if p.FreqKHz < 150000 || p.FreqKHz > 2500000 {
		return fmt.Errorf("%w: freq %d kHz out of range 150000–2500000", ErrBadRadioParams, p.FreqKHz)
	}
	if p.BandwidthHz < 7000 || p.BandwidthHz > 500000 {
		return fmt.Errorf("%w: bandwidth %d Hz out of range 7000–500000", ErrBadRadioParams, p.BandwidthHz)
	}
	if p.SF < 5 || p.SF > 12 {
		return fmt.Errorf("%w: sf %d out of range 5–12", ErrBadRadioParams, p.SF)
	}
	if p.CR < 5 || p.CR > 8 {
		return fmt.Errorf("%w: cr %d out of range 5–8", ErrBadRadioParams, p.CR)
	}
	return nil
}

// BuildSetRadioParams builds CMD_SET_RADIO_PARAMS. Layout (companion firmware):
//
//	cmd + freq(4, kHz) + bw(4, Hz) + sf(1) + cr(1)
//
// The optional trailing client_repeat byte is omitted (defaults to 0/off).
func BuildSetRadioParams(p RadioParams) ([]byte, error) {
	if err := ValidateRadioParams(p); err != nil {
		return nil, err
	}
	frame := make([]byte, 1+4+4+1+1)
	frame[0] = CmdSetRadioParams
	binary.LittleEndian.PutUint32(frame[1:5], p.FreqKHz)
	binary.LittleEndian.PutUint32(frame[5:9], p.BandwidthHz)
	frame[9] = p.SF
	frame[10] = p.CR
	return frame, nil
}

// BuildSetTxPower builds CMD_SET_RADIO_TX_POWER (signed dBm, -9..MAX).
func BuildSetTxPower(dbm int8) ([]byte, error) {
	if dbm < -9 || dbm > 30 {
		return nil, fmt.Errorf("%w: tx power %d dBm out of range -9..30", ErrBadRadioParams, dbm)
	}
	return []byte{CmdSetRadioTxPower, byte(dbm)}, nil
}

// BattStorage is the parsed RESP_CODE_BATT_AND_STORAGE.
type BattStorage struct {
	BatteryMv     uint16 `json:"batteryMv"`
	StorageUsedKb uint32 `json:"storageUsedKb,omitempty"`
	StorageTotKb  uint32 `json:"storageTotalKb,omitempty"`
}

// ParseBattStorage parses RESP_CODE_BATT_AND_STORAGE (batt_mv + used + total).
func ParseBattStorage(frame []byte) (BattStorage, error) {
	var b BattStorage
	if len(frame) < 3 || frame[0] != RespBattStorage {
		return b, ErrProtocol
	}
	b.BatteryMv = binary.LittleEndian.Uint16(frame[1:3])
	if len(frame) >= 11 {
		b.StorageUsedKb = binary.LittleEndian.Uint32(frame[3:7])
		b.StorageTotKb = binary.LittleEndian.Uint32(frame[7:11])
	}
	return b, nil
}

// trimZ trims trailing NULs and surrounding whitespace from a fixed byte field.
func trimZ(b []byte) string {
	return strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
}

// BuildAddUpdateContact builds CMD_ADD_UPDATE_CONTACT for a new/updated contact.
// Layout matches meshcore_py / companion firmware updateContactFromFrame:
//
//	cmd + pubkey(32) + type + flags + out_path_len + path(64) + name(32)
//	+ last_advert(4) + gps_lat(4) + gps_lon(4)
//
// Use OutPathZeroHop for poller-seeded contacts (direct RF, not flood).
// Use OutPathUnknown only when intentionally requesting flood routing.
// lastAdvert/latE6/lonE6 may be zero when seeding from the vault alone.
func BuildAddUpdateContact(pk []byte, advType, flags, outPathLen uint8, name string) ([]byte, error) {
	return BuildAddUpdateContactFull(pk, advType, flags, outPathLen, name, 0, 0, 0)
}

// BuildAddUpdateContactFull is BuildAddUpdateContact with advert timestamp / GPS.
func BuildAddUpdateContactFull(pk []byte, advType, flags, outPathLen uint8, name string, lastAdvert uint32, latE6, lonE6 int32) ([]byte, error) {
	if len(pk) != PubKeySize {
		return nil, ErrBadPubkey
	}
	if len(name) > 32 {
		name = name[:32]
	}
	// cmd(1)+pk(32)+type+flags+pathLen(3)+path(64)+name(32)+last_advert+lat+lon(12)
	const trail = 12
	frame := make([]byte, 1+PubKeySize+3+MaxPathSize+32+trail)
	i := 0
	frame[i] = CmdAddUpdateContact
	i++
	copy(frame[i:], pk)
	i += PubKeySize
	frame[i] = advType
	i++
	frame[i] = flags
	i++
	frame[i] = outPathLen
	i++
	i += MaxPathSize // zero out_path bytes
	copy(frame[i:], []byte(name))
	i += 32
	binary.LittleEndian.PutUint32(frame[i:], lastAdvert)
	i += 4
	binary.LittleEndian.PutUint32(frame[i:], uint32(latE6))
	i += 4
	binary.LittleEndian.PutUint32(frame[i:], uint32(lonE6))
	return frame, nil
}

// Contact is one companion contact from RESP_CODE_CONTACT.
type Contact struct {
	PublicKey  string `json:"publicKey"`
	Name       string `json:"name,omitempty"`
	Type       uint8  `json:"type"`
	TypeLabel  string `json:"typeLabel,omitempty"`
	Flags      uint8  `json:"flags,omitempty"`
	OutPathLen int    `json:"outPathLen"` // 0–64, or 255 = unknown path
	LastAdvert uint32 `json:"lastAdvert,omitempty"`
	LastMod    uint32 `json:"lastMod,omitempty"`
	LatE6      int32  `json:"latE6,omitempty"`
	LonE6      int32  `json:"lonE6,omitempty"`
}

// AdvTypeLabel maps ADV_TYPE_* to a short UI label.
func AdvTypeLabel(t uint8) string {
	switch t {
	case AdvTypeChat:
		return "chat"
	case AdvTypeRepeater:
		return "repeater"
	case AdvTypeRoom:
		return "room"
	case AdvTypeSensor:
		return "sensor"
	case AdvTypeNone:
		return "none"
	default:
		return fmt.Sprintf("type%d", t)
	}
}

// ParseContactsStart returns the total contact count from RESP_CODE_CONTACTS_START.
func ParseContactsStart(frame []byte) (uint32, error) {
	if len(frame) < 5 || frame[0] != RespContactsStart {
		return 0, ErrProtocol
	}
	return binary.LittleEndian.Uint32(frame[1:5]), nil
}

// ParseContact parses RESP_CODE_CONTACT into a Contact summary.
func ParseContact(frame []byte) (Contact, error) {
	var c Contact
	if len(frame) < ContactFrameMin || frame[0] != RespContact {
		return c, ErrProtocol
	}
	c.PublicKey = hex.EncodeToString(frame[1 : 1+PubKeySize])
	c.Type = frame[1+PubKeySize]
	c.Flags = frame[1+PubKeySize+1]
	c.OutPathLen = int(frame[1+PubKeySize+2])
	c.TypeLabel = AdvTypeLabel(c.Type)
	nameOff := 1 + PubKeySize + 3 + MaxPathSize
	nameBytes := frame[nameOff : nameOff+32]
	c.Name = strings.TrimRight(string(nameBytes), "\x00")
	tsOff := nameOff + 32
	c.LastAdvert = binary.LittleEndian.Uint32(frame[tsOff : tsOff+4])
	c.LatE6 = int32(binary.LittleEndian.Uint32(frame[tsOff+4 : tsOff+8]))
	c.LonE6 = int32(binary.LittleEndian.Uint32(frame[tsOff+8 : tsOff+12]))
	c.LastMod = binary.LittleEndian.Uint32(frame[tsOff+12 : tsOff+16])
	return c, nil
}

// NotFoundHint builds a verbose explanation when ERR_CODE_NOT_FOUND fires.
func NotFoundHint(pubKeyHex string, contacts []Contact) string {
	pk := strings.ToLower(strings.TrimSpace(pubKeyHex))
	shortPK := pk
	if len(shortPK) > 8 {
		shortPK = shortPK[:8]
	}
	for _, c := range contacts {
		if strings.EqualFold(c.PublicKey, pk) {
			path := "unknown path"
			if c.OutPathLen != 0xFF {
				path = fmt.Sprintf("out_path_len=%d", c.OutPathLen)
			}
			return fmt.Sprintf(
				"contact not found on companion for %s — unexpected: pubkey IS in companion contacts (%s, %s, %s). Retry or re-add contact.",
				shortPK, c.Name, c.TypeLabel, path)
		}
	}
	names := make([]string, 0, len(contacts))
	for _, c := range contacts {
		label := c.Name
		if label == "" {
			if len(c.PublicKey) >= 8 {
				label = c.PublicKey[:8]
			} else {
				label = c.PublicKey
			}
		}
		names = append(names, fmt.Sprintf("%s(%s)", label, c.TypeLabel))
	}
	list := "(none)"
	if len(names) > 0 {
		if len(names) > 12 {
			list = strings.Join(names[:12], ", ") + fmt.Sprintf(", … +%d more", len(names)-12)
		} else {
			list = strings.Join(names, ", ")
		}
	}
	return fmt.Sprintf(
		"contact not found on companion for %s — companion has %d contact(s) [%s]; pubkey not among them. MQTT/DB knowing the node is not enough: flood an advert toward the USB companion or add the contact in the MeshCore app.",
		shortPK, len(contacts), list)
}

// BuildLogin builds CMD_SEND_LOGIN.
func BuildLogin(pubKey []byte, password string) ([]byte, error) {
	if len(pubKey) != PubKeySize {
		return nil, ErrBadPubkey
	}
	if len(password) > MaxPasswordLen {
		return nil, ErrPasswordLong
	}
	frame := make([]byte, 1+PubKeySize, 1+PubKeySize+len(password))
	frame[0] = CmdSendLogin
	copy(frame[1:], pubKey)
	frame = append(frame, []byte(password)...)
	return frame, nil
}

// BuildStatusReq builds CMD_SEND_STATUS_REQ.
func BuildStatusReq(pubKey []byte) ([]byte, error) {
	if len(pubKey) != PubKeySize {
		return nil, ErrBadPubkey
	}
	frame := make([]byte, 1+PubKeySize)
	frame[0] = CmdSendStatusReq
	copy(frame[1:], pubKey)
	return frame, nil
}

// SentAck is RESP_CODE_SENT (0x06) for an outbound RF request.
type SentAck struct {
	Flood            bool
	ExpectedAckOrTag uint32
	SuggestedTimeout time.Duration
}

// ParseSentAck parses RESP_CODE_SENT.
func ParseSentAck(frame []byte) (SentAck, error) {
	var a SentAck
	if len(frame) < 10 || frame[0] != RespMsgSent {
		return a, ErrProtocol
	}
	a.Flood = frame[1] == 1
	a.ExpectedAckOrTag = binary.LittleEndian.Uint32(frame[2:6])
	ms := binary.LittleEndian.Uint32(frame[6:10])
	a.SuggestedTimeout = time.Duration(ms) * time.Millisecond
	return a, nil
}

// ParseErrorCode returns the ERR_CODE from RESP_CODE_ERR, or -1.
func ParseErrorCode(frame []byte) int {
	if len(frame) < 2 || frame[0] != RespError {
		return -1
	}
	return int(frame[1])
}

// LoginPush is PUSH_CODE_LOGIN_SUCCESS / FAIL summary.
type LoginPush struct {
	OK          bool
	IsAdmin     bool
	PubKeyPref  string
	Permissions byte
}

// ParseLoginPush parses login success/fail pushes.
func ParseLoginPush(frame []byte) (LoginPush, error) {
	var p LoginPush
	if len(frame) < 1 {
		return p, ErrProtocol
	}
	switch frame[0] {
	case PushLoginSuccess:
		p.OK = true
		if len(frame) >= 2 {
			p.Permissions = frame[1]
			p.IsAdmin = (frame[1] & 0x01) != 0
		}
		if len(frame) >= 8 {
			p.PubKeyPref = hex.EncodeToString(frame[2:8])
		}
		return p, nil
	case PushLoginFail:
		p.OK = false
		if len(frame) >= 8 {
			p.PubKeyPref = hex.EncodeToString(frame[2:8])
		}
		return p, nil
	default:
		return p, ErrProtocol
	}
}

// StatusPush wraps PUSH_CODE_STATUS_RESPONSE.
type StatusPush struct {
	PubKeyPref string
	Raw        []byte // status_data bytes after header
	Stats      RepeaterStats
}

// ParseStatusPush parses PUSH_CODE_STATUS_RESPONSE and nested RepeaterStats.
func ParseStatusPush(frame []byte) (StatusPush, error) {
	var s StatusPush
	if len(frame) < 8 || frame[0] != PushStatusResponse {
		return s, ErrProtocol
	}
	s.PubKeyPref = hex.EncodeToString(frame[2:8])
	s.Raw = append([]byte(nil), frame[8:]...)
	stats, err := ParseRepeaterStats(s.Raw)
	if err != nil {
		return s, err
	}
	s.Stats = stats
	return s, nil
}

// MapErrorCode turns companion ERR_CODE into a Go error.
func MapErrorCode(code int) error {
	switch code {
	case 2: // ERR_CODE_NOT_FOUND
		return ErrNotFound
	case -1:
		return ErrProtocol
	default:
		return fmt.Errorf("%w: err_code=%d", ErrProtocol, code)
	}
}
