package invite

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIssueAndVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	issuer := New([]byte("secret"), time.Hour)
	issuer.now = func() time.Time { return now }

	token, expires := issuer.Issue("room-1")
	if !expires.Equal(now.Add(time.Hour)) {
		t.Errorf("expires = %v", expires)
	}
	if err := issuer.Verify("room-1", token); err != nil {
		t.Fatalf("fresh token rejected: %v", err)
	}

	stamp, signature, _ := strings.Cut(token, ".")
	later := strings.ToUpper(signature[:1]) + signature[1:]
	if later == signature {
		later = strings.ToLower(signature[:1]) + signature[1:]
	}
	rejected := map[string]string{
		"":                                 "room-1",
		"nonsense":                         "room-1",
		stamp + "." + later:                "room-1",
		"zzzzzz." + signature:              "room-1",
		token:                              "room-2",
		token + "x":                        "room-1",
		strings.Replace(token, ".", "", 1): "room-1",
	}
	for bad, room := range rejected {
		if err := issuer.Verify(room, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Verify(%q, %q) = %v, want ErrInvalid", room, bad, err)
		}
	}

	if err := New([]byte("other"), time.Hour).Verify("room-1", token); !errors.Is(err, ErrInvalid) {
		t.Errorf("token accepted under another key: %v", err)
	}

	now = now.Add(time.Hour)
	if err := issuer.Verify("room-1", token); !errors.Is(err, ErrExpired) {
		t.Errorf("expired token: %v", err)
	}
}

func TestLoadKeyPersists(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || !bytes.Equal(first, second) {
		t.Errorf("key not kept between loads")
	}
	ephemeral, _ := LoadKey("")
	if bytes.Equal(first, ephemeral) {
		t.Errorf("in-memory key reused the stored one")
	}
}
