package main

import (
	"strings"
	"time"

	"github.com/meshcore-analyzer/companion"
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

// routeAction is what the poller should do with a managed repeater's companion contact path.
type routeAction int

const (
	// routeLeave keeps the companion's existing out_path (learned multi-hop or flood).
	routeLeave routeAction = iota
	// routeSeedFlood adds a missing contact with OUT_PATH_UNKNOWN so login can flood.
	routeSeedFlood
	// routeRestoreFlood rewrites a forced zero-hop path back to OUT_PATH_UNKNOWN.
	// Most managed repeaters are not RF-adjacent; zero-hop was a mistaken brownout
	// mitigation that made multi-hop logins unreachable.
	routeRestoreFlood
)

// chooseContactRoute decides path handling for a managed repeater.
//
// Policy (matches RemoteTerm / meshcore_py practice):
//   - missing contact → seed as flood (0xFF), never zero-hop
//   - known with learned hops (1–64) → leave alone
//   - known with flood (0xFF) → leave alone
//   - known with path_len=0 → restore flood (we previously forced this on every
//     unknown-path contact; most managed repeaters are not zero-hop reachable)
func chooseContactRoute(known bool, outPathLen int) routeAction {
	if !known {
		return routeSeedFlood
	}
	if outPathLen == int(companion.OutPathZeroHop) {
		return routeRestoreFlood
	}
	return routeLeave
}

func routeActionLabel(a routeAction) string {
	switch a {
	case routeSeedFlood:
		return "seed flood (out_path_len=255)"
	case routeRestoreFlood:
		return "restore flood path (was zero-hop; most managed repeaters are multi-hop)"
	default:
		return "leave existing path"
	}
}
