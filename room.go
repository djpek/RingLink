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
	MaxRoomSize  = 40

	pkPunch   = 1
	pkPing    = 2
	pkPong    = 3
	pkVoice   = 4
	pkBye     = 5
	pkLinks   = 6 // who I can reach directly and how fast: [id u64, rtt ms u16]...
	pkRelay   = 7 // please pass this on: [target id u64][sealed packet from me]
	pkRelayed = 8 // passed on for someone: [sealed packet from them]

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

	links     map[uint64]time.Duration // who they reach directly (from their pkLinks)
	linksAt   time.Time
	lastRelay time.Time // last packet from them that came through someone else
	via       uint64    // relay we last used to reach them (0 = direct)
}

type MemberInfo struct {
	ID        uint64
	Name      string
	Self      bool
	Connected bool
	RTT       time.Duration
	Via       string // name of the person relaying for them ("" = direct)
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
	now := time.Now()
	for _, p := range ps {
		mi := MemberInfo{ID: p.id, Name: p.name, Connected: r.directLocked(p, now), RTT: p.rtt}
		if rel, cost := r.bestRelayLocked(p.id, now); rel != nil && (!mi.Connected || p.via == rel.id) {
			if !mi.Connected || cost+relayGain < p.rtt {
				mi.Connected, mi.RTT, mi.Via = true, cost, rel.name
			}
		}
		out = append(out, mi)
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

// testBlock lets tests pretend two members can't reach each other directly.
var testBlock func(fromPort, toPort int) bool

func (r *Room) send(a *net.UDPAddr, pkt []byte) {
	if a == nil {
		return
	}
	if testBlock != nil && testBlock(r.conn.LocalAddr().(*net.UDPAddr).Port, a.Port) {
		return
	}
	if n, err := r.conn.WriteToUDP(pkt, a); err == nil {
		r.bytesOut.Add(int64(n))
	}
}

// ---------- routing ----------
//
// Everyone sends to everyone directly when they can. When two people can't reach each
// other (some routers block it) or the direct path is much slower, packets go through
// whoever has the best ping to both. Relays only pass packets on as they are: they're
// sealed with the room key by the original sender, so the receiver still sees who
// sent them, and a relay never relays a relayed packet.

const (
	relayGain  = 40 * time.Millisecond // a relay must beat the direct path by this much
	linksFresh = 5 * time.Second
)

func (r *Room) directLocked(p *peer, now time.Time) bool {
	return p.addr != nil && now.Sub(p.lastRecv) < 10*time.Second
}

// bestRelayLocked: the directly connected member with the quickest path to id.
func (r *Room) bestRelayLocked(id uint64, now time.Time) (*peer, time.Duration) {
	var best *peer
	var bestCost time.Duration
	for _, q := range r.peers {
		if q.id == id || q.gone || !r.directLocked(q, now) || now.Sub(q.linksAt) > linksFresh {
			continue
		}
		d, ok := q.links[id]
		if !ok {
			continue
		}
		rtt := q.rtt
		if rtt == 0 {
			rtt = 100 * time.Millisecond
		}
		if c := rtt + d; best == nil || c < bestCost {
			best, bestCost = q, c
		}
	}
	return best, bestCost
}

type sendJob struct {
	a   *net.UDPAddr
	pkt []byte
}

// routeLocked: how to get a sealed packet to p (direct, through a relay, or not at all).
func (r *Room) routeLocked(p *peer, pkt []byte, now time.Time) (sendJob, bool) {
	direct := r.directLocked(p, now)
	rel, cost := r.bestRelayLocked(p.id, now)
	if rel != nil && (!direct || p.rtt > 0 && cost+relayGain < p.rtt) {
		p.via = rel.id
		w := make([]byte, 8+len(pkt))
		binary.BigEndian.PutUint64(w, p.id)
		copy(w[8:], pkt)
		return sendJob{rel.addr, r.seal(pkRelay, w)}, true
	}
	p.via = 0
	if direct {
		return sendJob{p.addr, pkt}, true
	}
	return sendJob{}, false
}

// broadcast sends one packet to every connected member.
func (r *Room) broadcast(t byte, payload []byte) { r.broadcastSkip(t, payload, nil) }

func (r *Room) broadcastSkip(t byte, payload []byte, skip func(uint64) bool) {
	pkt := r.seal(t, payload)
	now := time.Now()
	r.mu.Lock()
	var jobs []sendJob
	for _, p := range r.peers {
		if p.gone || skip != nil && skip(p.id) {
			continue
		}
		if j, ok := r.routeLocked(p, pkt, now); ok {
			jobs = append(jobs, j)
		}
	}
	r.mu.Unlock()
	for _, j := range jobs {
		r.send(j.a, j.pkt)
	}
}

// SendVoice sends a voice payload to everyone in the room.
func (r *Room) SendVoice(p []byte) { r.SendVoiceSkip(p, nil) }

// SendVoiceSkip sends to everyone except those skip says no to (too far away to hear).
func (r *Room) SendVoiceSkip(p []byte, skip func(uint64) bool) {
	if !r.closed.Load() {
		r.broadcastSkip(pkVoice, p, skip)
	}
}

// Size: how many people are in the room, you included.
func (r *Room) Size() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 1
	for _, p := range r.peers {
		if !p.gone {
			n++
		}
	}
	return n
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
		r.bytesIn.Add(int64(n))
		r.handle(b, from, 0)
	}
}

// handle one sealed packet. via is the member who relayed it (0 = it came directly).
func (r *Room) handle(b []byte, from *net.UDPAddr, via uint64) {
	ns := r.aead.NonceSize()
	if len(b) < ns+9+r.aead.Overhead() {
		return
	}
	pt, err := r.aead.Open(nil, b[:ns], b[ns:], nil)
	if err != nil || len(pt) < 9 {
		return
	}
	t := pt[0]
	id := binary.BigEndian.Uint64(pt[1:9])
	payload := pt[9:]
	if id == r.ID {
		return
	}
	if via != 0 && t != pkVoice && t != pkBye {
		return // only voice (and goodbyes) travel through relays
	}
	r.mu.Lock()
	p := r.peers[id]
	if p == nil || (p.gone && t != pkBye && time.Since(p.lastAnn) > peerTimeout) {
		// not announced in this room (yet): ignore until their announce arrives
		r.mu.Unlock()
		return
	}
	first := false
	wasGone := p.gone
	if t == pkBye {
		p.gone = true
	} else if via != 0 {
		first = wasGone || time.Since(p.lastRelay) > 15*time.Second && !r.directLocked(p, time.Now())
		p.gone = false
		p.lastRelay = time.Now()
	} else {
		first = p.addr == nil || time.Since(p.lastRecv) > 15*time.Second
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
		return
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
	case pkLinks:
		m := map[uint64]time.Duration{}
		for o := 0; o+10 <= len(payload); o += 10 {
			m[binary.BigEndian.Uint64(payload[o:])] = time.Duration(binary.BigEndian.Uint16(payload[o+8:])) * time.Millisecond
		}
		r.mu.Lock()
		p.links, p.linksAt = m, time.Now()
		r.mu.Unlock()
	case pkRelay: // pass a sealed packet on to someone we reach directly
		if len(payload) > 8 {
			r.mu.Lock()
			q := r.peers[binary.BigEndian.Uint64(payload)]
			var to *net.UDPAddr
			if q != nil && !q.gone && q.id != id && r.directLocked(q, time.Now()) {
				to = q.addr
			}
			r.mu.Unlock()
			if to != nil {
				r.send(to, r.seal(pkRelayed, payload[8:]))
			}
		}
	case pkRelayed:
		if via == 0 {
			r.handle(payload, from, id)
		}
	}
	if first {
		if via != 0 {
			r.log("Connected to %s (through %s).", name, r.nameOf(via))
		} else {
			r.log("Connected to %s.", name)
		}
		r.changed()
	}
}

func (r *Room) nameOf(id uint64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.peers[id]; p != nil {
		return p.name
	}
	return "someone"
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
				// nobody answering, not reachable through anyone and no recent announce: they've gone
				rel, _ := r.bestRelayLocked(id, time.Now())
				if rel == nil && time.Since(p.lastRecv) > peerTimeout && time.Since(p.lastRelay) > peerTimeout && time.Since(p.lastAnn) > peerTimeout {
					p.gone = true
					changed = true
					r.log("Lost connection to %s.", p.name)
				}
			} else if ping != nil {
				jobs = append(jobs, job{p.addr, ping})
			}
		}
		if n%5 == 0 { // once a second: tell everyone who we reach directly, so they can route through us
			now := time.Now()
			var lb []byte
			var to []*net.UDPAddr
			for id, p := range r.peers {
				if p.gone || !r.directLocked(p, now) {
					continue
				}
				var e [10]byte
				binary.BigEndian.PutUint64(e[:], id)
				ms := p.rtt.Milliseconds()
				if ms <= 0 || ms > 65535 {
					ms = 500
				}
				binary.BigEndian.PutUint16(e[8:], uint16(ms))
				lb = append(lb, e[:]...)
				to = append(to, p.addr)
			}
			if len(to) > 0 {
				pkt := r.seal(pkLinks, lb)
				for _, a := range to {
					jobs = append(jobs, job{a, pkt})
				}
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
