package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoginQueuedBeforeDrop(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unrelated", errors.New("handshake: app_start response: EOF"), false},
		{"pre-SENT", errors.New("login: no RESP_CODE_SENT — companion reset before transmitting: EOF"), false},
		{"post-SENT", errors.New("login: dropped after RESP_CODE_SENT (direct/zero-hop TX queued, est 2.334s) awaiting reply: EOF"), true},
		{"wrapped post-SENT", fmt.Errorf("poll: %w", errors.New("dropped after RESP_CODE_SENT awaiting reply: EOF")), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := loginQueuedBeforeDrop(tc.err); got != tc.want {
				t.Fatalf("loginQueuedBeforeDrop(%v)=%v want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestSessionProbeMessage_ModernRepeaterFalseNegative(t *testing.T) {
	// The bug we hit in production logs: treating HasConnection=false as
	// "login did not complete" caused an immediate re-login that hung CDC again.
	msg := sessionProbeMessage(false, "poll ab53eac0 (Uptop Schaghticoke)")
	if strings.Contains(msg, "login did not complete") {
		t.Fatalf("must not claim login failure on HasConnection=false: %s", msg)
	}
	if !strings.Contains(msg, "NOT proof of login failure") {
		t.Fatalf("expected explicit false-negative warning, got: %s", msg)
	}
	if !strings.Contains(msg, "StatusOnly") {
		t.Fatalf("expected StatusOnly recovery hint, got: %s", msg)
	}

	ok := sessionProbeMessage(true, "poll x")
	if !strings.Contains(ok, "login succeeded") {
		t.Fatalf("true session must affirm login success: %s", ok)
	}
}

func TestCDCHangupCooldownConfigured(t *testing.T) {
	// Guard against accidentally zeroing the cooldown (would reintroduce the
	// reconnect/re-login storm that kills later handshakes).
	if cdcHangupCooldown < time.Second {
		t.Fatalf("cdcHangupCooldown=%s; want >=1s to let the ACM endpoint settle", cdcHangupCooldown)
	}
}

func TestChooseContactRoute(t *testing.T) {
	cases := []struct {
		name       string
		known      bool
		outPathLen int
		want       routeAction
	}{
		{"missing → seed flood", false, 0, routeSeedFlood},
		{"known flood → leave", true, 0xFF, routeLeave},
		{"known 1-hop → leave", true, 1, routeLeave},
		{"known 3-hop → leave", true, 3, routeLeave},
		{"known zero-hop → restore flood", true, 0, routeRestoreFlood},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chooseContactRoute(tc.known, tc.outPathLen); got != tc.want {
				t.Fatalf("chooseContactRoute(%v,%d)=%v want %v (%s)",
					tc.known, tc.outPathLen, got, tc.want, routeActionLabel(tc.want))
			}
		})
	}
}
