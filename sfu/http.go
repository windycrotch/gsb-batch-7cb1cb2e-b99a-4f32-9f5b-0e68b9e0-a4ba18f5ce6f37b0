package sfu

import (
	"io"
	"mime"
	"net/http"

	"google.golang.org/protobuf/proto"
)

// ProtobufContentType is the content type used by the /servers and /status endpoints.
const ProtobufContentType = "application/x-protobuf"

// isProtobufRequest reports whether the request asks for protobuf. The media type is
// compared without parameters so a client sending "application/x-protobuf; charset=…"
// still reaches the API rather than falling through to the HTML page.
func isProtobufRequest(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return false
	}

	return mediaType == ProtobufContentType
}

// HandleServers serves the /servers endpoint for application/x-protobuf requests:
// GET returns the current state, POST replaces the intended server list and returns
// the resulting state. A POST result therefore reflects what actually happened to the
// submitted list (connected, connecting, retrying, failed) rather than echoing intent.
// It returns true when the request was handled.
func (s *Sfu) HandleServers(w http.ResponseWriter, r *http.Request) bool {
	if !isProtobufRequest(r) {
		return false
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.writeProto(w, s.GetCurrentServers())
		return true
	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Failed to read payload", http.StatusInternalServerError)
			return true
		}

		var update CurrentServers
		if err := proto.Unmarshal(body, &update); err != nil {
			http.Error(w, "Failed to deserialize payload", http.StatusInternalServerError)
			return true
		}

		s.writeProto(w, s.SetCurrentServers(&update))
		return true
	}

	return false
}

// HandleStatus serves the /status endpoint for application/x-protobuf requests.
// It returns true when the request was handled.
func (s *Sfu) HandleStatus(w http.ResponseWriter, r *http.Request) bool {
	if !isProtobufRequest(r) {
		return false
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	s.writeProto(w, s.GetStatus())
	return true
}

func (s *Sfu) writeProto(w http.ResponseWriter, message proto.Message) {
	data, err := proto.Marshal(message)
	if err != nil {
		http.Error(w, "Failed to serialize Protobuf", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", ProtobufContentType)
	w.WriteHeader(http.StatusOK)

	if _, err := w.Write(data); err != nil {
		s.logger.Error("sfu", "Failed writing protobuf response: "+err.Error())
	}
}
