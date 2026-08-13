// Package proto is the frozen v1 wire contract (docs/PROTOCOL.md).
// One struct covers every frame; unknown tags are ignored by all sides.
package proto

import "encoding/base64"

// Msg is a single JSON text frame on /ws/agent or /ws/term.
type Msg struct {
	T     string `json:"t"`
	Token string `json:"token,omitempty"` // hello
	Name  string `json:"name,omitempty"`  // hello
	Ver   string `json:"ver,omitempty"`   // hello
	E2E   bool   `json:"e2e,omitempty"`   // hello/hello_ok: v1e payload encryption
	ID    string `json:"id,omitempty"`    // hello_ok
	Sid   string `json:"sid,omitempty"`
	Data  string `json:"data,omitempty"` // base64 bytes
	Cmd   string `json:"cmd,omitempty"`  // open; empty = login shell
	Code  *int   `json:"code,omitempty"` // exit; pointer so code 0 still serializes
	C     int    `json:"c,omitempty"`    // resize cols
	R     int    `json:"r,omitempty"`    // resize rows
	Msg   string `json:"msg,omitempty"`  // err
}

// Enc encodes raw PTY bytes for the wire.
func Enc(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// Dec decodes a wire data field back to raw bytes.
func Dec(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// Int returns a pointer for the exit-code field.
func Int(i int) *int { return &i }
