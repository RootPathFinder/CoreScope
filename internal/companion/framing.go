// Package companion implements a minimal MeshCore companion-radio client
// for USB serial (length-prefixed frames) used by the managed-repeater poller.
package companion

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	frameInMarker  = 0x3c // '<' app → radio
	frameOutMarker = 0x3e // '>' radio → app
	maxFrameSize   = 300
)

var (
	ErrFrameTooLarge = errors.New("companion frame too large")
	ErrBadFrame      = errors.New("companion frame framing error")
	ErrTimeout       = errors.New("companion frame timeout")
)

// Port is the serial I/O surface (real tty or mock).
type Port interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
	SetReadDeadline(t time.Time) error
}

// WriteFrame sends one companion protocol payload with USB CDC framing.
func WriteFrame(w io.Writer, payload []byte) error {
	if len(payload) > maxFrameSize {
		return ErrFrameTooLarge
	}
	pkt := make([]byte, 3+len(payload))
	pkt[0] = frameInMarker
	binary.LittleEndian.PutUint16(pkt[1:], uint16(len(payload)))
	copy(pkt[3:], payload)
	_, err := w.Write(pkt)
	return WrapSerialErr(err)
}

// FrameReader reassembles outbound (radio→app) frames from a byte stream.
type FrameReader struct {
	r   io.Reader
	buf []byte // read scratch
	acc []byte // accumulated stream bytes
}

func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{r: r, buf: make([]byte, 256)}
}

// ReadFrame blocks until one full frame payload is available (without the
// 0x3E + length header). Junk/debug bytes before the marker are skipped.
func (fr *FrameReader) ReadFrame() ([]byte, error) {
	for {
		if payload, ok := fr.tryExtract(); ok {
			return payload, nil
		}
		n, err := fr.r.Read(fr.buf)
		if n > 0 {
			fr.acc = append(fr.acc, fr.buf[:n]...)
			if err == nil || err == io.EOF {
				continue
			}
		}
		if err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				return nil, ErrTimeout
			}
			return nil, WrapSerialErr(err)
		}
	}
}

func (fr *FrameReader) tryExtract() ([]byte, bool) {
	for {
		// Find start marker.
		idx := -1
		for i, b := range fr.acc {
			if b == frameOutMarker {
				idx = i
				break
			}
		}
		if idx < 0 {
			fr.acc = fr.acc[:0]
			return nil, false
		}
		if idx > 0 {
			fr.acc = fr.acc[idx:]
		}
		if len(fr.acc) < 3 {
			return nil, false
		}
		need := int(binary.LittleEndian.Uint16(fr.acc[1:3]))
		if need > maxFrameSize {
			// Resync past bad marker.
			fr.acc = fr.acc[1:]
			continue
		}
		total := 3 + need
		if len(fr.acc) < total {
			return nil, false
		}
		payload := make([]byte, need)
		copy(payload, fr.acc[3:total])
		fr.acc = fr.acc[total:]
		return payload, true
	}
}

// ReadFrameWithDeadline reads one frame, applying a deadline on ports that support it.
func ReadFrameWithDeadline(p Port, fr *FrameReader, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := p.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}
	defer func() { _ = p.SetReadDeadline(time.Time{}) }()
	return fr.ReadFrame()
}
