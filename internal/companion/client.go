package companion

import (
	"fmt"
	"sync"
	"time"
)

// Client talks companion protocol over a Port (USB serial).
type Client struct {
	port    Port
	fr      *FrameReader
	mu      sync.Mutex
	appName string
}

func NewClient(port Port, appName string) *Client {
	if appName == "" {
		appName = "corescope-poller"
	}
	return &Client{port: port, fr: NewFrameReader(port), appName: appName}
}

func (c *Client) Close() error {
	return c.port.Close()
}

// Handshake sends APP_START (+ optional DEVICE_QUERY) and drains self-info.
func (c *Client) Handshake(timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := WriteFrame(c.port, BuildAppStart(c.appName)); err != nil {
		return err
	}
	frame, err := ReadFrameWithDeadline(c.port, c.fr, timeout)
	if err != nil {
		return fmt.Errorf("app_start response: %w", err)
	}
	if len(frame) == 0 || (frame[0] != RespSelfInfo && frame[0] != RespOK && frame[0] != RespDeviceInfo) {
		// Some firmwares push device info asynchronously; tolerate OK/self/device.
		if len(frame) > 0 && frame[0] == RespError {
			return MapErrorCode(ParseErrorCode(frame))
		}
	}
	_ = WriteFrame(c.port, BuildDeviceQuery())
	// Best-effort drain one more frame (device info); ignore timeout.
	_, _ = ReadFrameWithDeadline(c.port, c.fr, 2*time.Second)
	return nil
}

// AppStartInfo sends CMD_APP_START and returns the parsed RESP_CODE_SELF_INFO.
// Unlike Handshake it strictly requires the self-info reply and returns the
// device's own public key / node name / radio params — proof the device answered.
func (c *Client) AppStartInfo(timeout time.Duration) (SelfInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := WriteFrame(c.port, BuildAppStart(c.appName)); err != nil {
		return SelfInfo{}, err
	}
	frame, err := c.awaitResp(RespSelfInfo, timeout)
	if err != nil {
		return SelfInfo{}, err
	}
	return ParseSelfInfo(frame)
}

// QueryDeviceInfo sends CMD_DEVICE_QUERY and returns RESP_CODE_DEVICE_INFO
// (firmware version, build date, manufacturer) straight off the wire.
func (c *Client) QueryDeviceInfo(timeout time.Duration) (DeviceInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := WriteFrame(c.port, BuildDeviceQuery()); err != nil {
		return DeviceInfo{}, err
	}
	frame, err := c.awaitResp(RespDeviceInfo, timeout)
	if err != nil {
		return DeviceInfo{}, err
	}
	return ParseDeviceInfo(frame)
}

// GetCoreStats sends CMD_GET_STATS (STATS_TYPE_CORE) and returns the device
// uptime + battery + error flags. Reading uptime before and after a suspected
// reset is the definitive way to prove the MCU rebooted vs. a CDC hiccup.
// Requires v8+ firmware; older devices reply RESP_CODE_ERR (unsupported).
func (c *Client) GetCoreStats(timeout time.Duration) (CoreStats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := WriteFrame(c.port, BuildGetCoreStats()); err != nil {
		return CoreStats{}, err
	}
	frame, err := c.awaitResp(RespStats, timeout)
	if err != nil {
		return CoreStats{}, err
	}
	return ParseCoreStats(frame)
}

// HasConnection sends CMD_HAS_CONNECTION and reports whether the companion
// holds an active login session to the contact. Returns (true,nil) on
// RESP_CODE_OK, (false,nil) when the firmware reports no session (RESP_CODE_ERR),
// and (false,err) only for a real serial disconnect/timeout. After a login whose
// serial reply was lost, this tells us the login actually succeeded device-side.
func (c *Client) HasConnection(pubKeyHex string, timeout time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	pk, err := DecodePubKey(pubKeyHex)
	if err != nil {
		return false, err
	}
	frame, err := BuildHasConnection(pk)
	if err != nil {
		return false, err
	}
	if err := WriteFrame(c.port, frame); err != nil {
		return false, err
	}
	if _, err := c.awaitResp(RespOK, timeout); err != nil {
		if IsDisconnected(err) {
			return false, err
		}
		// RESP_CODE_ERR (e.g. NOT_FOUND) or timeout → simply no active session.
		return false, nil
	}
	return true, nil
}

// GetBattStorage sends CMD_GET_BATT_AND_STORAGE and returns battery mV + storage.
func (c *Client) GetBattStorage(timeout time.Duration) (BattStorage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := WriteFrame(c.port, BuildGetBattStorage()); err != nil {
		return BattStorage{}, err
	}
	frame, err := c.awaitResp(RespBattStorage, timeout)
	if err != nil {
		return BattStorage{}, err
	}
	return ParseBattStorage(frame)
}

// SendSelfAdvert emits CMD_SEND_SELF_ADVERT and waits for RESP_CODE_OK.
// flood=false is a single zero-hop TX — the minimal RF transmit, used to test
// whether transmitting drops the USB link without logging into any repeater.
func (c *Client) SendSelfAdvert(flood bool, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	if err := WriteFrame(c.port, BuildSelfAdvert(flood)); err != nil {
		return err
	}
	_, err := c.awaitResp(RespOK, timeout)
	return err
}

// SelfAdvertAndProbe sends a zero-hop self-advert (an RF transmit) and then,
// after waiting `settle` for the transmit to physically complete, probes the
// device to confirm it did NOT reset.
//
// This is what makes the advert a *valid* RF-TX control. SendSelfAdvert alone
// returns on RESP_CODE_OK, which the firmware sends when it ACCEPTS/queues the
// command — before the transmit actually runs — so it cannot observe a
// TX-triggered reset. A login, by contrast, keeps the serial link open for the
// whole transmit+reply window (~2s), which is exactly when a brownout/firmware
// reset surfaces as EOF. Waiting out the airtime and re-probing removes that
// blind spot so "advert works" is proven, not assumed.
//
// Returns:
//   - alive=true  → device answered after the transmit (survived TX)
//   - alive=false → probe hit a disconnect/EOF (device reset during/after TX)
//   - err != nil  → the advert command itself failed (before the transmit ran)
//
// probeErr carries the probe result: a disconnect when alive=false, or a
// non-disconnect note (e.g. older firmware lacking DEVICE_QUERY) when alive=true.
func (c *Client) SelfAdvertAndProbe(flood bool, settle, timeout time.Duration) (alive bool, probeErr error, err error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := c.SendSelfAdvert(flood, timeout); err != nil {
		return false, nil, err
	}
	if settle > 0 {
		time.Sleep(settle)
	}
	// A lightweight query is enough to prove the CDC endpoint is still up.
	_, probeErr = c.QueryDeviceInfo(timeout)
	if probeErr == nil {
		return true, nil, nil
	}
	if IsDisconnected(probeErr) {
		return false, probeErr, nil
	}
	// Device answered with a non-disconnect error (e.g. unsupported cmd on older
	// firmware) — the link is alive, which is all this control needs to prove.
	return true, probeErr, nil
}

// SetRadioParams writes CMD_SET_RADIO_PARAMS (freq/bw/sf/cr) and waits for
// RESP_CODE_OK. The companion applies the change live (no reboot over serial).
func (c *Client) SetRadioParams(p RadioParams, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	frame, err := BuildSetRadioParams(p)
	if err != nil {
		return err
	}
	if err := WriteFrame(c.port, frame); err != nil {
		return err
	}
	_, err = c.awaitResp(RespOK, timeout)
	return err
}

// SetTxPower writes CMD_SET_RADIO_TX_POWER and waits for RESP_CODE_OK.
func (c *Client) SetTxPower(dbm int8, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	frame, err := BuildSetTxPower(dbm)
	if err != nil {
		return err
	}
	if err := WriteFrame(c.port, frame); err != nil {
		return err
	}
	_, err = c.awaitResp(RespOK, timeout)
	return err
}

// awaitResp reads frames until one matches want, mapping RESP_CODE_ERR to an
// error and skipping unsolicited pushes. Caller must hold c.mu.
func (c *Client) awaitResp(want byte, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame, err := ReadFrameWithDeadline(c.port, c.fr, time.Until(deadline))
		if err != nil {
			return nil, err
		}
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case want:
			return frame, nil
		case RespError:
			return nil, MapErrorCode(ParseErrorCode(frame))
		default:
			continue // ignore unsolicited pushes / stale replies
		}
	}
	return nil, ErrTimeout
}

// LoginAndStatus authenticates to a remote repeater then requests status.
func (c *Client) LoginAndStatus(pubKeyHex, password string, timeout time.Duration) (LoginPush, StatusPush, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var login LoginPush
	var status StatusPush

	pk, err := DecodePubKey(pubKeyHex)
	if err != nil {
		return login, status, err
	}
	loginFrame, err := BuildLogin(pk, password)
	if err != nil {
		return login, status, err
	}
	if err := WriteFrame(c.port, loginFrame); err != nil {
		return login, status, err
	}

	// Stage 1: RESP_CODE_SENT confirms the firmware built + queued the login
	// packet (ECDH/crypto done). Failing here = reset BEFORE any RF transmit.
	ack, err := c.waitSentOrErr(timeout)
	if err != nil {
		return login, status, fmt.Errorf("login: no RESP_CODE_SENT — companion reset before transmitting (login packet build, pre-TX): %w", err)
	}
	wait := ack.SuggestedTimeout
	if wait < 2*time.Second {
		wait = 2 * time.Second
	}
	if timeout > wait {
		wait = timeout
	}
	// Stage 2: reply arrives after the RF round-trip. Failing here = drop
	// during/after the actual transmit (packet already built OK).
	login, err = c.waitLoginPush(wait)
	if err != nil {
		return login, status, fmt.Errorf("login: dropped after RESP_CODE_SENT (%s TX queued, est %s) awaiting reply: %w", ackRouting(ack), ack.SuggestedTimeout, err)
	}
	if !login.OK {
		return login, status, ErrLoginFailed
	}

	status, err = c.statusReqLocked(pk, timeout)
	if err != nil {
		return login, status, err
	}
	return login, status, nil
}

// StatusOnly requests repeater status assuming the companion already holds an
// active login session (e.g. a prior login succeeded device-side but its serial
// reply was lost to a USB drop). Skips the login round-trip entirely.
func (c *Client) StatusOnly(pubKeyHex string, timeout time.Duration) (StatusPush, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pk, err := DecodePubKey(pubKeyHex)
	if err != nil {
		return StatusPush{}, err
	}
	return c.statusReqLocked(pk, timeout)
}

// statusReqLocked sends CMD_SEND_STATUS_REQ and awaits the status push.
// Caller must hold c.mu.
func (c *Client) statusReqLocked(pk []byte, timeout time.Duration) (StatusPush, error) {
	var status StatusPush
	statusFrame, err := BuildStatusReq(pk)
	if err != nil {
		return status, err
	}
	if err := WriteFrame(c.port, statusFrame); err != nil {
		return status, err
	}
	ack, err := c.waitSentOrErr(timeout)
	if err != nil {
		return status, fmt.Errorf("status_req: no RESP_CODE_SENT (pre-TX reset): %w", err)
	}
	wait := ack.SuggestedTimeout
	if wait < 2*time.Second {
		wait = 2 * time.Second
	}
	if timeout > wait {
		wait = timeout
	}
	status, err = c.waitStatusPush(wait)
	if err != nil {
		return status, fmt.Errorf("status_req: dropped after RESP_CODE_SENT (%s, est %s) awaiting reply: %w", ackRouting(ack), ack.SuggestedTimeout, err)
	}
	return status, nil
}

// ackRouting labels how the firmware queued the request (flood vs direct).
func ackRouting(a SentAck) string {
	if a.Flood {
		return "flood"
	}
	return "direct/zero-hop"
}

// AddOrUpdateContact seeds or updates a contact on the companion.
// outPathLen should be OutPathZeroHop for poller use, or OutPathUnknown for flood.
func (c *Client) AddOrUpdateContact(pubKeyHex string, advType uint8, name string, outPathLen uint8, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	pk, err := DecodePubKey(pubKeyHex)
	if err != nil {
		return err
	}
	frame, err := BuildAddUpdateContact(pk, advType, 0, outPathLen, name)
	if err != nil {
		return err
	}
	if err := WriteFrame(c.port, frame); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := ReadFrameWithDeadline(c.port, c.fr, time.Until(deadline))
		if err != nil {
			return err
		}
		if len(resp) == 0 {
			continue
		}
		switch resp[0] {
		case RespOK:
			return nil
		case RespError:
			return MapErrorCode(ParseErrorCode(resp))
		default:
			continue
		}
	}
	return ErrTimeout
}

// GetContacts sends CMD_GET_CONTACTS and drains START → CONTACT* → END.
// Contacts are streamed one-per-loop by companion firmware, so timeout should
// be generous (tens of seconds) when the contact book is large.
func (c *Client) GetContacts(timeout time.Duration) ([]Contact, uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if err := WriteFrame(c.port, BuildGetContacts()); err != nil {
		return nil, 0, err
	}

	deadline := time.Now().Add(timeout)
	var total uint32
	var gotStart bool
	out := make([]Contact, 0, 16)

	for time.Now().Before(deadline) {
		remain := time.Until(deadline)
		if remain < 50*time.Millisecond {
			break
		}
		frame, err := ReadFrameWithDeadline(c.port, c.fr, remain)
		if err != nil {
			if !gotStart {
				return nil, 0, fmt.Errorf("get_contacts start: %w", err)
			}
			return out, total, fmt.Errorf("get_contacts drain: %w (got %d/%d)", err, len(out), total)
		}
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case RespContactsStart:
			n, err := ParseContactsStart(frame)
			if err != nil {
				return nil, 0, err
			}
			total = n
			gotStart = true
			if cap(out) < int(n) && n < 512 {
				out = make([]Contact, 0, int(n))
			}
		case RespContact:
			ct, err := ParseContact(frame)
			if err != nil {
				continue // skip malformed; keep draining to END
			}
			out = append(out, ct)
		case RespEndOfContacts:
			return out, total, nil
		case RespError:
			return nil, 0, MapErrorCode(ParseErrorCode(frame))
		default:
			// Ignore unsolicited pushes while draining the contact list.
			continue
		}
	}
	if !gotStart {
		return nil, 0, ErrTimeout
	}
	return out, total, fmt.Errorf("%w: contact list incomplete (%d of ~%d)", ErrTimeout, len(out), total)
}

func (c *Client) waitSentOrErr(timeout time.Duration) (SentAck, error) {
	deadline := time.Now().Add(timeout)
	if timeout <= 0 {
		deadline = time.Now().Add(5 * time.Second)
	}
	for time.Now().Before(deadline) {
		remain := time.Until(deadline)
		frame, err := ReadFrameWithDeadline(c.port, c.fr, remain)
		if err != nil {
			return SentAck{}, err
		}
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case RespMsgSent:
			return ParseSentAck(frame)
		case RespError:
			return SentAck{}, MapErrorCode(ParseErrorCode(frame))
		default:
			// Ignore unsolicited pushes while waiting for SENT.
			continue
		}
	}
	return SentAck{}, ErrTimeout
}

func (c *Client) waitLoginPush(timeout time.Duration) (LoginPush, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame, err := ReadFrameWithDeadline(c.port, c.fr, time.Until(deadline))
		if err != nil {
			return LoginPush{}, err
		}
		if len(frame) == 0 {
			continue
		}
		switch frame[0] {
		case PushLoginSuccess, PushLoginFail:
			return ParseLoginPush(frame)
		default:
			continue
		}
	}
	return LoginPush{}, ErrTimeout
}

func (c *Client) waitStatusPush(timeout time.Duration) (StatusPush, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		frame, err := ReadFrameWithDeadline(c.port, c.fr, time.Until(deadline))
		if err != nil {
			return StatusPush{}, err
		}
		if len(frame) == 0 {
			continue
		}
		if frame[0] == PushStatusResponse {
			return ParseStatusPush(frame)
		}
	}
	return StatusPush{}, ErrTimeout
}
