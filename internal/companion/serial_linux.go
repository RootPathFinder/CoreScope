package companion

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// OpenSerial opens a USB CDC ACM device at the given baud (default 115200).
func OpenSerial(path string, baud int) (Port, error) {
	if baud <= 0 {
		baud = 115200
	}
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open serial %s: %w", path, err)
	}
	fd := int(f.Fd())
	if err := configureTTY(fd, baud); err != nil {
		_ = f.Close()
		return nil, err
	}
	// Clear O_NONBLOCK for simpler blocking reads with deadlines via SetReadDeadline.
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &serialPort{f: f}, nil
}

type serialPort struct {
	f *os.File
}

func (s *serialPort) Read(p []byte) (int, error)  { return s.f.Read(p) }
func (s *serialPort) Write(p []byte) (int, error) { return s.f.Write(p) }
func (s *serialPort) Close() error                { return s.f.Close() }

func (s *serialPort) SetReadDeadline(t time.Time) error {
	return s.f.SetReadDeadline(t)
}

func configureTTY(fd, baud int) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return fmt.Errorf("tcgets: %w", err)
	}
	speed, ok := baudToConst(baud)
	if !ok {
		return fmt.Errorf("unsupported baud %d", baud)
	}
	t.Cflag = unix.CS8 | unix.CREAD | unix.CLOCAL | speed
	t.Iflag = 0
	t.Oflag = 0
	t.Lflag = 0
	t.Cc[unix.VMIN] = 0
	t.Cc[unix.VTIME] = 1 // 100ms read timeout units when VMIN=0
	t.Ispeed = speed
	t.Ospeed = speed
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, t); err != nil {
		return fmt.Errorf("tcsets: %w", err)
	}
	return nil
}

func baudToConst(baud int) (uint32, bool) {
	switch baud {
	case 9600:
		return unix.B9600, true
	case 57600:
		return unix.B57600, true
	case 115200:
		return unix.B115200, true
	case 230400:
		return unix.B230400, true
	default:
		return 0, false
	}
}
