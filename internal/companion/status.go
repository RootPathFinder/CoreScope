package companion

import (
	"encoding/binary"
	"errors"
)

// RepeaterStats mirrors firmware simple_repeater::RepeaterStats (little-endian).
// status_data in PUSH_CODE_STATUS_RESPONSE may be prefixed with a 4-byte
// reflected sender_timestamp; ParseRepeaterStats accepts both layouts.
type RepeaterStats struct {
	BatteryMv      uint16  `json:"batteryMv"`
	TxQueueLen     uint16  `json:"txQueueLen"`
	NoiseFloor     int16   `json:"noiseFloor"`
	LastRSSI       int16   `json:"lastRssi"`
	PacketsRecv    uint32  `json:"packetsRecv"`
	PacketsSent    uint32  `json:"packetsSent"`
	AirtimeSecs    uint32  `json:"airtimeSecs"`
	UptimeSecs     uint32  `json:"uptimeSecs"`
	SentFlood      uint32  `json:"sentFlood"`
	SentDirect     uint32  `json:"sentDirect"`
	RecvFlood      uint32  `json:"recvFlood"`
	RecvDirect     uint32  `json:"recvDirect"`
	ErrEvents      uint16  `json:"errEvents"`
	LastSNR        float64 `json:"lastSnr"` // firmware stores SNR*4 as int16
	DirectDups     uint16  `json:"directDups"`
	FloodDups      uint16  `json:"floodDups"`
	RxAirtimeSecs  uint32  `json:"rxAirtimeSecs"`
	RecvErrors     *uint32 `json:"recvErrors,omitempty"` // present when frame ≥ 56 bytes
}

var ErrShortStatus = errors.New("status payload too short for RepeaterStats")

const minStatsLen = 52 // through rx_airtime; recv_errors optional (+4)

// ParseRepeaterStats parses RepeaterStats from status_data.
// If data is longer and starts with a 4-byte prefix that would leave a valid
// stats blob, the first 4 bytes are treated as the reflected timestamp tag.
func ParseRepeaterStats(data []byte) (RepeaterStats, error) {
	var s RepeaterStats
	off := 0
	if len(data) >= 4+minStatsLen {
		// Prefer tagged layout used by repeater handleRequest replies.
		off = 4
	}
	if len(data) < off+minStatsLen {
		if len(data) >= minStatsLen {
			off = 0
		} else {
			return s, ErrShortStatus
		}
	}
	return parseStatsAt(data, off)
}

func parseStatsAt(data []byte, offset int) (RepeaterStats, error) {
	var s RepeaterStats
	if len(data) < offset+minStatsLen {
		return s, ErrShortStatus
	}
	s.BatteryMv = binary.LittleEndian.Uint16(data[offset : offset+2])
	s.TxQueueLen = binary.LittleEndian.Uint16(data[offset+2 : offset+4])
	s.NoiseFloor = int16(binary.LittleEndian.Uint16(data[offset+4 : offset+6]))
	s.LastRSSI = int16(binary.LittleEndian.Uint16(data[offset+6 : offset+8]))
	s.PacketsRecv = binary.LittleEndian.Uint32(data[offset+8 : offset+12])
	s.PacketsSent = binary.LittleEndian.Uint32(data[offset+12 : offset+16])
	s.AirtimeSecs = binary.LittleEndian.Uint32(data[offset+16 : offset+20])
	s.UptimeSecs = binary.LittleEndian.Uint32(data[offset+20 : offset+24])
	s.SentFlood = binary.LittleEndian.Uint32(data[offset+24 : offset+28])
	s.SentDirect = binary.LittleEndian.Uint32(data[offset+28 : offset+32])
	s.RecvFlood = binary.LittleEndian.Uint32(data[offset+32 : offset+36])
	s.RecvDirect = binary.LittleEndian.Uint32(data[offset+36 : offset+40])
	s.ErrEvents = binary.LittleEndian.Uint16(data[offset+40 : offset+42])
	snrX4 := int16(binary.LittleEndian.Uint16(data[offset+42 : offset+44]))
	s.LastSNR = float64(snrX4) / 4.0
	s.DirectDups = binary.LittleEndian.Uint16(data[offset+44 : offset+46])
	s.FloodDups = binary.LittleEndian.Uint16(data[offset+46 : offset+48])
	s.RxAirtimeSecs = binary.LittleEndian.Uint32(data[offset+48 : offset+52])
	if len(data) >= offset+56 {
		v := binary.LittleEndian.Uint32(data[offset+52 : offset+56])
		s.RecvErrors = &v
	}
	return s, nil
}
