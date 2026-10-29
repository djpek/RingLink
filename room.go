package main

// A room: everyone who typed the same short code. Every member connects
// directly to every other member (a small mesh) over one UDP socket, using
// UDP hole punching. Addresses are swapped through the signaling relay
// (signal.go); everything is encrypted with a key derived from the room code.

import (
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32: no I, L, O, U
	codeLen      = 8
	MaxRoomSize  = 8

	pkPunch = 1
	pkPing  = 2
	pkPong  = 3
	pkVoice = 4
	pkBye   = 5

	announceEvery = 4 * time.Minute
	announceFresh = 6 * time.Minute // older announces are ignored when joining
	peerTimeout   = 30 * time.Second
)

// ---------- room codes ----------

func NewRoomCode() string {
	b := make([]byte, codeLen)
	rand.Read(b)
	out := make([]byte, codeLen)
	for i := range b {
		out[i] = codeAlphabet[int(b[i])%32]
	}
	return FormatCode(string(out))
}

// NormalizeCode accepts what people actually type: lower case, spaces, dashes,
// O for 0 and I/L for 1.
func NormalizeCode(s string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		case r == 'U':
			r = 'V'
		case r == ' ' || r == '-' || r == '_' || r == '\t':
			continue
		}
		if !strings.ContainsRune(codeAlphabet, r) {
			return "", fmt.Errorf("%q isn't used in room codes", string(r))
		}
		b.WriteRune(r)
	}
	if b.Len() != codeLen {
		return "", fmt.Errorf("room codes have %d characters (got %d)", codeLen, b.Len())
	}
	return b.String(), nil
}

func FormatCode(c string) string {
	if len(c) == codeLen {
		return c[:4] + "-" + c[4:]
	}
	return c
}

// roomSecrets derives the encryption key and the relay topic from the code.
// Argon2 makes guessing codes from captured relay messages slow.
func roomSecrets(code string) (key []byte, topic string) {
	key = argon2.IDKey([]byte(code), []byte("ringlink-room-v2"), 2, 32*1024, 2, 32)
	h := sha256.Sum256(append([]byte("ringlink-topic-v2/"), key...))
	return key, "rl2_" + hex.EncodeToString(h[:12])
}

// ---------- types ----------

type announce struct {
	Kind   string `json:"k"`           // "a" announce, "b" bye
	ID     string `json:"id"`          // member id (hex)
	Name   string `json:"n,omitempty"` // display name
	Pub    string `json:"p,omitempty"` // public UDP address (from STUN)
	LAN    string `json:"l,omitempty"` // LAN UDP address
	Max    int    `json:"m,omitempty"` // room size limit (set by whoever made the room)
	Joined int64  `json:"j,omitempty"` // unix ms when this member joined
	T      int64  `json:"t"`           // unix ms when sent
}

type peer struct {
	id       uint64
	name     string
	cands    []*net.UDPAddr
	addr     *net.UDPAddr // confirmed address (we got a packet from it)
	lastRecv time.Time
	lastAnn  time.Time
	joined   int64
	rtt      time.Duration
	gone     bool
}

type MemberInfo struct {
	ID        uint64
	Name      string
	Self      bool
	Connected bool
	RTT       time.Duration
}

type Room struct {
	Code    string // formatted, e.g. K7QM-29XD
	ID      uint64
	Name    string
	Creator bool

	aead  cipher.AEAD
	topic string
	sig   Signaler
	conn  *net.UDPConn
	stun  chan []byte
	logf  func(string, ...any)

	OnVoice  func(from uint64, payload []byte)
	OnChange func()

	mu        sync.Mutex
	max       int
	joined    int64
	pub, lan  *net.UDPAddr
	symmetric bool
	peers     map[uint64]*peer
	state     string
	lastAnn   time.Time
	ctx       context.Context
	cancel    context.CancelFunc
	closed    atomic.Bool
	bytesIn   atomic.Int64
	bytesOut  atomic.Int64
}

// OpenRoom creates (creator=true) or joins a room. sig may be nil to use the
// public relay. It returns quickly; connecting happens in the background.
func OpenRoom(code, name string, max int, creator bool, sig Signaler, logf func(string, ...any)) (*Room, error) {
	c, err := NormalizeCode(code)
	if err != nil {
		return nil, err
	}
	key, topic := roomSecrets(c)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	if sig == nil {
		sig = newNtfySignal(topic, logf)
	}
	var idb [8]byte
	rand.Read(idb[:])
	if max < 2 || max > MaxRoomSize {
		max = MaxRoomSize
	}
	r := &Room{Code: FormatCode(c), ID: binary.BigEndian.Uint64(idb[:]), Name: name, Creator: creator,
		aead: aead, topic: topic, sig: sig, conn: conn, stun: make(chan []byte, 8), logf: logf,
		max: max, joined: time.Now().UnixMilli(), peers: map[uint64]*peer{}, state: "starting"}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	go r.readLoop()
	go r.run()
	return r, nil
}

func (r *Room) setState(s string) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
	r.changed()
}

func (r *Room) changed() {
	if r.OnChange != nil {
		r.OnChange()
	}
}

func (r *Room) State() (state string, max int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, r.max
}

func (r *Room) Members() []MemberInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []MemberInfo{{ID: r.ID, Name: r.Name, Self: true, Connected: true}}
	var ps []*peer
	for _, p := range r.peers {
		if !p.gone {
			ps = append(ps, p)
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].joined < ps[j].joined })
	for _, p := range ps {
		out = append(out, MemberInfo{ID: p.id, Name: p.name, Connected: p.addr != nil && time.Since(p.lastRecv) < 10*time.Second, RTT: p.rtt})
	}
	return out
}

func (r *Room) Traffic() (in, out int64) { return r.bytesIn.Load(), r.bytesOut.Load() }

// ---------- lifecycle ----------

func (r *Room) run() {
	// 1. find our addresses
	r.setState("finding your internet address")
	pub, sym, err := stunDiscover(func(b []byte, a *net.UDPAddr) { r.conn.WriteToUDP(b, a) }, r.stun)
	lanIP := localIPv4()
	r.mu.Lock()
	if err == nil {
		r.pub, r.symmetric = pub, sym
	}
	if lanIP != nil {
		r.lan = &net.UDPAddr{IP: lanIP, Port: r.conn.LocalAddr().(*net.UDPAddr).Port}
	}
	r.mu.Unlock()
	if err != nil {
		r.log("Couldn't find your internet address (%v) - only people on your home network can connect.", err)
	} else if sym {
		r.log("Your router changes ports for every connection (symmetric NAT); connecting to people outside your network may not work.")
	}

	// 2. listen on the relay (also replays recent announces from people already in the room)
	go r.sig.Subscribe(r.ctx, r.onSignal)
	go r.tickLoop()

	if !r.Creator {
		r.setState("looking for the room")
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(4 * time.Second):
		}
		r.mu.Lock()
		n := 0
		for _, p := range r.peers {
			if !p.gone {
				n++
			}
		}
		full := n >= r.max
		max := r.max
		r.mu.Unlock()
		if full {
			r.log("This room is full (%d people max).", max)
			r.setState(fmt.Sprintf("room is full (%d max)", max))
			r.cancel()
			return
		}
		if n == 0 {
			r.log("Nobody is in this room right now - waiting. Check the code if this doesn't change.")
		}
	}
	r.announce()
	r.setState("in room")
	r.keepAddress()
}

// keepAddress re-checks our public address every 25 s. The STUN traffic also
// keeps the router's port mapping open while nobody else is in the room, so
// the address we announced stays valid for people who join later.
func (r *Room) keepAddress() {
	if len(stunServers) == 0 {
		return
	}
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(25 * time.Second):
		}
		pub, _, err := stunDiscover(func(b []byte, a *net.UDPAddr) { r.conn.WriteToUDP(b, a) }, r.stun)
		if err != nil {
			continue
		}
		r.mu.Lock()
		changed := r.pub == nil || r.pub.String() != pub.String()
		r.pub = pub
		r.mu.Unlock()
		if changed {
			r.log("Your internet address changed - telling the room.")
			r.announce()
		}
	}
}

func (r *Room) Leave() {
	if r.closed.Swap(true) {
		return
	}
	b := r.sealAnnounce(announce{Kind: "b", ID: idHex(r.ID), T: time.Now().UnixMilli()})
	r.broadcast(pkBye, nil)
	done := make(chan struct{})
	go func() { r.sig.Publish(b); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	r.cancel()
	r.conn.Close()
}

func (r *Room) log(f string, v ...any) {
	if r.logf != nil {
		r.logf(f, v...)
	}
}

// ---------- signaling ----------

func idHex(id uint64) string { return fmt.Sprintf("%016x", id) }

func (r *Room) sealAnnounce(a announce) []byte {
	js, _ := json.Marshal(a)
	nonce := make([]byte, r.aead.NonceSize())
	rand.Read(nonce)
	return []byte(base64.RawURLEncoding.EncodeToString(r.aead.Seal(nonce, nonce, js, []byte("announce"))))
}

func (r *Room) openAnnounce(msg []byte) (announce, bool) {
	var a announce
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(msg)))
	ns := r.aead.NonceSize()
	if err != nil || len(raw) < ns+r.aead.Overhead() {
		return a, false
	}
	pt, err := r.aead.Open(nil, raw[:ns], raw[ns:], []byte("announce"))
	if err != nil || json.Unmarshal(pt, &a) != nil {
		return a, false
	}
	return a, true
}

func (r *Room) announce() {
	if r.closed.Load() {
		return
	}
	r.mu.Lock()
	a := announce{Kind: "a", ID: idHex(r.ID), Name: r.Name, Max: r.max, Joined: r.joined, T: time.Now().UnixMilli()}
	if r.pub != nil {
		a.Pub = r.pub.String()
	}
	if r.lan != nil {
		a.LAN = r.lan.String()
	}
	r.lastAnn = time.Now()
	r.mu.Unlock()
	go func() {
		if err := r.sig.Publish(r.sealAnnounce(a)); err != nil {
			r.log("Couldn't reach the room relay (%v).", err)
		}
	}()
}

func (r *Room) onSignal(msg []byte) {
	a, ok := r.openAnnounce(msg)
	if !ok {
		return
	}
	var id uint64
	if _, err := fmt.Sscanf(a.ID, "%x", &id); err != nil || id == r.ID {
		return
	}
	at := time.UnixMilli(a.T)
	if time.Since(at) > announceFresh {
		return
	}
	r.mu.Lock()
	p := r.peers[id]
	if a.Kind == "b" {
		if p != nil && !p.gone {
			p.gone = true
			r.mu.Unlock()
			r.log("%s left the room.", p.name)
			r.changed()
			return
		}
		r.mu.Unlock()
		return
	}
	isNew := p == nil || p.gone
	if p == nil {
		p = &peer{id: id}
		r.peers[id] = p
	}
	if p.lastAnn.After(at) { // older than what we know
		r.mu.Unlock()
		return
	}
	p.gone = false
	p.name = a.Name
	p.joined = a.Joined
	p.lastAnn = at
	p.cands = nil
	for _, s := range []string{a.Pub, a.LAN} {
		if u, err := net.ResolveUDPAddr("udp4", s); err == nil && s != "" {
			p.cands = append(p.cands, u)
		}
	}
	// the room limit is whatever the earliest member says
	if a.Max >= 2 && a.Joined < r.joined && !r.Creator {
		r.max = a.Max
	}
	reannounce := isNew && r.lastAnn.Before(time.Now().Add(-20*time.Second)) && r.state == "in room"
	r.mu.Unlock()
	if isNew {
		r.log("%s is in the room - connecting...", a.Name)
		r.changed()
	}
	if reannounce {
		r.announce() // so the newcomer gets our current address
	}
}

// ---------- UDP ----------

func (r *Room) seal(t byte, payload []byte) []byte {
	ns := r.aead.NonceSize()
	pt := make([]byte, 9+len(payload))
	pt[0] = t
	binary.BigEndian.PutUint64(pt[1:], r.ID)
	copy(pt[9:], payload)
	nonce := make([]byte, ns, ns+len(pt)+r.aead.Overhead())
	rand.Read(nonce)
	return r.aead.Seal(nonce, nonce, pt, nil)
}

func (r *Room) send(a *net.UDPAddr, pkt []byte) {
	if a == nil {
		return
	}
	if n, err := r.conn.WriteToUDP(pkt, a); err == nil {
		r.bytesOut.Add(int64(n))
	}
}

// broadcast sends one packet to every connected member.
func (r *Room) broadcast(t byte, payload []byte) {
	pkt := r.seal(t, payload)
	r.mu.Lock()
	var to []*net.UDPAddr
	for _, p := range r.peers {
		if !p.gone && p.addr != nil && time.Since(p.lastRecv) < 15*time.Second {
			to = append(to, p.addr)
		}
	}
	r.mu.Unlock()
	for _, a := range to {
		r.send(a, pkt)
	}
}

// SendVoice sends a voice payload to everyone in the room.
func (r *Room) SendVoice(p []byte) {
	if !r.closed.Load() {
		r.broadcast(pkVoice, p)
	}
}

func (r *Room) readLoop() {
	buf := make([]byte, 65536)
	ns := r.aead.NonceSize()
	for {
		n, from, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if r.closed.Load() {
				return
			}
			continue
		}
		b := buf[:n]
		if isSTUN(b) {
			select {
			case r.stun <- append([]byte(nil), b...):
			default:
			}
			continue
		}
		if n < ns+9+r.aead.Overhead() {
			continue
		}
		pt, err := r.aead.Open(nil, b[:ns], b[ns:], nil)
		if err != nil || len(pt) < 9 {
			continue
		}
		r.bytesIn.Add(int64(n))
		t := pt[0]
		id := binary.BigEndian.Uint64(pt[1:9])
		payload := pt[9:]
		if id == r.ID {
			continue
		}
		r.mu.Lock()
		p := r.peers[id]
		if p == nil || (p.gone && t != pkBye && time.Since(p.lastAnn) > peerTimeout) {
			// not announced in this room (yet): ignore until their announce arrives
			r.mu.Unlock()
			continue
		}
		first := p.addr == nil || time.Since(p.lastRecv) > 15*time.Second
		wasGone := p.gone
		if t == pkBye {
			p.gone = true
		} else {
			p.gone = false
			p.addr = from
			p.lastRecv = time.Now()
		}
		name := p.name
		r.mu.Unlock()
		switch t {
		case pkBye:
			if !wasGone {
				r.log("%s left the room.", name)
				r.changed()
			}
			continue
		case pkPing:
			r.send(from, r.seal(pkPong, payload))
		case pkPong:
			if len(payload) == 8 {
				d := time.Duration(time.Now().UnixNano() - int64(binary.BigEndian.Uint64(payload)))
				r.mu.Lock()
				p.rtt = d
				r.mu.Unlock()
			}
		case pkVoice:
			if r.OnVoice != nil {
				r.OnVoice(id, payload)
			}
		}
		if first {
			r.log("Connected to %s.", name)
			r.changed()
		}
	}
}

func (r *Room) tickLoop() {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-tick.C:
		}
		n++
		punch := r.seal(pkPunch, nil)
		var ping []byte
		if n%5 == 0 {
			var ts [8]byte
			binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixNano()))
			ping = r.seal(pkPing, ts[:])
		}
		type job struct {
			a   *net.UDPAddr
			pkt []byte
		}
		var jobs []job
		changed := false
		r.mu.Lock()
		for id, p := range r.peers {
			if p.gone {
				if time.Since(p.lastAnn) > announceFresh && time.Since(p.lastRecv) > announceFresh {
					delete(r.peers, id)
				}
				continue
			}
			alive := p.addr != nil && time.Since(p.lastRecv) < 8*time.Second
			if !alive {
				for _, c := range p.cands {
					jobs = append(jobs, job{c, punch})
				}
				if p.addr != nil {
					jobs = append(jobs, job{p.addr, punch})
				}
				// nobody answering and no recent announce: they've gone
				if time.Since(p.lastRecv) > peerTimeout && time.Since(p.lastAnn) > peerTimeout {
					p.gone = true
					changed = true
					r.log("Lost connection to %s.", p.name)
				}
			} else if ping != nil {
				jobs = append(jobs, job{p.addr, ping})
			}
		}
		reann := r.state == "in room" && time.Since(r.lastAnn) > announceEvery
		r.mu.Unlock()
		for _, j := range jobs {
			r.send(j.a, j.pkt)
		}
		if changed {
			r.changed()
		}
		if reann {
			r.announce()
		}
	}
}

func localIPv4() net.IP {
	c, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.To4()
}
