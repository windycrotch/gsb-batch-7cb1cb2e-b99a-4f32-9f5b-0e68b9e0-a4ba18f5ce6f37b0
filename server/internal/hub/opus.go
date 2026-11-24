package hub

import "time"

const defaultFrame = 20 * time.Millisecond

// opusDuration reads the TOC byte of an Opus packet (RFC 6716, section 3.1).
func opusDuration(packet []byte) time.Duration {
	if len(packet) == 0 {
		return defaultFrame
	}
	config := packet[0] >> 3
	var frame time.Duration
	switch {
	case config < 12:
		frame = [...]time.Duration{10, 20, 40, 60}[config&3] * time.Millisecond
	case config < 16:
		frame = [...]time.Duration{10, 20}[config&1] * time.Millisecond
	default:
		frame = [...]time.Duration{2500, 5000, 10000, 20000}[config&3] * time.Microsecond
	}

	frames := 1
	switch packet[0] & 3 {
	case 1, 2:
		frames = 2
	case 3:
		if len(packet) < 2 {
			return defaultFrame
		}
		frames = int(packet[1] & 0x3f)
	}
	total := frame * time.Duration(frames)
	if total <= 0 || total > 120*time.Millisecond {
		return defaultFrame
	}
	return total
}
