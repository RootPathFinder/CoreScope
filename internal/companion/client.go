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

	ack, err := c.waitSentOrErr(timeout)
	if err != nil {
		return login, status, err
	}
	wait := ack.SuggestedTimeout
	if wait < 2*time.Second {
		wait = 2 * time.Second
	}
	if timeout > wait {
		wait = timeout
	}
	login, err = c.waitLoginPush(wait)
	if err != nil {
		return login, status, err
	}
	if !login.OK {
		return login, status, ErrLoginFailed
	}

	statusFrame, err := BuildStatusReq(pk)
	if err != nil {
		return login, status, err
	}
	if err := WriteFrame(c.port, statusFrame); err != nil {
		return login, status, err
	}
	ack, err = c.waitSentOrErr(timeout)
	if err != nil {
		return login, status, err
	}
	wait = ack.SuggestedTimeout
	if wait < 2*time.Second {
		wait = 2 * time.Second
	}
	if timeout > wait {
		wait = timeout
	}
	status, err = c.waitStatusPush(wait)
	return login, status, err
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
