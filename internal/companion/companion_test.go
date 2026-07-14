package companion

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWriteAndReadFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte{CmdAppStart, 0, 0, 0, 0, 0, 0, 0, 'c', 's'}
	if err := WriteFrame(&buf, payload); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if raw[0] != frameInMarker {
		t.Fatalf("marker=%x", raw[0])
	}
	if binary.LittleEndian.Uint16(raw[1:3]) != uint16(len(payload)) {
		t.Fatalf("len mismatch")
	}

	// Simulate radio→app by rewriting marker.
	out := append([]byte{frameOutMarker}, raw[1:]...)
	fr := NewFrameReader(bytes.NewReader(out))
	got, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %x want %x", got, payload)
	}
}

func TestFrameReaderSkipsJunk(t *testing.T) {
	payload := []byte{RespSelfInfo, 1, 2, 3}
	var framed bytes.Buffer
	_ = WriteFrame(&framed, payload)
	raw := framed.Bytes()
	raw[0] = frameOutMarker
	stream := append([]byte("DEBUG: hello\r\n"), raw...)
	fr := NewFrameReader(bytes.NewReader(stream))
	got, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %x", got)
	}
}

func TestParseRepeaterStatsTagged(t *testing.T) {
	// 4-byte tag + 56-byte stats
	raw := make([]byte, 4+56)
	binary.LittleEndian.PutUint32(raw[0:4], 0x11223344)
	binary.LittleEndian.PutUint16(raw[4:6], 3710)       // battery
	binary.LittleEndian.PutUint16(raw[6:8], 2)          // queue
	binary.LittleEndian.PutUint16(raw[8:10], 0xFF9C)    // noise -100 as uint16 bits of int16
	binary.LittleEndian.PutUint16(raw[10:12], 0xFFCE)   // rssi -50
	binary.LittleEndian.PutUint32(raw[12:16], 100)      // recv
	binary.LittleEndian.PutUint32(raw[16:20], 50)       // sent
	binary.LittleEndian.PutUint32(raw[20:24], 10)       // airtime
	binary.LittleEndian.PutUint32(raw[24:28], 3600)     // uptime
	binary.LittleEndian.PutUint32(raw[28:32], 1)        // sent flood
	binary.LittleEndian.PutUint32(raw[32:36], 2)        // sent direct
	binary.LittleEndian.PutUint32(raw[36:40], 3)        // recv flood
	binary.LittleEndian.PutUint32(raw[40:44], 4)        // recv direct
	binary.LittleEndian.PutUint16(raw[44:46], 0)        // err events
	binary.LittleEndian.PutUint16(raw[46:48], 20)       // snr*4 = 5.0
	binary.LittleEndian.PutUint16(raw[48:50], 0)        // direct dups
	binary.LittleEndian.PutUint16(raw[50:52], 0)        // flood dups
	binary.LittleEndian.PutUint32(raw[52:56], 7)        // rx airtime
	binary.LittleEndian.PutUint32(raw[56:60], 9)        // recv errors

	stats, err := ParseRepeaterStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stats.BatteryMv != 3710 {
		t.Fatalf("battery=%d", stats.BatteryMv)
	}
	if stats.UptimeSecs != 3600 {
		t.Fatalf("uptime=%d", stats.UptimeSecs)
	}
	if stats.LastSNR != 5.0 {
		t.Fatalf("snr=%v", stats.LastSNR)
	}
	if stats.RecvErrors == nil || *stats.RecvErrors != 9 {
		t.Fatalf("recv_errors=%v", stats.RecvErrors)
	}
	if stats.NoiseFloor != -100 {
		t.Fatalf("noise=%d", stats.NoiseFloor)
	}
}

func TestBuildLoginRejectsLongPassword(t *testing.T) {
	pk := make([]byte, 32)
	_, err := BuildLogin(pk, "this-password-is-way-too-long")
	if err != ErrPasswordLong {
		t.Fatalf("err=%v", err)
	}
}

func TestParseStatusPush(t *testing.T) {
	statsBody := make([]byte, 4+52)
	binary.LittleEndian.PutUint16(statsBody[4:6], 4000)
	binary.LittleEndian.PutUint32(statsBody[24:28], 99)
	frame := make([]byte, 8+len(statsBody))
	frame[0] = PushStatusResponse
	copy(frame[2:8], []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})
	copy(frame[8:], statsBody)
	push, err := ParseStatusPush(frame)
	if err != nil {
		t.Fatal(err)
	}
	if push.PubKeyPref != "aabbccddeeff" {
		t.Fatalf("pref=%s", push.PubKeyPref)
	}
	if push.Stats.BatteryMv != 4000 || push.Stats.UptimeSecs != 99 {
		t.Fatalf("stats=%+v", push.Stats)
	}
}

// mockPort is a duplex pipe for Client tests.
type mockPort struct {
	mu   sync.Mutex
	in   bytes.Buffer // app reads from here (radio→app)
	out  bytes.Buffer // app writes here (app→radio)
	dead time.Time
}

func (m *mockPort) Read(p []byte) (int, error) {
	for {
		m.mu.Lock()
		dead := m.dead
		if !dead.IsZero() && time.Now().After(dead) {
			m.mu.Unlock()
			return 0, timeoutErr{}
		}
		if m.in.Len() > 0 {
			n, err := m.in.Read(p)
			m.mu.Unlock()
			return n, err
		}
		m.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
}

func (m *mockPort) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out.Write(p)
}

func (m *mockPort) Close() error { return nil }

func (m *mockPort) SetReadDeadline(t time.Time) error {
	m.mu.Lock()
	m.dead = t
	m.mu.Unlock()
	return nil
}

func (m *mockPort) pushRadioFrame(payload []byte) {
	var b bytes.Buffer
	_ = WriteFrame(&b, payload)
	raw := b.Bytes()
	raw[0] = frameOutMarker
	m.mu.Lock()
	m.in.Write(raw)
	m.mu.Unlock()
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// scriptedPort returns canned radio frames after each Write.
type scriptedPort struct {
	mu    sync.Mutex
	reads [][][]byte // each Write consumes one batch of response frames
	ri    int
	buf   bytes.Buffer
	dead  time.Time
}

func (s *scriptedPort) Read(p []byte) (int, error) {
	for {
		s.mu.Lock()
		if !s.dead.IsZero() && time.Now().After(s.dead) {
			s.mu.Unlock()
			return 0, timeoutErr{}
		}
		if s.buf.Len() > 0 {
			n, err := s.buf.Read(p)
			s.mu.Unlock()
			return n, err
		}
		s.mu.Unlock()
		time.Sleep(1 * time.Millisecond)
	}
}

func (s *scriptedPort) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ri < len(s.reads) {
		for _, frame := range s.reads[s.ri] {
			var b bytes.Buffer
			_ = WriteFrame(&b, frame)
			raw := b.Bytes()
			raw[0] = frameOutMarker
			s.buf.Write(raw)
		}
		s.ri++
	}
	return len(p), nil
}

func (s *scriptedPort) Close() error { return nil }
func (s *scriptedPort) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.dead = t
	s.mu.Unlock()
	return nil
}

func TestClientLoginAndStatus(t *testing.T) {
	pkHex := hex.EncodeToString(bytes.Repeat([]byte{0x11}, 32))
	pk, _ := DecodePubKey(pkHex)

	sent := make([]byte, 10)
	sent[0] = RespMsgSent
	binary.LittleEndian.PutUint32(sent[6:10], 100)

	ok := make([]byte, 8)
	ok[0] = PushLoginSuccess
	ok[1] = 0x01
	copy(ok[2:8], pk[:6])

	statsBody := make([]byte, 4+52)
	binary.LittleEndian.PutUint16(statsBody[4:6], 3650)
	binary.LittleEndian.PutUint32(statsBody[24:28], 42)
	st := make([]byte, 8+len(statsBody))
	st[0] = PushStatusResponse
	copy(st[2:8], pk[:6])
	copy(st[8:], statsBody)

	port := &scriptedPort{reads: [][][]byte{
		{sent, ok}, // after login write
		{sent, st}, // after status write
	}}
	c := NewClient(port, "test")

	login, status, err := c.LoginAndStatus(pkHex, "password", 2*time.Second)
	if err != nil {
		t.Fatalf("LoginAndStatus: %v", err)
	}
	if !login.OK || !login.IsAdmin {
		t.Fatalf("login=%+v", login)
	}
	if status.Stats.BatteryMv != 3650 || status.Stats.UptimeSecs != 42 {
		t.Fatalf("status=%+v", status.Stats)
	}
}

func TestParseContactAndNotFoundHint(t *testing.T) {
	pk := bytes.Repeat([]byte{0xab}, 32)
	frame := make([]byte, ContactFrameMin)
	frame[0] = RespContact
	copy(frame[1:], pk)
	frame[1+PubKeySize] = AdvTypeRepeater
	frame[1+PubKeySize+1] = 0
	frame[1+PubKeySize+2] = 0xFF // unknown path
	copy(frame[1+PubKeySize+3+MaxPathSize:], []byte("Hilltop\x00"))
	binary.LittleEndian.PutUint32(frame[ContactFrameMin-16:], 100)
	binary.LittleEndian.PutUint32(frame[ContactFrameMin-4:], 200)

	ct, err := ParseContact(frame)
	if err != nil {
		t.Fatal(err)
	}
	if ct.PublicKey != hex.EncodeToString(pk) || ct.Name != "Hilltop" || ct.TypeLabel != "repeater" {
		t.Fatalf("contact=%+v", ct)
	}
	if ct.OutPathLen != 0xFF || ct.LastMod != 200 {
		t.Fatalf("path/lastmod=%+v", ct)
	}

	start := make([]byte, 5)
	start[0] = RespContactsStart
	binary.LittleEndian.PutUint32(start[1:], 3)
	n, err := ParseContactsStart(start)
	if err != nil || n != 3 {
		t.Fatalf("start n=%d err=%v", n, err)
	}

	hintMiss := NotFoundHint(hex.EncodeToString(pk), nil)
	if !strings.Contains(hintMiss, "companion has 0 contact") || !strings.Contains(hintMiss, "flood an advert") {
		t.Fatalf("miss hint=%s", hintMiss)
	}
	hintHit := NotFoundHint(hex.EncodeToString(pk), []Contact{ct})
	if !strings.Contains(hintHit, "IS in companion contacts") {
		t.Fatalf("hit hint=%s", hintHit)
	}
	other := NotFoundHint(hex.EncodeToString(bytes.Repeat([]byte{0x11}, 32)), []Contact{ct})
	if !strings.Contains(other, "Hilltop(repeater)") || !strings.Contains(other, "not among them") {
		t.Fatalf("other hint=%s", other)
	}
}

func TestClientGetContacts(t *testing.T) {
	pk := bytes.Repeat([]byte{0x22}, 32)
	start := make([]byte, 5)
	start[0] = RespContactsStart
	binary.LittleEndian.PutUint32(start[1:], 1)
	contact := make([]byte, ContactFrameMin)
	contact[0] = RespContact
	copy(contact[1:], pk)
	contact[1+PubKeySize] = AdvTypeRoom
	copy(contact[1+PubKeySize+3+MaxPathSize:], []byte("RoomA"))
	end := []byte{RespEndOfContacts, 0, 0, 0, 0}

	port := &scriptedPort{reads: [][][]byte{
		{start, contact, end},
	}}
	c := NewClient(port, "test")
	list, total, err := c.GetContacts(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(list) != 1 || list[0].Name != "RoomA" || list[0].TypeLabel != "room" {
		t.Fatalf("list=%+v total=%d", list, total)
	}
}

func TestIsDisconnectedAndWrap(t *testing.T) {
	if !IsDisconnected(io.EOF) {
		t.Fatal("EOF should be disconnected")
	}
	if !IsDisconnected(fmt.Errorf("%w: eof", ErrDisconnected)) {
		t.Fatal("wrapped ErrDisconnected")
	}
	if IsDisconnected(ErrTimeout) {
		t.Fatal("timeout is not disconnect")
	}
	if IsDisconnected(nil) {
		t.Fatal("nil")
	}
	wrapped := WrapSerialErr(io.EOF)
	if !errors.Is(wrapped, ErrDisconnected) {
		t.Fatalf("wrap=%v", wrapped)
	}
	if WrapSerialErr(ErrTimeout) != ErrTimeout {
		t.Fatal("timeout passthrough")
	}
}

func TestBuildAddUpdateContact(t *testing.T) {
	pk := bytes.Repeat([]byte{0xFE}, 32)
	frame, err := BuildAddUpdateContact(pk, AdvTypeRepeater, 0, OutPathZeroHop, "Hilltop")
	if err != nil {
		t.Fatal(err)
	}
	wantLen := 1 + PubKeySize + 1 + 1 + 1 + MaxPathSize + 32
	if len(frame) != wantLen || frame[0] != CmdAddUpdateContact {
		t.Fatalf("len=%d code=%02x", len(frame), frame[0])
	}
	if frame[1+PubKeySize] != AdvTypeRepeater || frame[1+PubKeySize+2] != OutPathZeroHop {
		t.Fatalf("type/path=%02x %02x", frame[1+PubKeySize], frame[1+PubKeySize+2])
	}
	nameOff := 1 + PubKeySize + 3 + MaxPathSize
	if string(frame[nameOff:nameOff+7]) != "Hilltop" {
		t.Fatalf("name=%q", string(frame[nameOff:nameOff+32]))
	}
	flood, err := BuildAddUpdateContact(pk, AdvTypeRepeater, 0, OutPathUnknown, "Hilltop")
	if err != nil || flood[1+PubKeySize+2] != OutPathUnknown {
		t.Fatalf("flood path byte: %v", err)
	}
}

func TestClientAddOrUpdateContact(t *testing.T) {
	pkHex := hex.EncodeToString(bytes.Repeat([]byte{0xFE}, 32))
	port := &scriptedPort{reads: [][][]byte{
		{{RespOK}},
	}}
	c := NewClient(port, "test")
	if err := c.AddOrUpdateContact(pkHex, AdvTypeRepeater, "Hilltop", OutPathZeroHop, time.Second); err != nil {
		t.Fatal(err)
	}
}

var _ io.Reader = (*mockPort)(nil)
