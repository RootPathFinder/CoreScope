package companion

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Companion protocol command / response codes (MeshCore companion radio).
const (
	CmdAppStart       byte = 0x01
	CmdDeviceQuery    byte = 0x16
	CmdSendLogin      byte = 0x1A
	CmdSendStatusReq  byte = 0x1B
	CmdLogout         byte = 0x1D
	CmdSendTelemetry  byte = 0x27

	RespOK         byte = 0x00
	RespError      byte = 0x01
	RespSelfInfo   byte = 0x05
	RespMsgSent    byte = 0x06
	RespDeviceInfo byte = 0x0D

	PushLoginSuccess   byte = 0x85
	PushLoginFail      byte = 0x86
	PushStatusResponse byte = 0x87
	PushTelemetryResp  byte = 0x8B

	PubKeySize     = 32
	MaxPasswordLen = 15
)

var (
	ErrNotFound     = errors.New("contact not found on companion")
	ErrLoginFailed  = errors.New("repeater login failed or timed out")
	ErrBadPubkey    = errors.New("public key must be 64 hex chars (32 bytes)")
	ErrPasswordLong = errors.New("admin password exceeds 15-byte companion limit")
	ErrProtocol     = errors.New("unexpected companion response")
)

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
