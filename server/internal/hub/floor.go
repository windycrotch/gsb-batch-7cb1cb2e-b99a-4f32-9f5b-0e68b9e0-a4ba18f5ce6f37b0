package hub

import (
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// audioLevelURI is the RTP header extension in which browsers report how loud
// each packet is, in -dBov: 0 is the loudest, 127 silence.
const audioLevelURI = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"

const (
	noLevel       = -1
	speechLevel   = 50
	floorMargin   = 6
	levelMemory   = 200 * time.Millisecond
	floorHangover = 600 * time.Millisecond
)

func audioLevelID(receiver *webrtc.RTPReceiver) uint8 {
	for _, ext := range receiver.GetParameters().HeaderExtensions {
		if ext.URI == audioLevelURI {
			return uint8(ext.ID)
		}
	}
	return 0
}

func audioLevel(packet *rtp.Packet, id uint8) int {
	if id == 0 {
		return noLevel
	}
	raw := packet.GetExtension(id)
	if raw == nil {
		return noLevel
	}
	var ext rtp.AudioLevelExtension
	if ext.Unmarshal(raw) != nil {
		return noLevel
	}
	return int(ext.Level)
}

func speaking(level int) bool {
	return level != noLevel && level <= speechLevel
}

// hasFloor tells, in a room shared by both participants, whether this packet
// belongs to the person speaking. Both microphones hear the same voice, the
// one closer to it louder. The caller holds r.mu.
func (r *Room) hasFloor(p *Peer, level int, now time.Time) bool {
	if level == noLevel {
		return true
	}
	p.level, p.levelAt = level, now
	held := r.floor != nil && now.Before(r.floorUntil)
	if !speaking(level) {
		return !held || r.floor == p
	}
	heard := func(q *Peer) bool { return now.Sub(q.levelAt) <= levelMemory && speaking(q.level) }
	if held && r.floor != p && heard(r.floor) && p.level+floorMargin > r.floor.level {
		return false
	}
	if !held {
		if other := r.other(p); other != nil && heard(other) && other.level < p.level {
			return false
		}
	}
	r.floor, r.floorUntil = p, now.Add(floorHangover)
	return true
}
