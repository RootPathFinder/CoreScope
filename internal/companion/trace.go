package companion

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"syscall"
	"time"
)

// TracePort wraps a Port and logs every read/write with a byte preview, elapsed
// timestamp, and precise error classification. It exists to answer "is the EOF a
// real USB/device fault or a CDC hangup?" without a logic analyzer: enable it for
// deep serial diagnostics on a single repeater.
type TracePort struct {
	inner Port
	label string
	logf  func(string, ...any)
	start time.Time
}

// NewTracePort wraps inner, logging each I/O op via logf (e.g. log.Printf).
func NewTracePort(inner Port, label string, logf func(string, ...any)) *TracePort {
	return &TracePort{inner: inner, label: label, logf: logf, start: time.Now()}
}

func (t *TracePort) Read(b []byte) (int, error) {
	n, err := t.inner.Read(b)
	// Skip the routine "no data yet" deadline timeouts to keep the trace readable;
	// always log real bytes and non-timeout errors (EOF/errno are the interesting bit).
	if n > 0 || (err != nil && !isTimeoutErr(err)) {
		t.logf("[serial %s +%dms] read n=%d err=%v%s%s", t.label, t.sinceMs(), n, errStr(err), ClassifyErr(err), hexPreview(b[:clamp(n, len(b))]))
	}
	return n, err
}

func (t *TracePort) Write(b []byte) (int, error) {
	n, err := t.inner.Write(b)
	t.logf("[serial %s +%dms] write n=%d err=%v hex=%s", t.label, t.sinceMs(), n, errStr(err), hex.EncodeToString(b[:clamp(len(b), 64)]))
	return n, err
}

func (t *TracePort) Close() error {
	t.logf("[serial %s +%dms] close", t.label, t.sinceMs())
	return t.inner.Close()
}

func (t *TracePort) SetReadDeadline(dl time.Time) error { return t.inner.SetReadDeadline(dl) }

func (t *TracePort) sinceMs() int64 { return time.Since(t.start).Milliseconds() }

// ClassifyErr distinguishes a bare EOF (0-byte read → CDC hangup, NOT a syscall
// error) from a real syscall errno (EIO/ENODEV → an actual device/USB fault).
// This is the decisive signal when dmesg shows no USB disconnect.
func ClassifyErr(err error) string {
	if err == nil {
		return ""
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return fmt.Sprintf(" [errno=%d (%s) — real device/USB fault]", int(errno), errno.Error())
	}
	if errors.Is(err, io.EOF) {
		return " [EOF: read() returned 0 bytes — CDC hangup, NOT a syscall/USB error; device stayed enumerated]"
	}
	return ""
}

func errStr(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func isTimeoutErr(err error) bool {
	if errors.Is(err, ErrTimeout) {
		return true
	}
	if te, ok := err.(interface{ Timeout() bool }); ok && te.Timeout() {
		return true
	}
	return errors.Is(err, ErrTimeout)
}

func hexPreview(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return " hex=" + hex.EncodeToString(b)
}

func clamp(n, max int) int {
	if n < 0 {
		return 0
	}
	if n > max {
		return max
	}
	return n
}
