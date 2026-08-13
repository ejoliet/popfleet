package e2e

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func fleetKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	k := fleetKey(t)
	s, err := NewSession(k, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range [][]byte{nil, {}, []byte("x"), []byte("hello world"), bytes.Repeat([]byte{0xff, 0x00}, 5000)} {
		got, err := s.Open(s.Seal(plain))
		if err != nil {
			t.Fatalf("Open(Seal(%d bytes)): %v", len(plain), err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("round trip mismatch for %d bytes", len(plain))
		}
	}
}

func TestTamperFailsClosed(t *testing.T) {
	s, _ := NewSession(fleetKey(t), "sid1")
	wire := s.Seal([]byte("secret keystrokes"))
	raw, _ := base64.StdEncoding.DecodeString(wire)
	for i := range raw { // flip one byte anywhere: nonce, ciphertext, tag
		mut := bytes.Clone(raw)
		mut[i] ^= 0x01
		if _, err := s.Open(base64.StdEncoding.EncodeToString(mut)); err == nil {
			t.Fatalf("tampered byte %d accepted", i)
		}
	}
}

func TestSessionsAreIndependent(t *testing.T) {
	k := fleetKey(t)
	a, _ := NewSession(k, "sid-a")
	b, _ := NewSession(k, "sid-b")
	if _, err := b.Open(a.Seal([]byte("hi"))); err == nil {
		t.Fatal("sid-b key decrypted sid-a frame: HKDF salt not applied")
	}
}

func TestNonceUnique(t *testing.T) {
	s, _ := NewSession(fleetKey(t), "sid")
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		raw, _ := base64.StdEncoding.DecodeString(s.Seal([]byte("x")))
		n := string(raw[:12])
		if seen[n] {
			t.Fatal("nonce reuse")
		}
		seen[n] = true
	}
}

func TestParseKey(t *testing.T) {
	if _, err := ParseKey(base64.StdEncoding.EncodeToString(make([]byte, 32))); err != nil {
		t.Fatalf("valid key rejected: %v", err)
	}
	for _, bad := range []string{"", "not-base64!!!", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		if _, err := ParseKey(bad); err == nil {
			t.Fatalf("bad key %q accepted", bad)
		}
	}
	if _, err := NewSession(make([]byte, 16), "sid"); err == nil {
		t.Fatal("short fleet key accepted")
	}
	// garbage wire values must error, not panic
	s, _ := NewSession(make([]byte, 32), "sid")
	for _, w := range []string{"", "!!!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := s.Open(w); err == nil {
			t.Fatalf("garbage wire %q accepted", w)
		}
	}
	if strings.TrimSpace(info) != info || info == "" {
		t.Fatal("info constant malformed")
	}
}
