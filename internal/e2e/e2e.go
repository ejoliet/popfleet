// Package e2e implements protocol v1e payload encryption (docs/RDD-v2.md):
// every data/cmd value becomes base64( nonce(12) || AES-256-GCM ciphertext )
// under a per-session key HKDF-SHA256(fleet key, salt=sid, info="popfleet-v2").
// The relay never holds the fleet key; a tampered frame fails the GCM tag and
// the session dies rather than rendering garbage.
package e2e

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

const (
	keyLen   = 32 // AES-256
	nonceLen = 12 // GCM standard
	info     = "popfleet-v2"
)

// ParseKey decodes POPFLEET_E2E_KEY (base64, exactly 32 bytes).
func ParseKey(s string) ([]byte, error) {
	k, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("POPFLEET_E2E_KEY is not valid base64: %w", err)
	}
	if len(k) != keyLen {
		return nil, fmt.Errorf("POPFLEET_E2E_KEY must be %d bytes, got %d (generate with: openssl rand -base64 32)", keyLen, len(k))
	}
	return k, nil
}

// Session holds the derived per-sid AEAD. One per open session.
type Session struct {
	aead cipher.AEAD
}

// NewSession derives the per-session key: HKDF-SHA256(fleetKey, salt=sid).
func NewSession(fleetKey []byte, sid string) (*Session, error) {
	if len(fleetKey) != keyLen {
		return nil, errors.New("e2e: fleet key must be 32 bytes")
	}
	k, err := hkdf.Key(sha256.New, fleetKey, []byte(sid), info, keyLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Session{aead: aead}, nil
}

// Seal encrypts raw bytes to the wire form: base64(nonce || ciphertext).
func (s *Session) Seal(plain []byte) string {
	out := make([]byte, nonceLen, nonceLen+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(out[:nonceLen]); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	out = s.aead.Seal(out, out[:nonceLen], plain, nil)
	return base64.StdEncoding.EncodeToString(out)
}

// Open decrypts a wire value. Any error means a forged, tampered or
// wrong-key frame: the caller must kill the session, never render.
func (s *Session) Open(wire string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(wire)
	if err != nil {
		return nil, err
	}
	if len(raw) < nonceLen {
		return nil, errors.New("e2e: frame too short")
	}
	return s.aead.Open(nil, raw[:nonceLen], raw[nonceLen:], nil)
}
