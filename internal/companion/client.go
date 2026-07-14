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
