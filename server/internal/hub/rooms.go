package hub

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type grant struct {
	Room    string `json:"room"`
	Token   string `json:"token,omitempty"`
	Expires string `json:"expires,omitempty"`
}

func (h *Hub) InviteOnly() bool {
	return h.cfg.Invites != nil
}

func (h *Hub) AccessKeyRequired() bool {
	return h.cfg.AccessKey != ""
}

func (h *Hub) authorized(r *http.Request) bool {
	if h.cfg.AccessKey == "" {
		return true
	}
	given, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	a, b := sha256.Sum256([]byte(given)), sha256.Sum256([]byte(h.cfg.AccessKey))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// CreateRoom picks an unguessable room name and, unless rooms are open,
// signs the invitation that lets two people into it.
func (h *Hub) CreateRoom(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !h.authorized(r) {
		time.Sleep(250 * time.Millisecond)
		http.Error(w, "a valid access key is required to start a call", http.StatusUnauthorized)
		return
	}
	room := grant{Room: newID() + newID()}
	if h.cfg.Invites != nil {
		token, expires := h.cfg.Invites.Issue(room.Room)
		room.Token, room.Expires = token, expires.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(room)
}
