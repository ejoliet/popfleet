// Package broker serves the panel, the admin HTTP API, and the two
// WebSocket endpoints, relaying terminal frames between browsers and
// agents. No PTY ever runs on the broker.
package broker

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ejoliet/popfleet/contrib"
	"github.com/ejoliet/popfleet/internal/panel"
	"github.com/ejoliet/popfleet/internal/proto"
	"github.com/ejoliet/popfleet/internal/store"
)

const (
	offlineAfter = 25 * time.Second // two missed 10 s heartbeats
	agentRead    = 35 * time.Second // read deadline per agent frame
	keyTTL       = 60 * time.Second // one-time session key lifetime
	queueLen     = 256              // bounded outbound queue per connection
)

type Broker struct {
	st       *store.Store
	adminSum [32]byte // sha256 of POPFLEET_ADMIN_TOKEN

	mu     sync.Mutex
	agents map[string]*agentConn // machine id -> live socket
	pend   map[string]pendKey    // sid -> unminted session
	sess   map[string]*session   // sid -> attached browser
}

type pendKey struct {
	keySum [32]byte
	mid    string
	cmd    string
	exp    time.Time
}

func New(st *store.Store, adminToken string) *Broker {
	b := &Broker{
		st:       st,
		adminSum: sha256.Sum256([]byte(adminToken)),
		agents:   map[string]*agentConn{},
		pend:     map[string]pendKey{},
		sess:     map[string]*session{},
	}
	go b.sweep()
	return b
}

// sweep drops expired never-used session keys so they are not leaked.
func (b *Broker) sweep() {
	for range time.Tick(10 * time.Second) {
		now := time.Now()
		b.mu.Lock()
		for sid, p := range b.pend {
			if now.After(p.exp) {
				delete(b.pend, sid)
			}
		}
		b.mu.Unlock()
	}
}

func (b *Broker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /agent.sh", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Write(contrib.AgentSH)
	})
	mux.HandleFunc("GET /agent.py", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
		w.Write(contrib.AgentPY)
	})
	// The broker IS the agent binary, so it can hand out a copy of itself for
	// boxes on its own platform. That makes agent.sh work with no published
	// GitHub release; other platforms still need one (or a manual scp).
	mux.HandleFunc("GET /bin/{name}", func(w http.ResponseWriter, r *http.Request) {
		want := "popfleet-" + runtime.GOOS + "-" + runtime.GOARCH
		if r.PathValue("name") != want {
			http.Error(w, "this broker only serves "+want+" (it can only copy itself)", http.StatusNotFound)
			return
		}
		exe, err := os.Executable()
		if err != nil {
			http.Error(w, "cannot locate own binary", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, exe)
	})
	mux.Handle("GET /{$}", panel.Index())
	mux.Handle("GET /term/{sid}", panel.Term())
	mux.Handle("GET /index.js", panel.Assets())
	mux.Handle("GET /term.js", panel.Assets())
	mux.Handle("GET /vendor/", panel.Assets())

	mux.HandleFunc("POST /api/tokens", b.auth(b.apiMint))
	mux.HandleFunc("GET /api/machines", b.auth(b.apiMachines))
	mux.HandleFunc("POST /api/machines/{id}/term", b.auth(b.apiTerm))
	mux.HandleFunc("DELETE /api/machines/{id}", b.auth(b.apiDelete))

	mux.HandleFunc("GET /ws/agent", b.wsAgent)
	mux.HandleFunc("GET /ws/term/{sid}", b.wsTerm)
	return mux
}

// ---- admin auth ----

func (b *Broker) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sum := sha256.Sum256([]byte(tok))
		if !ok || subtle.ConstantTimeCompare(sum[:], b.adminSum[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// ---- HTTP API ----

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

func (b *Broker) apiMint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req) // body optional
	m, err := b.st.Mint(req.Name)
	if err != nil {
		http.Error(w, "store: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"token": m.Token})
}

func (b *Broker) apiMachines(w http.ResponseWriter, _ *http.Request) {
	type out struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Online   bool   `json:"online"`
		LastSeen string `json:"last_seen"`
		AgentVer string `json:"agent_ver"`
		Sessions int    `json:"sessions"`
	}
	machines := b.st.List()
	b.mu.Lock()
	counts := map[string]int{}
	for _, s := range b.sess {
		counts[s.mid]++
	}
	list := make([]out, 0, len(machines))
	for _, m := range machines {
		o := out{ID: m.ID, Name: m.Name, AgentVer: m.AgentVer, Sessions: counts[m.ID]}
		if !m.LastSeen.IsZero() {
			o.LastSeen = m.LastSeen.Format(time.RFC3339)
		}
		_, connected := b.agents[m.ID]
		o.Online = connected && time.Since(m.LastSeen) < offlineAfter
		list = append(list, o)
	}
	b.mu.Unlock()
	writeJSON(w, list)
}

func (b *Broker) apiTerm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := b.st.ByID(id); !ok {
		http.Error(w, "no such machine", http.StatusNotFound)
		return
	}
	var req struct {
		Cmd string `json:"cmd"`
	}
	json.NewDecoder(r.Body).Decode(&req) // body optional
	sid, key := randHex(8), randHex(24)
	b.mu.Lock()
	b.pend[sid] = pendKey{keySum: sha256.Sum256([]byte(key)), mid: id, cmd: req.Cmd, exp: time.Now().Add(keyTTL)}
	b.mu.Unlock()
	writeJSON(w, map[string]string{"url": "/term/" + sid + "?k=" + key, "sid": sid})
}

func (b *Broker) apiDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !b.st.Delete(id) {
		http.Error(w, "no such machine", http.StatusNotFound)
		return
	}
	b.mu.Lock()
	ac := b.agents[id]
	b.mu.Unlock()
	if ac != nil {
		b.dropAgent(ac, "machine revoked")
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ---- connections ----

var upgrader = websocket.Upgrader{
	ReadBufferSize:  16 * 1024,
	WriteBufferSize: 16 * 1024,
	// Same-origin only matters once fronted with TLS; the admin/session
	// secrets, not the origin, gate every socket here.
	CheckOrigin: func(*http.Request) bool { return true },
}

// conn wraps a websocket with a bounded outbound queue drained by one
// writer goroutine, so a slow peer can never block a relay loop.
type conn struct {
	ws     *websocket.Conn
	out    chan proto.Msg
	mu     sync.Mutex
	closed bool
}

func newConn(ws *websocket.Conn) *conn {
	c := &conn{ws: ws, out: make(chan proto.Msg, queueLen)}
	go func() {
		for m := range c.out {
			c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if c.ws.WriteJSON(m) != nil {
				break
			}
		}
		c.ws.Close()
	}()
	return c
}

// send enqueues without blocking. false = queue full or connection closed;
// the caller decides whether that is fatal for the session.
func (c *conn) send(m proto.Msg) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	select {
	case c.out <- m:
		return true
	default:
		return false
	}
}

// close optionally enqueues a final frame, then closes the queue; the
// writer goroutine flushes and closes the socket.
func (c *conn) close(final *proto.Msg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if final != nil {
		select {
		case c.out <- *final:
		default:
		}
	}
	close(c.out)
}

type agentConn struct {
	*conn
	mid string
}

type session struct {
	*conn
	sid string
	mid string
}

// ---- /ws/agent ----

func (b *Broker) wsAgent(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	// First frame must be hello with a known token.
	ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var hello proto.Msg
	if err := ws.ReadJSON(&hello); err != nil || hello.T != "hello" {
		ws.Close()
		return
	}
	m, ok := b.st.ByToken(hello.Token)
	if !ok {
		ws.Close()
		return
	}
	b.st.Touch(m.ID, hello.Name, hello.Ver)

	// Register, replacing any zombie socket for the same machine; the
	// zombie's sessions die with a banner, the new socket keeps the id.
	ac := &agentConn{conn: newConn(ws), mid: m.ID}
	b.mu.Lock()
	old := b.agents[m.ID]
	b.agents[m.ID] = ac
	var victims []*session
	if old != nil {
		for sid, s := range b.sess {
			if s.mid == m.ID {
				delete(b.sess, sid)
				victims = append(victims, s)
			}
		}
	}
	b.mu.Unlock()
	if old != nil {
		old.close(nil)
	}
	for _, s := range victims {
		s.close(&proto.Msg{T: "err", Msg: "agent reconnected"})
	}
	ac.send(proto.Msg{T: "hello_ok", ID: m.ID})
	log.Printf("broker: agent %s (%s) connected", m.ID, m.Name)

	for {
		ws.SetReadDeadline(time.Now().Add(agentRead)) // hb every 10 s; 35 s = dead socket
		var f proto.Msg
		if err := ws.ReadJSON(&f); err != nil {
			break
		}
		switch f.T {
		case "hb":
			b.st.Touch(m.ID, "", "")
		case "out":
			b.toBrowser(ac, f.Sid, proto.Msg{T: "out", Data: f.Data})
		case "exit", "err": // err: v1e agent killed the session (GCM auth failure)
			b.mu.Lock()
			s := b.sess[f.Sid]
			if s != nil && s.mid != ac.mid { // not this agent's session to end
				s = nil
			} else {
				delete(b.sess, f.Sid)
			}
			b.mu.Unlock()
			if s != nil {
				if f.T == "err" {
					s.close(&proto.Msg{T: "err", Msg: f.Msg})
				} else {
					s.close(&proto.Msg{T: "exit", Code: f.Code})
				}
			}
		}
	}
	b.dropAgent(ac, "agent went offline")
	log.Printf("broker: agent %s (%s) disconnected", m.ID, m.Name)
}

// toBrowser routes agent output to the attached browser. A browser whose
// bounded queue is full gets its session dropped rather than stalling the
// shared agent socket (requirement: never block the relay).
func (b *Broker) toBrowser(ac *agentConn, sid string, m proto.Msg) {
	b.mu.Lock()
	s := b.sess[sid]
	b.mu.Unlock()
	// s.mid check: an enrolled agent must not be able to write into a session
	// that belongs to a different machine, even though sids are 64 random bits
	// and only ever sent to the owning agent.
	if s == nil || s.mid != ac.mid {
		return
	}
	if !s.send(m) {
		b.endSession(s, &proto.Msg{T: "err", Msg: "session dropped: browser too slow"}, true)
	}
}

// dropAgent removes a live agent socket and kills its sessions with an
// err banner in every attached browser.
func (b *Broker) dropAgent(ac *agentConn, why string) {
	b.mu.Lock()
	var victims []*session
	if b.agents[ac.mid] == ac { // a replaced zombie must not kill its successor's sessions
		delete(b.agents, ac.mid)
		for sid, s := range b.sess {
			if s.mid == ac.mid {
				delete(b.sess, sid)
				victims = append(victims, s)
			}
		}
	}
	b.mu.Unlock()
	ac.close(nil)
	for _, s := range victims {
		s.close(&proto.Msg{T: "err", Msg: why})
	}
}

// endSession detaches a browser and tells the agent to kill the PTY.
func (b *Broker) endSession(s *session, final *proto.Msg, notifyAgent bool) {
	b.mu.Lock()
	if b.sess[s.sid] == s {
		delete(b.sess, s.sid)
	}
	ac := b.agents[s.mid]
	b.mu.Unlock()
	s.close(final)
	if notifyAgent && ac != nil {
		ac.send(proto.Msg{T: "close", Sid: s.sid})
	}
}

// ---- /ws/term/{sid} ----

func (b *Broker) wsTerm(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	keySum := sha256.Sum256([]byte(r.URL.Query().Get("k")))

	// Consume the one-time key: first valid upgrade wins, later ones lose.
	b.mu.Lock()
	p, ok := b.pend[sid]
	valid := ok && time.Now().Before(p.exp) &&
		subtle.ConstantTimeCompare(keySum[:], p.keySum[:]) == 1
	if valid {
		delete(b.pend, sid)
	}
	ac := b.agents[p.mid]
	b.mu.Unlock()
	if !valid {
		http.Error(w, "invalid, expired or already-used session key", http.StatusForbidden)
		return
	}

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	// Register before sending open, or the first PTY bytes can race the
	// session map and vanish.
	s := &session{conn: newConn(ws), sid: sid, mid: p.mid}
	b.mu.Lock()
	b.sess[sid] = s
	b.mu.Unlock()
	if ac == nil || !ac.send(proto.Msg{T: "open", Sid: sid, Cmd: p.cmd}) {
		b.endSession(s, &proto.Msg{T: "err", Msg: "agent is offline"}, false)
		return
	}

	for {
		var f proto.Msg
		if err := ws.ReadJSON(&f); err != nil {
			break
		}
		switch f.T {
		case "in":
			ac.send(proto.Msg{T: "in", Sid: sid, Data: f.Data})
		case "resize":
			ac.send(proto.Msg{T: "resize", Sid: sid, C: f.C, R: f.R})
		}
	}
	b.endSession(s, nil, true) // browser went away: kill the PTY
}
