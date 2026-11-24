// Package testclient is a headless participant used by the integration tests.
package testclient

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

type Event map[string]any

func (e Event) String(key string) string {
	s, _ := e[key].(string)
	return s
}

type Client struct {
	ID string

	conn  *websocket.Conn
	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	level atomic.Int32

	mu        sync.Mutex
	events    []Event
	consumed  int
	packets   [][]byte
	closed    bool
	closeCode websocket.StatusCode
	changed   chan struct{}
}

func Dial(ctx context.Context, url string) (*Client, error) {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	c := &Client{conn: conn, changed: make(chan struct{}, 1)}
	c.level.Store(-1)
	go c.read()
	return c, nil
}

func Join(ctx context.Context, url, room, lang, name string) (*Client, Event, error) {
	return JoinWith(ctx, url, Event{"room": room, "lang": lang, "name": name})
}

func JoinWith(ctx context.Context, url string, join Event) (*Client, Event, error) {
	c, err := Dial(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	join["type"] = "join"
	if err := c.Send(join); err != nil {
		c.Close()
		return nil, nil, err
	}
	welcome, err := c.Wait(ctx, "joined")
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	c.ID = welcome.String("id")
	return c, welcome, nil
}

func (c *Client) read() {
	for {
		_, data, err := c.conn.Read(context.Background())
		c.mu.Lock()
		if err != nil {
			c.closed = true
			c.closeCode = websocket.CloseStatus(err)
			c.mu.Unlock()
			c.wake()
			return
		}
		var event Event
		if json.Unmarshal(data, &event) == nil {
			c.events = append(c.events, event)
		}
		c.mu.Unlock()
		c.wake()
	}
}

func (c *Client) wake() {
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *Client) Send(event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.conn.Write(ctx, websocket.MessageText, data)
}

// Wait returns the next unconsumed event of the given type, skipping others.
func (c *Client) Wait(ctx context.Context, kind string) (Event, error) {
	for {
		c.mu.Lock()
		for c.consumed < len(c.events) {
			event := c.events[c.consumed]
			c.consumed++
			if event.String("type") == kind {
				c.mu.Unlock()
				return event, nil
			}
		}
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return nil, fmt.Errorf("connection closed while waiting for %q", kind)
		}
		select {
		case <-c.changed:
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for %q: %w", kind, ctx.Err())
		}
	}
}

func (c *Client) WaitClosed(ctx context.Context) (websocket.StatusCode, error) {
	for {
		c.mu.Lock()
		closed, code := c.closed, c.closeCode
		c.mu.Unlock()
		if closed {
			return code, nil
		}
		select {
		case <-c.changed:
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

func (c *Client) Events(kind string) []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Event
	for _, event := range c.events {
		if event.String("type") == kind {
			out = append(out, event)
		}
	}
	return out
}

func (c *Client) Connect(ctx context.Context) error {
	engine := &webrtc.MediaEngine{}
	capability := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
	err := engine.RegisterCodec(webrtc.RTPCodecParameters{RTPCodecCapability: capability, PayloadType: 111}, webrtc.RTPCodecTypeAudio)
	if err != nil {
		return err
	}
	err = engine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: audioLevelURI}, webrtc.RTPCodecTypeAudio)
	if err != nil {
		return err
	}
	interceptors := &interceptor.Registry{}
	interceptors.Add(levelStamper{&c.level})
	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithSettingEngine(settings), webrtc.WithInterceptorRegistry(interceptors))

	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return err
	}
	c.pc = pc
	if c.track, err = webrtc.NewTrackLocalStaticSample(capability, "audio", "testclient"); err != nil {
		return err
	}
	if _, err = pc.AddTrack(c.track); err != nil {
		return err
	}

	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			packet, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			c.mu.Lock()
			c.packets = append(c.packets, packet.Payload)
			c.mu.Unlock()
		}
	})
	connected := make(chan struct{})
	var once sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		return err
	}
	<-gathered
	if err := c.Send(Event{"type": "offer", "sdp": pc.LocalDescription().SDP}); err != nil {
		return err
	}
	answer, err := c.Wait(ctx, "answer")
	if err != nil {
		return err
	}
	err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer.String("sdp")})
	if err != nil {
		return err
	}
	select {
	case <-connected:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("media connection: %w", ctx.Err())
	}
}

// SetLevel makes the following packets report this loudness in -dBov, as a
// browser does; a negative level stops reporting it.
func (c *Client) SetLevel(level int) {
	c.level.Store(int32(level))
}

func (c *Client) WriteAudio(packet []byte, duration time.Duration) error {
	return c.track.WriteSample(media.Sample{Data: packet, Duration: duration})
}

func (c *Client) Packets() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.packets...)
}

func (c *Client) Close() {
	if c.pc != nil {
		_ = c.pc.Close()
	}
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

const audioLevelURI = "urn:ietf:params:rtp-hdrext:ssrc-audio-level"

type levelStamper struct{ level *atomic.Int32 }

func (f levelStamper) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &stamper{level: f.level}, nil
}

type stamper struct {
	interceptor.NoOp
	level *atomic.Int32
}

func (s *stamper) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	var id uint8
	for _, ext := range info.RTPHeaderExtensions {
		if ext.URI == audioLevelURI {
			id = uint8(ext.ID)
		}
	}
	if id == 0 {
		return writer
	}
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		if level := s.level.Load(); level >= 0 {
			ext, err := rtp.AudioLevelExtension{Level: uint8(level), Voice: level <= 50}.Marshal()
			if err == nil {
				_ = header.SetExtension(id, ext)
			}
		}
		return writer.Write(header, payload, attributes)
	})
}
