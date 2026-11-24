package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

const (
	joinTimeout  = 10 * time.Second
	writeTimeout = 5 * time.Second
	pingInterval = 20 * time.Second
	readLimit    = 256 * 1024
)

// ServeWS runs one signalling session: join, offer/answer, then events until
// either side hangs up.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.cfg.AllowedOrigins})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(readLimit)

	h.sessions.Add(1)
	defer h.sessions.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer context.AfterFunc(h.ctx, func() {
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		cancel()
	})()
	h.sessionsTotal.Inc()

	reject := func(reason error) {
		data, _ := json.Marshal(message{Type: "error", Message: reason.Error()})
		writeCtx, done := context.WithTimeout(ctx, writeTimeout)
		defer done()
		_ = conn.Write(writeCtx, websocket.MessageText, data)
		_ = conn.Close(websocket.StatusPolicyViolation, reason.Error())
	}

	joinCtx, joined := context.WithTimeout(ctx, joinTimeout)
	first, err := readMessage(joinCtx, conn)
	joined()
	if err != nil {
		return
	}
	if first.Type != "join" {
		reject(errors.New("the first message must be a join"))
		return
	}
	peer, err := h.join(first.Room, first.Lang, first.Name, first.Token)
	if err != nil {
		reject(err)
		return
	}
	defer h.leave(peer)

	go func() {
		defer cancel()
		ping := time.NewTicker(pingInterval)
		defer ping.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-peer.send:
				writeCtx, done := context.WithTimeout(ctx, writeTimeout)
				err := conn.Write(writeCtx, websocket.MessageText, data)
				done()
				if err != nil {
					return
				}
			case <-ping.C:
				pingCtx, done := context.WithTimeout(ctx, writeTimeout)
				err := conn.Ping(pingCtx)
				done()
				if err != nil {
					return
				}
			}
		}
	}()

	for {
		msg, err := readMessage(ctx, conn)
		if err != nil {
			return
		}
		switch msg.Type {
		case "offer":
			answer, err := peer.negotiate(msg.SDP, cancel)
			if err != nil {
				h.log.Warn("negotiation failed", "peer", peer.ID, "error", err)
				peer.notify(message{Type: "error", Message: "negotiation failed: " + err.Error()})
				continue
			}
			peer.notify(message{Type: "answer", SDP: answer})
		case "same-room":
			peer.room.setSameRoom(msg.SameRoom != nil && *msg.SameRoom)
		case "headphones":
			peer.room.setHeadphones(msg.Headphones != nil && *msg.Headphones)
		case "leave":
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		default:
			peer.notify(message{Type: "error", Message: "unknown message type: " + msg.Type})
		}
	}
}

func readMessage(ctx context.Context, conn *websocket.Conn) (message, error) {
	kind, data, err := conn.Read(ctx)
	if err != nil {
		return message{}, err
	}
	var msg message
	if kind != websocket.MessageText || json.Unmarshal(data, &msg) != nil {
		return message{Type: "invalid"}, nil
	}
	return msg, nil
}
