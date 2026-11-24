package main

import (
	"log"
	"net"
	"time"

	"github.com/pion/webrtc/v3"
)

// rtpWriter is the small surface of *webrtc.TrackLocalStaticRTP that the
// receive loop depends on. Declaring it here lets tests substitute a writer
// that permanently fails, proving write errors never stop the loop.
type rtpWriter interface {
	Write([]byte) (int, error)
}

// listenRtp reads UDP RTP packets forever and forwards them to a track.
//
// It is built to survive an RTP source disappearing and coming back:
//   - a UDP read error (source interruption, transient socket error) is
//     logged and the read loop continues instead of exiting;
//   - a track write error (no viewer yet, every PeerConnection closed) is
//     logged and treated as non-fatal;
//   - a nil track means this media is not enabled, in which case nothing is
//     written;
//   - close(stop) makes the loop return cleanly (used by tests).
func listenRtp(ip string, port int, mediaType string, track rtpWriter, stop <-chan struct{}) {
	client, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	})
	if err != nil {
		log.Printf("Fatal: cannot bind UDP socket for %s on %s:%d: %v", mediaType, ip, port, err)
		return
	}
	listenRtpConn(client, mediaType, track, stop)
}

// listenRtpConn runs the receive loop on an already-bound UDP connection.
func listenRtpConn(client net.PacketConn, mediaType string, track rtpWriter, stop <-chan struct{}) {
	defer client.Close()

	addr := client.LocalAddr()
	log.Printf("Listening for UDP RTP packets (%s) on %s", mediaType, addr)

	buf := make([]byte, 1500)

	for {
		select {
		case <-stop:
			log.Printf("Stopping RTP receive loop (%s) on %s", mediaType, addr)
			return
		default:
		}

		// A periodic read deadline keeps the loop responsive to stop even
		// when no packets arrive at all.
		_ = client.SetReadDeadline(time.Now().Add(time.Second))
		n, _, readErr := client.ReadFrom(buf)

		if readErr != nil {
			// Source interruption or any transient socket problem: record it
			// and keep looping so recovery is automatic when the source
			// returns. Do NOT return.
			log.Printf("RTP receive error (%s): %v; continuing", mediaType, readErr)
			continue
		}

		if n == 0 {
			continue
		}

		if track == nil {
			// Media disabled: never feed invalid data into a missing track.
			log.Printf("No %s track available; dropping %d-byte RTP packet", mediaType, n)
			continue
		}

		if _, writeErr := track.Write(buf[:n]); writeErr != nil {
			// No viewer, closed PeerConnection, etc. Non-fatal: the receive
			// side must keep working so later viewers see the stream.
			log.Printf("RTP write to %s track failed (non-fatal): %v; continuing", mediaType, writeErr)
			continue
		}
	}
}

// ensure *webrtc.TrackLocalStaticRTP satisfies rtpWriter at compile time.
var _ rtpWriter = (*webrtc.TrackLocalStaticRTP)(nil)
