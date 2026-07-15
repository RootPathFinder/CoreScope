package main

import (
	"strings"
	"time"
)

// cdcHangupCooldown is how long to wait after a bare-EOF CDC hangup before
// issuing another secure command. Prevents the reconnect/re-login storm that
// leaves the ACM endpoint half-dead for subsequent handshakes (see poll logs:
// post-SENT EOF → immediate re-login → another EOF → handshake fails).
var cdcHangupCooldown = 3 * time.Second

// loginQueuedBeforeDrop reports whether a disconnect happened after the
// companion firmware queued the login RF packet (RESP_CODE_SENT arrived).
// In that case the login may have succeeded on-air; StatusOnly can recover
// without another ECDH/login that would just hang the CDC again.
func loginQueuedBeforeDrop(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "after RESP_CODE_SENT")
}

// sessionProbeMessage explains a CMD_HAS_CONNECTION result for logs.
//
// Modern repeaters send keep_alive=0 in RESP_SERVER_LOGIN_OK (legacy field),
// so the companion never calls startConnection and HasConnection is permanently
// false — that is NOT proof that login failed. Only a true result is decisive
// (legacy keep-alive repeaters).
func sessionProbeMessage(connected bool, ctx string) string {
	if connected {
		return ctx + ": keep-alive session present after reconnect (legacy repeater) — USB reply was lost; login succeeded device-side"
	}
	return ctx + ": no keep-alive session after reconnect (expected on modern repeaters — keep_alive=0; HasConnection is NOT proof of login failure). Trying StatusOnly next."
}
