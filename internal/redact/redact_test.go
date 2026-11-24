package redact

import (
	"errors"
	"strings"
	"testing"
)

func TestURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "user and password",
			in:   "rtsp://camera-user:camera-password@192.0.2.10:554/stream1",
			want: "rtsp://REDACTED@192.0.2.10:554/stream1",
		},
		{
			name: "uppercase scheme",
			in:   "RTSPS://token:secret@example.test/live",
			want: "rtsps://REDACTED@example.test/live",
		},
		{
			name: "user only",
			in:   "rtsp://admin@10.0.0.1:8554/cam",
			want: "rtsp://REDACTED@10.0.0.1:8554/cam",
		},
		{
			name: "query and path preserved",
			in:   "rtsp://u:p@host:554/cam/realmonitor?channel=1&subtype=0",
			want: "rtsp://REDACTED@host:554/cam/realmonitor?channel=1&subtype=0",
		},
		{
			name: "percent encoded password",
			in:   "rtsp://user:p%40ss%3Aword@host:554/path",
			want: "rtsp://REDACTED@host:554/path",
		},
		{
			name: "ipv6 authority",
			in:   "rtsp://u:p@[fe80::1%25en0]:8554/stream1",
			want: "rtsp://REDACTED@[fe80::1%25en0]:8554/stream1",
		},
		{
			name: "no credentials unchanged",
			in:   "rtsp://192.0.2.10:554/stream1",
			want: "rtsp://192.0.2.10:554/stream1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := URL(tc.in); got != tc.want {
				t.Fatalf("URL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestText(t *testing.T) {
	input := "open rtsp://camera-user:camera-password@192.0.2.10:554/stream1 and RTSPS://token:secret@example.test/live today"
	got := Text(input)
	for _, secret := range []string{"camera-user", "camera-password", "token", "secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted text still contains %q: %s", secret, got)
		}
	}
	for _, kept := range []string{
		"192.0.2.10:554/stream1",
		"example.test/live",
		"rtsp://REDACTED@",
		"RTSPS://REDACTED@",
	} {
		if !strings.Contains(got, kept) {
			t.Fatalf("redacted text lost %q: %s", kept, got)
		}
	}
}

func TestTextLeavesCredentialFreeURLsAlone(t *testing.T) {
	plain := "rtsp://192.0.2.10:554/stream1"
	if got := Text(plain); got != plain {
		t.Fatalf("credential-free URL changed: got %q want %q", got, plain)
	}

	// A malformed URL that still carries an "@" must not leak the secret.
	malformed := "rtsp://user:password@example.test/%ZZ"
	got := Text(malformed)
	if strings.Contains(got, "password") || strings.Contains(got, "user:") {
		t.Fatalf("malformed URL leaked credentials: %s", got)
	}
	if !strings.Contains(got, "example.test/%ZZ") {
		t.Fatalf("malformed URL lost host/path: %s", got)
	}
}

func TestTextIgnoresUnrelatedSchemes(t *testing.T) {
	input := "http://u:p@example.test/page should stay untouched"
	if got := Text(input); got != input {
		t.Fatalf("non-RTSP URL was rewritten: %q", got)
	}
}

func TestErrorDoesNotExposeOriginal(t *testing.T) {
	original := errors.New("describe failed: rtsp://u:secret@host:554/stream1 boom")
	safe := Error(original)
	if strings.Contains(safe.Error(), "secret") {
		t.Fatalf("safe error leaks credential: %v", safe)
	}
	if !strings.Contains(safe.Error(), "host:554/stream1") {
		t.Fatalf("safe error lost location info: %v", safe)
	}
	// The original error must not be reachable through Unwrap.
	if err := errors.Unwrap(safe); err != nil {
		t.Fatalf("redacted error unexpectedly wraps %v", err)
	}
}

func TestNilPassThrough(t *testing.T) {
	if Error(nil) != nil {
		t.Fatal("Error(nil) must be nil")
	}
}

func TestErrorf(t *testing.T) {
	err := Errorf("describe: %s", "rtsp://u:p@h:554/s")
	if strings.Contains(err.Error(), "p@") {
		t.Fatalf("Errorf leaked credentials: %v", err)
	}
	if !strings.Contains(err.Error(), "h:554/s") {
		t.Fatalf("Errorf dropped location: %v", err)
	}
}
