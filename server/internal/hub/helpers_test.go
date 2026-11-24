package hub_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/whispem/ambivox/server/internal/hub"
)

func httptestHandler(h *hub.Hub) http.Handler {
	return http.HandlerFunc(h.ServeWS)
}

func TestOpusDuration(t *testing.T) {
	cases := []struct {
		packet []byte
		want   time.Duration
	}{
		{nil, 20 * time.Millisecond},
		{[]byte{0x78}, 20 * time.Millisecond},
		{[]byte{0x08}, 20 * time.Millisecond},
		{[]byte{0x00}, 10 * time.Millisecond},
		{[]byte{0x18}, 60 * time.Millisecond},
		{[]byte{0x60}, 10 * time.Millisecond},
		{[]byte{0x80}, 2500 * time.Microsecond},
		{[]byte{0xf8}, 20 * time.Millisecond},
		{[]byte{0x79}, 40 * time.Millisecond},
		{[]byte{0x7b, 0x03}, 60 * time.Millisecond},
		{[]byte{0x7b}, 20 * time.Millisecond},
		{[]byte{0x7b, 0x00}, 20 * time.Millisecond},
		{[]byte{0x1b, 0x3f}, 20 * time.Millisecond},
	}
	for _, c := range cases {
		if got := hub.OpusDuration(c.packet); got != c.want {
			t.Errorf("OpusDuration(%x) = %v, want %v", c.packet, got, c.want)
		}
	}
}

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"":                      "Guest",
		"   ":                   "Guest",
		" Em \t Dupont\n":       "Em Dupont",
		strings.Repeat("é", 50): strings.Repeat("é", 40),
	}
	for in, want := range cases {
		if got := hub.CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}
