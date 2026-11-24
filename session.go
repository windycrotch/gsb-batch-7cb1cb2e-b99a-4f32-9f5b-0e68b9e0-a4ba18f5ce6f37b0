package udpsocket

import (
	"crypto/rand"
	"net"
	"sync"

	"github.com/theredrad/udpsocket/crypto"
)

// A struct to manage sessions secrets
type sessionManager struct {
	sHMACKey []byte //session random key
	cHMACKey []byte //cookie random key

	mu sync.RWMutex // guards the HMAC keys so a restart rotation is race-free
}

// Returns a new session manager
// Generates new random secrets for cookies & session IDs
func newSessionManager() (*sessionManager, error) {
	s := &sessionManager{}
	if err := s.rotateKeys(); err != nil {
		return nil, err
	}
	return s, nil
}

// rotateKeys replaces both HMAC secrets with freshly generated random keys.
// It is used after a Stop so cookies & session IDs minted by a previous epoch
// can no longer validate.
func (s *sessionManager) rotateKeys() error {
	sessionHMAC := make([]byte, 32)
	if _, err := rand.Read(sessionHMAC); err != nil {
		return err
	}

	cookieHMAC := make([]byte, 32)
	if _, err := rand.Read(cookieHMAC); err != nil {
		return err
	}

	s.mu.Lock()
	s.sHMACKey = sessionHMAC
	s.cHMACKey = cookieHMAC
	s.mu.Unlock()
	return nil
}

// Reset invalidates every cookie & session ID issued before the call by
// rotating the signing secrets in place. The sessionManager pointer stays the
// same, so clients/tests that already hold it compute cookies with the new
// epoch's keys instead of being detached from the server.
func (s *sessionManager) Reset() error {
	return s.rotateKeys()
}

// Generates a cookie for an UDP address with params
func (s *sessionManager) GetAddrCookieHMAC(addr *net.UDPAddr, params ...[]byte) []byte {
	return s.GetCookieHMAC(append([][]byte{addr.IP}, params...)...)
}

// Generates a cookie for a byte array with the cookie secret
func (s *sessionManager) GetCookieHMAC(params ...[]byte) []byte {
	s.mu.RLock()
	key := s.cHMACKey
	s.mu.RUnlock()
	return crypto.HMAC(key, params...)
}

// Generates a session HMAC with the params
func (s *sessionManager) GetSessionHMAC(params ...[]byte) []byte {
	s.mu.RLock()
	key := s.sHMACKey
	s.mu.RUnlock()
	return crypto.HMAC(key, params...)
}

// Generates a new random session ID for the address & the user ID
func (s *sessionManager) GenerateSessionID(addr *net.UDPAddr, userID string) ([]byte, error) {
	sessionKey := make([]byte, 32)
	_, err := rand.Read(sessionKey)
	if err != nil {
		return nil, err
	}

	return append(s.GetSessionHMAC(addr.IP, []byte(userID)), sessionKey...), nil
}
