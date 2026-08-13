// Gate v2-0 harness: plays the browser side of /ws/term against the Worker
// relay. Verifies e2e negotiation, encrypted round trip, ciphertext on the
// wire, tamper-kill, and (mode "rtt") measures keystroke round-trip time the
// way GATES.md Gate 0 demands: socket frame timestamps, not feel.
//
//	go run ./contrib/gateharness 'wss://host/ws/term/SID?k=KEY' SID $POPFLEET_E2E_KEY normal|tamper|rtt
//
//	Mint SID/KEY with: curl -X POST -H "Authorization: Bearer $POPFLEET_ADMIN_TOKEN" \
//		https://host/api/machines/MID/term
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ejoliet/popfleet/internal/e2e"
)

type msg struct {
	T    string `json:"t"`
	Sid  string `json:"sid,omitempty"`
	Data string `json:"data,omitempty"`
	Cmd  string `json:"cmd,omitempty"`
	Code *int   `json:"code,omitempty"`
	C    int    `json:"c,omitempty"`
	R    int    `json:"r,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

func main() {
	wsURL, sid, keyB64, mode := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	key, _ := base64.StdEncoding.DecodeString(keyB64)
	enc, err := e2e.NewSession(key, sid)
	if err != nil {
		panic(err)
	}
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		panic(err)
	}
	defer ws.Close()

	ws.SetReadDeadline(time.Now().Add(20 * time.Second))
	var m msg
	if err := ws.ReadJSON(&m); err != nil || m.T != "e2e" {
		panic(fmt.Sprintf("first frame = %+v err=%v, want e2e negotiation", m, err))
	}
	fmt.Println("HARNESS: got e2e negotiation frame")
	ws.WriteJSON(msg{T: "resize", C: 100, R: 30})

	send := func(s string) {
		ws.WriteJSON(msg{T: "in", Data: enc.Seal([]byte(s))})
	}

	if mode == "tamper" {
		// flip bytes in a validly-sealed frame -> agent must kill with err
		w := enc.Seal([]byte("never runs"))
		raw, _ := base64.StdEncoding.DecodeString(w)
		raw[len(raw)-1] ^= 1
		ws.WriteJSON(msg{T: "in", Data: base64.StdEncoding.EncodeToString(raw)})
		for {
			if err := ws.ReadJSON(&m); err != nil {
				panic("socket closed without err frame: " + err.Error())
			}
			if m.T == "err" {
				fmt.Println("HARNESS: tampered frame killed session with err:", m.Msg)
				return
			}
			if m.T == "out" {
				if _, err := enc.Open(m.Data); err != nil {
					panic("relay forwarded garbage after tamper")
				}
			}
		}
	}

	if mode == "rtt" {
		// Keystroke RTT: type single chars at `cat`, timestamp send -> echo.
		// stty -echo so the only bytes coming back are cat's; the split
		// marker keeps the command's own echo from matching. A timed-out
		// drain read would kill the gorilla conn, so drain by marker instead.
		send("stty -echo; printf 'DRAIN''_END'; cat\n")
		var drained strings.Builder
		for !strings.Contains(drained.String(), "DRAIN_END") {
			ws.SetReadDeadline(time.Now().Add(10 * time.Second))
			if err := ws.ReadJSON(&m); err != nil {
				panic("rtt drain: " + err.Error())
			}
			if m.T == "out" {
				b, err := enc.Open(m.Data)
				if err != nil {
					panic("rtt drain decrypt: " + err.Error())
				}
				drained.Write(b)
			}
		}
		var samples []time.Duration
		for i := 0; i < 50; i++ {
			t0 := time.Now()
			send("x\n") // newline: the PTY is canonical, cat sees whole lines
			for {
				ws.SetReadDeadline(time.Now().Add(5 * time.Second))
				if err := ws.ReadJSON(&m); err != nil {
					panic("rtt read: " + err.Error())
				}
				if m.T == "out" {
					if _, err := enc.Open(m.Data); err != nil {
						panic("rtt out failed decrypt: " + err.Error())
					}
					samples = append(samples, time.Since(t0))
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		fmt.Printf("HARNESS: keystroke RTT over %d samples: p50=%v p90=%v max=%v (gate: p50 < 250ms)\n",
			len(samples), samples[len(samples)/2], samples[len(samples)*9/10], samples[len(samples)-1])
		return
	}

	send("printf 'E2E_ROUNDTRIP_%s' OK; sleep 30\n")
	var plain strings.Builder
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ws.SetReadDeadline(deadline)
		if err := ws.ReadJSON(&m); err != nil {
			break
		}
		if m.T != "out" {
			continue
		}
		if strings.Contains(m.Data, "E2E_ROUNDTRIP") {
			panic("PLAINTEXT ON THE WIRE")
		}
		b, err := enc.Open(m.Data)
		if err != nil {
			panic("out frame failed decrypt: " + err.Error())
		}
		plain.Write(b)
		if strings.Contains(plain.String(), "E2E_ROUNDTRIP_OK") {
			fmt.Println("HARNESS: encrypted round trip OK; wire was ciphertext only")
			return
		}
	}
	panic("never saw E2E_ROUNDTRIP_OK; got: " + plain.String())
}
