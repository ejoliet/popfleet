package proto

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

// PTY output is not valid UTF-8 in general; base64 is in the contract for
// exactly this reason. Feed it every byte value and prove nothing changes.
func TestEncDecAllByteValues(t *testing.T) {
	t.Parallel()
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	got, err := Dec(Enc(all))
	if err != nil {
		t.Fatalf("Dec: %v", err)
	}
	if !bytes.Equal(got, all) {
		t.Fatalf("round trip corrupted bytes:\n got %x\nwant %x", got, all)
	}
}

func TestEncDecThroughJSONFrame(t *testing.T) {
	t.Parallel()
	// The bytes must survive the whole frame, not just Enc/Dec: a raw string
	// field would be mangled by encoding/json's invalid-UTF-8 replacement.
	payloads := [][]byte{
		{},
		{0x00},
		{0xff, 0x00, 0xfe, 0x80, 0x01},
		[]byte("plain ascii\r\n\x1b[2J"),
		bytes.Repeat([]byte{0x00, 0xff}, 4096),
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		b := make([]byte, rng.Intn(1024))
		rng.Read(b)
		payloads = append(payloads, b)
	}
	for _, want := range payloads {
		wire, err := json.Marshal(Msg{T: "out", Sid: "abc", Data: Enc(want)})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var back Msg
		if err := json.Unmarshal(wire, &back); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		got, err := Dec(back.Data)
		if err != nil {
			t.Fatalf("Dec: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame corrupted %d bytes:\n got %x\nwant %x", len(want), got, want)
		}
	}
}

func TestDecRejectsGarbage(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"!!!!", "a", "====", "AAAAA"} {
		if _, err := Dec(s); err == nil {
			t.Errorf("Dec(%q) = nil error, want a decode error", s)
		}
	}
}

// docs/PROTOCOL.md: {"t":"exit","sid":"…","code":0}. Code is *int precisely so
// a zero exit code is still on the wire. Pin it.
func TestExitCodeZeroSerializes(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Msg{T: "exit", Sid: "s1", Code: Int(0)})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"code":0`) {
		t.Fatalf(`exit frame lost the zero code: %s`, b)
	}
	for _, code := range []int{0, 1, 3, 7, 127, 137, -1} {
		b, _ := json.Marshal(Msg{T: "exit", Code: Int(code)})
		var back Msg
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if back.Code == nil || *back.Code != code {
			t.Fatalf("code %d did not survive the wire: %s", code, b)
		}
	}
}

func TestCodeAbsentWhenNil(t *testing.T) {
	t.Parallel()
	b, _ := json.Marshal(Msg{T: "hb"})
	if string(b) != `{"t":"hb"}` {
		t.Fatalf("heartbeat frame carries junk: %s", b)
	}
	var m Msg
	if err := json.Unmarshal([]byte(`{"t":"exit","sid":"s"}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.Code != nil {
		t.Fatalf("missing code decoded as %d, want nil", *m.Code)
	}
}

// Forward compatibility: unknown tags and unknown fields must not break decode.
func TestUnknownTagsAndFieldsDecode(t *testing.T) {
	t.Parallel()
	var m Msg
	if err := json.Unmarshal([]byte(`{"t":"future","sid":"s","wat":{"deep":[1,2]}}`), &m); err != nil {
		t.Fatalf("unknown field broke decode: %v", err)
	}
	if m.T != "future" || m.Sid != "s" {
		t.Fatalf("got %+v", m)
	}
}

func TestFrameShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		m    Msg
		want string
	}{
		{Msg{T: "hello", Token: "tok", Name: "lab1", Ver: "1.0.0"}, `{"t":"hello","token":"tok","name":"lab1","ver":"1.0.0"}`},
		{Msg{T: "hello_ok", ID: "abc"}, `{"t":"hello_ok","id":"abc"}`},
		{Msg{T: "open", Sid: "s", Cmd: "htop"}, `{"t":"open","sid":"s","cmd":"htop"}`},
		{Msg{T: "open", Sid: "s"}, `{"t":"open","sid":"s"}`},
		{Msg{T: "resize", Sid: "s", C: 120, R: 40}, `{"t":"resize","sid":"s","c":120,"r":40}`},
		{Msg{T: "err", Msg: "agent went offline"}, `{"t":"err","msg":"agent went offline"}`},
		{Msg{T: "in", Data: Enc([]byte("x"))}, `{"t":"in","data":"eA=="}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.m)
		if err != nil {
			t.Fatalf("Marshal(%+v): %v", c.m, err)
		}
		if string(b) != c.want {
			t.Errorf("frame mismatch\n got %s\nwant %s", b, c.want)
		}
	}
}
