// Package agent is the outbound-only popfleet agent: dial the broker,
// heartbeat, spawn one PTY per session id, relay bytes. It opens no
// listening sockets and reconnects forever.
package agent

import (
	"fmt"
	"log"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"

	"github.com/ejoliet/popfleet/internal/proto"
)

// Version is stamped at release time via
// -ldflags="-X github.com/ejoliet/popfleet/internal/agent.Version=<tag>".
// Plain `go build` keeps "dev" so unstamped binaries are honest about it.
var Version = "dev"

// Run dials the broker and never returns (reconnect loop with backoff
// 1 s doubling to 30 s, ±20% jitter), except on a config error.
func Run(rawURL, token, name string) error {
	wsURL, err := agentWSURL(rawURL)
	if err != nil {
		return err
	}
	backoff := time.Second
	for {
		ok, err := session(wsURL, token, name)
		if err != nil {
			log.Printf("agent: connection lost: %v", err)
		}
		if ok {
			backoff = time.Second // successful hello resets backoff
		}
		jitter := 0.8 + rand.Float64()*0.4 // ±20%
		time.Sleep(time.Duration(float64(backoff) * jitter))
		backoff = nextBackoff(backoff)
	}
}

// nextBackoff doubles up to the 30 s cap. Split out of Run so the sequence is
// testable: Run's loop never returns, so it has no seam of its own.
func nextBackoff(d time.Duration) time.Duration {
	if d *= 2; d > maxBackoff {
		return maxBackoff
	}
	return d
}

const maxBackoff = 30 * time.Second

// agentWSURL turns http(s):// or ws(s):// into the /ws/agent endpoint.
func agentWSURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("bad POPFLEET_URL: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("bad POPFLEET_URL scheme %q (want http/https/ws/wss)", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws/agent"
	return u.String(), nil
}

type ptySession struct {
	f   *os.File
	cmd *exec.Cmd
}

// session runs one connection to the broker. Returns hello-succeeded plus
// the terminal error. On any exit every PTY is killed.
func session(wsURL, token, name string) (helloOK bool, err error) {
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return false, err
	}
	defer ws.Close()

	var wmu sync.Mutex // gorilla allows one concurrent writer
	send := func(m proto.Msg) error {
		wmu.Lock()
		defer wmu.Unlock()
		return ws.WriteJSON(m)
	}

	if err := send(proto.Msg{T: "hello", Token: token, Name: name, Ver: Version}); err != nil {
		return false, err
	}
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var m proto.Msg
	if err := ws.ReadJSON(&m); err != nil || m.T != "hello_ok" {
		return false, fmt.Errorf("hello rejected (bad or revoked token?): %v", err)
	}
	log.Printf("agent: connected as machine %s", m.ID)

	var pmu sync.Mutex
	ptys := map[string]*ptySession{}
	defer func() { // socket loss kills every PTY (requirement 7)
		pmu.Lock()
		for sid, p := range ptys {
			p.cmd.Process.Kill()
			p.f.Close()
			delete(ptys, sid)
		}
		pmu.Unlock()
	}()

	// Liveness: broker sends nothing to an idle agent, so JSON reads alone
	// can't detect a dead broker. Ping alongside each hb; pongs (gorilla
	// auto-replies to pings) extend the read deadline.
	const liveness = 45 * time.Second
	ws.SetReadDeadline(time.Now().Add(liveness))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(liveness))
	})

	done := make(chan struct{})
	defer close(done)
	go func() { // heartbeat every 10 s
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if send(proto.Msg{T: "hb"}) != nil {
					return
				}
				ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()

	for {
		var m proto.Msg
		if err := ws.ReadJSON(&m); err != nil {
			return true, err
		}
		switch m.T {
		case "open":
			pmu.Lock()
			if _, exists := ptys[m.Sid]; exists { // duplicate open ignored
				pmu.Unlock()
				continue
			}
			p, err := spawn(m.Cmd)
			if err != nil {
				pmu.Unlock()
				log.Printf("agent: spawn failed for sid %s: %v", m.Sid, err)
				send(proto.Msg{T: "exit", Sid: m.Sid, Code: proto.Int(127)})
				continue
			}
			ptys[m.Sid] = p
			pmu.Unlock()
			go pump(p, m.Sid, send, func() {
				pmu.Lock()
				delete(ptys, m.Sid)
				pmu.Unlock()
			})
		case "in":
			b, err := proto.Dec(m.Data)
			if err != nil {
				continue
			}
			pmu.Lock()
			if p, ok := ptys[m.Sid]; ok {
				p.f.Write(b)
			}
			pmu.Unlock()
		case "resize":
			pmu.Lock()
			if p, ok := ptys[m.Sid]; ok {
				pty.Setsize(p.f, &pty.Winsize{Cols: uint16(m.C), Rows: uint16(m.R)})
			}
			pmu.Unlock()
		case "close":
			pmu.Lock()
			if p, ok := ptys[m.Sid]; ok {
				p.cmd.Process.Kill() // pump sends exit and forgets the sid
			}
			pmu.Unlock()
		}
	}
}

// spawn starts a login shell (or `$SHELL -c cmd`) on a fresh PTY.
func spawn(cmdline string) (*ptySession, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	var c *exec.Cmd
	if cmdline != "" {
		c = exec.Command(shell, "-c", cmdline)
	} else {
		c = exec.Command(shell, "-l")
	}
	c.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.Start(c)
	if err != nil {
		return nil, err
	}
	return &ptySession{f: f, cmd: c}, nil
}

// pump copies PTY output to the broker, then reports exit and forgets the sid.
func pump(p *ptySession, sid string, send func(proto.Msg) error, forget func()) {
	buf := make([]byte, 8192)
	for {
		n, err := p.f.Read(buf)
		if n > 0 {
			if send(proto.Msg{T: "out", Sid: sid, Data: proto.Enc(buf[:n])}) != nil {
				break // socket gone; session() defer kills the PTY
			}
		}
		if err != nil {
			break // EIO on shell exit is the normal path
		}
	}
	code := 0
	if err := p.cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
			// ExitCode() is -1 when the shell died on a signal; the protocol
			// says 128+N (137 for SIGKILL), matching the Python agent.
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
		} else {
			code = 1
		}
	}
	p.f.Close()
	forget()
	send(proto.Msg{T: "exit", Sid: sid, Code: proto.Int(code)})
}
