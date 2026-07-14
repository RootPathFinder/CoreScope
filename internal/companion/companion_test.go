package companion

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
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

func TestDecodePubKey(t *testing.T) {
	if _, err := DecodePubKey("abcd"); err != ErrBadPubkey {
		t.Fatalf("short: %v", err)
	}
	_, err := DecodePubKey(hex.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
}

var _ io.Reader = (*mockPort)(nil)
