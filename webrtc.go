package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/pion/webrtc/v3"
)

// peerConnections tracks every active PeerConnection so that a viewer
// disconnecting can never touch another viewer's session. The map and its
// mutex are the only shared state: each offer request gets its own local
// SessionDescription and PeerConnection.
var (
	peerMu          sync.Mutex
	peerConnections = make(map[*webrtc.PeerConnection]struct{})
)

func registerPeer(pc *webrtc.PeerConnection) {
	peerMu.Lock()
	defer peerMu.Unlock()
	peerConnections[pc] = struct{}{}
}

// unregisterPeer removes pc from the registry and reports whether it was
// present, so callers can avoid closing the same PeerConnection twice.
func unregisterPeer(pc *webrtc.PeerConnection) bool {
	peerMu.Lock()
	defer peerMu.Unlock()
	if _, ok := peerConnections[pc]; !ok {
		return false
	}
	delete(peerConnections, pc)
	return true
}

func closeAndRemovePeer(pc *webrtc.PeerConnection) {
	if unregisterPeer(pc) {
		if err := pc.Close(); err != nil {
			log.Printf("Error closing PeerConnection: %v", err)
		}
	}
}

func peerCount() int {
	peerMu.Lock()
	defer peerMu.Unlock()
	return len(peerConnections)
}

func initWebrtcTrack(mediaType string, codec string, trackName string) *webrtc.TrackLocalStaticRTP {
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: codec}, mediaType, trackName)

	if err != nil {
		log.Panicln(err)
	}

	return track
}

// respondError logs the problem and sends an explicit error status. It never
// panics: if the response headers have already been written, the error is
// logged only and the connection is left to the HTTP server.
func respondError(w http.ResponseWriter, status int, message string) {
	log.Println(message)
	defer func() { _ = recover() }()
	http.Error(w, message, status)
}

func OnConnect(w http.ResponseWriter, r *http.Request) {
	// Last-resort guard: an unexpected panic must tear down only this
	// request, never the whole server process.
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("Recovered from panic while handling offer: %v", rec)
			respondError(w, http.StatusInternalServerError, "Internal server error")
		}
	}()

	if config.AllowCrossOrigin {
		header := w.Header()
		header.Add("Access-Control-Allow-Origin", "*")
		header.Add("Access-Control-Allow-Methods", "DELETE, POST, GET, OPTIONS")
		header.Add("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Requested-With")
		header.Add("Access-Control-Allow-Private-Network", "Content-Type, Authorization, X-Requested-With")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	if r.Method != "POST" {
		log.Printf("Rejected %s request: only POST is accepted", r.Method)
		w.Header().Set("Allow", "POST")
		respondError(w, http.StatusMethodNotAllowed, "Invalid HTTP method")
		return
	}

	// Each request decodes into its own local variable: there is no shared
	// offer state, so concurrent requests can never overwrite each other.
	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		respondError(w, http.StatusBadRequest, "Bad session description: malformed JSON")
		return
	}

	if offer.Type != webrtc.SDPTypeOffer || offer.SDP == "" {
		respondError(w, http.StatusBadRequest, "Bad session description: expected a non-empty SDP offer")
		return
	}

	if !config.EnableVideo && !config.EnableAudio {
		respondError(w, http.StatusServiceUnavailable, "No media track is enabled on the server")
		return
	}

	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: iceServersArray,
			},
		},
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to create PeerConnection: "+err.Error())
		return
	}
	registerPeer(peerConnection)

	negotiationSucceeded := false
	defer func() {
		// On any failure path the PeerConnection is closed and removed so it
		// cannot leak; on success it stays registered until the state-change
		// callback observes disconnect/failure/close.
		if !negotiationSucceeded {
			closeAndRemovePeer(peerConnection)
		}
	}()

	if config.EnableVideo {
		if videoTrack == nil {
			respondError(w, http.StatusServiceUnavailable, "Video track is not available")
			return
		}
		if _, err = peerConnection.AddTrack(videoTrack); err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to add video track: "+err.Error())
			return
		}
	}

	if config.EnableAudio {
		if audioTrack == nil {
			respondError(w, http.StatusServiceUnavailable, "Audio track is not available")
			return
		}
		if _, err = peerConnection.AddTrack(audioTrack); err != nil {
			respondError(w, http.StatusInternalServerError, "Failed to add audio track: "+err.Error())
			return
		}
	}

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Connection state changed: %s", state)
		switch state {
		case webrtc.PeerConnectionStateDisconnected,
			webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed:
			// Releasing one viewer only removes that viewer's own
			// PeerConnection; every other session keeps running.
			closeAndRemovePeer(peerConnection)
		}
	})

	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid SDP offer: "+err.Error())
		return
	}

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to create answer: "+err.Error())
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	if err = peerConnection.SetLocalDescription(answer); err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to set local description: "+err.Error())
		return
	}

	<-gatherComplete

	response, err := json.Marshal(peerConnection.LocalDescription())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "Failed to encode answer: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err = w.Write(response); err != nil {
		// The status line is already on the wire; log only, do not crash.
		log.Printf("Failed to send SDP answer to client: %v", err)
		return
	}
	negotiationSucceeded = true
}
