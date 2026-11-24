// Package invite signs and checks the expiring links that let someone into a
// room.
package invite

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalid = errors.New("this invitation link is not valid")
	ErrExpired = errors.New("this invitation link has expired")
)

type Issuer struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

func New(key []byte, ttl time.Duration) *Issuer {
	return &Issuer{key: key, ttl: ttl, now: time.Now}
}

func (i *Issuer) Issue(room string) (string, time.Time) {
	expires := i.now().Add(i.ttl).Truncate(time.Second)
	stamp := strconv.FormatInt(expires.Unix(), 36)
	return stamp + "." + i.sign(room, stamp), expires
}

func (i *Issuer) Verify(room, token string) error {
	stamp, signature, ok := strings.Cut(token, ".")
	unix, err := strconv.ParseInt(stamp, 36, 64)
	if !ok || err != nil || !hmac.Equal([]byte(signature), []byte(i.sign(room, stamp))) {
		return ErrInvalid
	}
	if !i.now().Before(time.Unix(unix, 0)) {
		return ErrExpired
	}
	return nil
}

func (i *Issuer) sign(room, stamp string) string {
	mac := hmac.New(sha256.New, i.key)
	mac.Write([]byte(room + "\x00" + stamp))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:18])
}

// LoadKey returns the signing key kept in dir, creating it on first use. With
// no dir the key lives in memory and links stop working after a restart.
func LoadKey(dir string) ([]byte, error) {
	key := make([]byte, 32)
	if dir == "" {
		_, err := rand.Read(key)
		return key, err
	}
	path := filepath.Join(dir, "invite.key")
	if existing, err := os.ReadFile(path); err == nil && len(existing) == len(key) {
		return existing, nil
	}
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return key, os.WriteFile(path, key, 0o600)
}
