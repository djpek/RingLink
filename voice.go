package main

// Proximity voice chat for any number of people.
//
// Audio: 16 kHz mono, 20 ms frames (320 samples), G.711 mu-law (128 kbit/s per
// talker). Frames go to everyone in the room as unreliable UDP packets: a lost
// frame is better than a late one.
//
// Proximity: everyone reads their own car position from LMU's local REST API
// (http://127.0.0.1:6397/rest/watch/standings, the entry with player=true) and
// sends it to the room a few times a second. Each person's voice is scaled and
// muffled by the distance between your car and theirs. When either position is
// unknown (menus, garage, different servers, LMU closed) they play at normal
// volume so you can always talk.

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	voiceRate    = 24000
	voiceFrame   = 480 // samples per 20 ms
	vkAudio      = 1   // old 16 kHz frames (320 samples), still understood
	vkAudio24    = 3   // 24 kHz frames (480 samples)
	legacyFrame  = 320
	vkPos        = 2
	vkSettings   = 4  // your "other people's voices" settings, so others can copy them (JSON)
	vkPresence   = 6  // [flags: 1 in garage, 2 broadcasting][sub-room name length][name]
	jitterTarget = 3  // frames buffered before a talker starts playing (60 ms)
	jitterMax    = 12 // drop old frames beyond this (240 ms)
	spkChannels  = 2  // stereo out, so voices can come from the left or right
)

// AudioDevice is a mic or output the user can pick. An empty ID means the
// system default.
type AudioDevice struct{ ID, Name string }

// audio backends implement these (audio_*.go)
type micSource interface {
	Read(frame []int16) error // blocks until a full frame is captured
	Close()
}
type speakerSink interface {
	Write(frame []int16) error // blocks to pace playback
	Close()
}

type vec3 struct{ X, Y, Z float64 }

// which game a position came from: distances only make sense within one game
const (
	gameNone   = 0
	gameLMU    = 1
	gameBeamNG = 2
)

func gameName(g byte) string {
	switch g {
	case gameLMU:
		return "Le Mans Ultimate"
	case gameBeamNG:
		return "BeamNG"
	}
	return ""
}

// one remote talker
type vpeer struct {
	queue    [][]int16
	priming  bool
	pos      *vec3
	game     byte
	posSeen  time.Time
	mt       motion  // their car's movement, for guessing where it is now
	userVol  float64 // your volume for this person (1 = 100 %), if userSet
	userSet  bool
	fullAt   time.Time // last full-quality voice packet from them
	lowAt    time.Time // last low-quality one (arrives 60 ms at a time: buffer a bit more)
	inGarage bool      // from their presence packets
	bcasting bool
	noRecord bool   // they opted out of being in other people's replays
	sub      string // sub-room they picked
	presAt   time.Time
	shared   *VoiceSettings // their voice settings, if they've sent them
	talkAt   time.Time
	gain     float64 // playback goroutine only
	lp1, lp2 float64
	lpA      float64
	pan      float64 // -1 left .. +1 right, playback goroutine only

	// Doppler (playback goroutine, under v.mu): resampler state and closing speed
	rs      []float64 // decoded samples not yet played
	ph      float64   // fractional read position in rs
	rate    float64   // current playback rate (1 = normal)
	lastD   float64
	lastDAt time.Time
	closing float64 // m/s, positive when the cars are getting closer
}

const soundSpeed = 343.0

// nextFrameLocked returns the next 20 ms of this peer's voice, played at rate r
// (r > 1 = higher pitch, consumes audio a little faster), or nil when they're silent.
func (p *vpeer) nextFrameLocked(r float64) []int16 {
	// at fast replay speeds each output frame eats several input frames, so buffer more
	extra := 0
	if time.Since(p.lowAt) < time.Second {
		extra = lowFrames - 1
	}
	if p.priming && len(p.queue) >= jitterTarget+int(r)-1+extra {
		p.priming = false
	}
	if p.priming {
		return nil
	}
	if r == 1 && p.ph == 0 && len(p.rs) == 0 { // no Doppler: frames straight through
		if len(p.queue) == 0 {
			p.priming = true // ran dry: rebuffer before their next sentence
			return nil
		}
		fr := p.queue[0]
		p.queue = p.queue[1:]
		return fr
	}
	need := int(p.ph+r*float64(voiceFrame-1)) + 2
	for len(p.rs) < need && len(p.queue) > 0 {
		for _, x := range p.queue[0] {
			p.rs = append(p.rs, float64(x))
		}
		p.queue = p.queue[1:]
	}
	if len(p.rs) < need {
		p.priming = true // keep what's buffered: it continues once more arrives
		return nil
	}
	out := make([]int16, voiceFrame)
	ph := p.ph
	for i := range out {
		k := int(ph)
		t := ph - float64(k)
		out[i] = int16(p.rs[k] + (p.rs[k+1]-p.rs[k])*t)
		ph += r
	}
	drop := int(ph)
	n := copy(p.rs, p.rs[drop:])
	p.rs, p.ph = p.rs[:n], ph-float64(drop)
	if p.rate == 1 && math.Abs(r-1) < 1e-9 && p.ph < 1e-6 && len(p.rs) == 0 {
		p.ph = 0
	}
	return out
}

// dopplerRateLocked tracks how fast this peer's car closes on yours and turns it
// into a playback rate. amount: 0 = off, 1 = physically realistic.
func (p *vpeer) dopplerRateLocked(d float64, now time.Time, amount float64) float64 {
	if d < 0 {
		p.closing *= 0.9
		p.lastDAt = time.Time{}
	} else if d != p.lastD {
		if !p.lastDAt.IsZero() {
			if dt := now.Sub(p.lastDAt).Seconds(); dt > 0.02 && dt < 1.5 {
				v := (p.lastD - d) / dt
				if v > 120 {
					v = 120
				} else if v < -120 {
					v = -120
				}
				p.closing = 0.6*p.closing + 0.4*v
			}
		}
		p.lastD, p.lastDAt = d, now
	} else if now.Sub(p.lastDAt) > 1500*time.Millisecond {
		p.closing *= 0.9 // positions stopped updating
	}
	target := 1.0
	if amount > 0 {
		target = 1 + amount*p.closing/soundSpeed
		target = math.Max(0.75, math.Min(1.33, target))
	}
	if p.rate == 0 {
		p.rate = 1
	}
	p.rate += (target - p.rate) * 0.15 // glide so pitch never jumps
	if amount == 0 && math.Abs(p.rate-1) < 0.002 {
		p.rate = 1
	}
	return p.rate
}

type Voice struct {
	send     func([]byte) // sends a voice payload to everyone in the room
	sendLowF func([]byte) // low-quality voice for far-away people
	logf     func(string, ...any)
	notify   func()

	mu        sync.Mutex
	running   bool
	stop      chan struct{}
	peers     map[uint64]*vpeer
	rpPeers   map[uint64]*vpeer // filter/pan state for replayed voices (playback goroutine)
	broadcast bool              // in the garage / watching a replay: you're heard at full volume, centred
	myPos     *vec3
	myGame    byte
	me        motion     // your car's movement
	myHead    [2]float64 // unit heading in the horizontal plane (from how the car moves)
	haveHead  bool
	headAt    [2]float64 // position the heading was last measured from
	micDev    string
	spkDev    string
	backend   string
	errText   string

	GameMode atomic.Int32 // gameNone = auto, gameLMU, gameBeamNG
	Muted    atomic.Bool
	Sens     atomic.Int32 // 0..100, higher = more sensitive
	VolPct   atomic.Int32 // everyone's volume, 0..200 %
	RangeM   atomic.Int32 // distance (m) where voices fade to silence
	Muffle   atomic.Int32 // 0..100: how muffled voices get with distance
	BaseMuff atomic.Int32 // 0..100: muffle applied to everyone all the time (helmet / radio feel)
	Doppler  atomic.Int32 // 0..100: pitch shift from closing speed (100 = realistic)
	Direct   atomic.Bool  // pan voices left/right by where their car is relative to yours
	Replay   *ReplayVoice // voice in game replays
	BcastBtn atomic.Bool  // "Broadcast" button switched on in the app
	BcastKey atomic.Bool  // push-to-broadcast key / wheel button held
	PTTMode  atomic.Bool  // push to talk instead of voice activated
	PTTKey   atomic.Bool  // push-to-talk key held
	PTMKey   atomic.Bool  // push-to-mute key held
	HostMute atomic.Bool  // the host or a mod muted you for everyone
	NoRecord atomic.Bool  // you opted out of your voice being saved in replays (yours and others')

	// room rules (from the host / mods) and where you are
	pitRadio bool   // garage = everyone at full volume (default on)
	isolate  uint64 // everyone hears only this member
	selfID   uint64
	inGarage bool
	mySub    string
	posKick  chan struct{}
	micLevel atomic.Int32 // 0..100 for the meter
	talking  atomic.Bool
	seq      uint16
	lastLoud time.Time
}

func NewVoice(logf func(string, ...any), notify func()) *Voice {
	v := &Voice{logf: logf, notify: notify, peers: map[uint64]*vpeer{}, Replay: NewReplayVoice(), posKick: make(chan struct{}, 1)}
	v.Replay.Run(make(chan struct{})) // session clock + replay position, for as long as the app runs
	v.pitRadio = true
	v.Sens.Store(60)
	v.VolPct.Store(100)
	v.RangeM.Store(150)
	v.Muffle.Store(50)
	v.Direct.Store(true)
	v.Doppler.Store(50)
	return v
}

// Broadcasting: everyone hears you at full volume, centred, wherever their car is.
func (v *Voice) Broadcasting() bool { return v.BcastBtn.Load() || v.BcastKey.Load() }

// KickPos sends your position (or "no position" while broadcasting) right away
// instead of waiting for the next 200 ms tick, so the first words already count.
func (v *Voice) KickPos() {
	select {
	case v.posKick <- struct{}{}:
	default:
	}
}

func (v *Voice) SetSender(f func([]byte)) {
	v.mu.Lock()
	v.send = f
	if f == nil {
		v.peers = map[uint64]*vpeer{}
	}
	v.mu.Unlock()
}

// cullDist: in big rooms your voice isn't sent to cars further than this (the longest
// "how far voices carry" setting is 500 m, so nobody could hear it anyway).
const (
	cullDist      = 600.0
	cullRoomAbove = 8 // rooms up to this size always get everything
)

// TooFar: whether member id is too far from you to hear you right now. Unknown
// positions (menus, garage, replays, other game) are never too far, and neither is
// anyone while you're broadcasting.
func (v *Voice) TooFar(id uint64) bool {
	if v.Broadcasting() {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.broadcast || v.myPos == nil {
		return false
	}
	p := v.peers[id]
	if p == nil || p.pos == nil || p.game != v.myGame || time.Since(p.posSeen) > 3*time.Second {
		return false
	}
	return v.distLocked(p) > cullDist
}

// SetRules applies the room's rules from the host / mods; selfID is your member id.
func (v *Voice) SetRules(pitRadio bool, isolate, selfID uint64) {
	v.mu.Lock()
	changed := v.pitRadio != pitRadio
	v.pitRadio, v.isolate, v.selfID = pitRadio, isolate, selfID
	v.mu.Unlock()
	if changed {
		v.KickPos()
	}
}

// SetSub picks your sub-room for the pits ("" = none).
func (v *Voice) SetSub(name string) {
	v.mu.Lock()
	v.mySub = name
	v.mu.Unlock()
	v.KickPos()
}

func (v *Voice) Sub() string { v.mu.Lock(); defer v.mu.Unlock(); return v.mySub }

// PeopleSubs: sub-rooms people in the room are in right now (anyone can make one by
// joining it), so they show up for everyone to pick.
func (v *Voice) PeopleSubs() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	if v.mySub != "" {
		out = append(out, v.mySub)
	}
	for _, p := range v.peers {
		if p.sub != "" && time.Since(p.presAt) < 3*time.Second {
			out = append(out, p.sub)
		}
	}
	return out
}

// PeerPresence: whether someone is in the garage and which sub-room they picked.
func (v *Voice) PeerPresence(id uint64) (inGarage bool, sub string, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	p := v.peers[id]
	if p == nil || time.Since(p.presAt) > 3*time.Second {
		return false, "", false
	}
	return p.inGarage, p.sub, true
}

// InGarage: you're in the garage right now (LMU).
func (v *Voice) InGarage() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.inGarage }

const isolateFade = 0.15 // everyone else's volume while someone is isolated

// peerGainLocked: how loud you hear member id, by the room's rules:
// isolate > broadcasting > sub-rooms in the pits > distance.
func (v *Voice) peerGainLocked(id uint64, p *vpeer, d float64) float64 {
	full := float64(v.VolPct.Load()) / 100
	g := v.gainFor(d)
	pres := time.Since(p.presAt) < 3*time.Second
	switch {
	case v.isolate != 0 && id == v.isolate:
		g = full
	case v.isolate != 0:
		g *= isolateFade
	case pres && p.bcasting && (p.sub == "" || strings.EqualFold(p.sub, v.mySub)):
		// broadcasting reaches everyone, or only their sub-room if they're in one (team
		// radio); people outside it hear them by distance as usual
		g = full
	case pres && p.bcasting && !v.inGarage:
		// you're on track, they're broadcasting to another sub-room: just distance
	case v.pitRadio && (v.inGarage && v.mySub != "" || pres && p.inGarage && p.sub != ""):
		// someone in the pits is in a sub-room: only people in the pits in the same
		// sub-room hear each other (on track it's back to everyone)
		if v.inGarage && pres && p.inGarage && strings.EqualFold(v.mySub, p.sub) {
			g = full
		} else {
			g = 0
		}
	}
	return g * p.volMul()
}

// SetLowSender: where low-quality packets go (the app sends them only to far-away
// people in big rooms; nil = nowhere).
func (v *Voice) SetLowSender(f func([]byte)) {
	v.mu.Lock()
	v.sendLowF = f
	v.mu.Unlock()
}

func (v *Voice) sendLow(p []byte) {
	v.mu.Lock()
	f := v.sendLowF
	v.mu.Unlock()
	if f != nil {
		f(p)
	}
}

func (v *Voice) sender() func([]byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.send
}

// SetDevices picks the mic and output ("" = system default). If voice is on,
// it restarts on the new devices.
func (v *Voice) SetDevices(mic, spk string) {
	v.mu.Lock()
	changed := v.micDev != mic || v.spkDev != spk
	v.micDev, v.spkDev = mic, spk
	running := v.running
	v.mu.Unlock()
	if changed && running {
		v.Stop()
		time.Sleep(150 * time.Millisecond)
		v.Start()
	}
}

func (v *Voice) Running() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.running
}

func (v *Voice) RemovePeer(id uint64) {
	v.mu.Lock()
	delete(v.peers, id)
	v.mu.Unlock()
}

// VoiceSettings: the "other people's voices" settings, shared so others can copy them.
type VoiceSettings struct {
	Vol      int  `json:"vol"`
	Range    int  `json:"range"`
	Muffle   int  `json:"muffle"`
	BaseMuff int  `json:"baseMuffle"`
	Dir      bool `json:"directional"`
	Doppler  int  `json:"doppler"`
}

func (v *Voice) mySettings() VoiceSettings {
	return VoiceSettings{int(v.VolPct.Load()), int(v.RangeM.Load()), int(v.Muffle.Load()), int(v.BaseMuff.Load()), v.Direct.Load(), int(v.Doppler.Load())}
}

// SetPeerVolume: your own volume for one person, in percent (100 = normal).
func (v *Voice) SetPeerVolume(id uint64, pct int) {
	v.mu.Lock()
	p := v.peerLocked(id)
	p.userVol, p.userSet = float64(pct)/100, true
	v.mu.Unlock()
}

// PeerSettings: the voice settings someone shared, if any.
func (v *Voice) PeerSettings(id uint64) *VoiceSettings {
	v.mu.Lock()
	defer v.mu.Unlock()
	if p := v.peers[id]; p != nil && p.shared != nil {
		c := *p.shared
		return &c
	}
	return nil
}

func (p *vpeer) volMul() float64 {
	if !p.userSet {
		return 1
	}
	return p.userVol
}

type PeerVoice struct {
	Talking bool
	Dist    float64 // -1 when unknown
	Volume  int     // percent you hear them at
	Muffled int     // 0..100
	OnTrack bool
	Game    string
}

type VoiceStatus struct {
	On, Talking  bool
	Level        int
	Backend, Err string
	HavePos      bool
	Game         string // game your position comes from
	Peers        map[uint64]PeerVoice
}

func (v *Voice) Status() VoiceStatus {
	v.mu.Lock()
	defer v.mu.Unlock()
	st := VoiceStatus{On: v.running, Talking: v.talking.Load(), Level: int(v.micLevel.Load()),
		Backend: v.backend, Err: v.errText, HavePos: v.myPos != nil, Game: gameName(v.myGame), Peers: map[uint64]PeerVoice{}}
	for id, p := range v.peers {
		d := v.distLocked(p)
		st.Peers[id] = PeerVoice{
			Talking: time.Since(p.talkAt) < 400*time.Millisecond,
			Dist:    d,
			Volume:  int(math.Round(v.peerGainLocked(id, p, d) * 100)),
			Muffled: int(math.Round(v.muffleFor(d) * 100)),
			OnTrack: p.pos != nil && time.Since(p.posSeen) < 3*time.Second,
			Game:    gameName(p.game),
		}
	}
	return st
}

func (v *Voice) distLocked(p *vpeer) float64 {
	if v.myPos == nil || p.pos == nil || p.game != v.myGame || time.Since(p.posSeen) > 3*time.Second {
		return -1
	}
	now := time.Now()
	a, b := v.mePosLocked(now), p.mt.now(now)
	if b == nil {
		b = p.pos
	}
	dx, dy, dz := a.X-b.X, a.Y-b.Y, a.Z-b.Z
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}

// farFrac: 0 when close (or position unknown), 1 at the edge of the range.
func farFrac(d, rng float64) float64 {
	near := rng * 0.1
	if d < 0 || d <= near {
		return 0
	}
	if d >= rng {
		return 1
	}
	return (d - near) / (rng - near)
}

// proximityGain maps car distance to playback volume (before the volume setting).
func proximityGain(d, rng float64) float64 {
	t := farFrac(d, rng)
	return (1 - t) * (1 - t) // falls off a bit faster than linear, like real sound
}

func (v *Voice) gainFor(d float64) float64 {
	return proximityGain(d, float64(v.RangeM.Load())) * float64(v.VolPct.Load()) / 100
}

// muffleFor: 0 = clear, 1 = as muffled as the Muffle setting allows at max.
func (v *Voice) muffleFor(d float64) float64 {
	m := farFrac(d, float64(v.RangeM.Load())) * float64(v.Muffle.Load()) / 100
	b := float64(v.BaseMuff.Load()) / 100
	return 1 - (1-m)*(1-b) // the constant muffle stacks with the distance one
}

// horiz: the two horizontal axes of a game's world (LMU is y-up, BeamNG is z-up)
func horiz(p *vec3, game byte) [2]float64 {
	if game == gameBeamNG {
		return [2]float64{p.X, p.Y}
	}
	return [2]float64{p.X, p.Z}
}

// dirLocked: where a peer is relative to your car. pan is -1 (left) .. +1 (right),
// behind is 0 (in front or beside) .. 1 (straight behind). Zero when unknown.
func (v *Voice) dirLocked(p *vpeer) (pan, behind float64) {
	if !v.haveHead || v.myPos == nil || p.pos == nil || p.game != v.myGame || time.Since(p.posSeen) > 3*time.Second {
		return 0, 0
	}
	now := time.Now()
	b := p.mt.now(now)
	if b == nil {
		b = p.pos
	}
	return v.dirFrom(v.mePosLocked(now), v.myHead, v.myGame, b)
}

// mePosLocked: where your car most likely is right now.
func (v *Voice) mePosLocked(now time.Time) *vec3 {
	if p := v.me.now(now); p != nil {
		return p
	}
	return v.myPos
}

// dirFrom: where src is relative to a listener at pos heading head.
// pan is -1 (left) .. +1 (right), behind is 0 (in front or beside) .. 1 (straight behind).
func (v *Voice) dirFrom(pos *vec3, head [2]float64, game byte, src *vec3) (pan, behind float64) {
	if !v.Direct.Load() || pos == nil || src == nil {
		return 0, 0
	}
	a, b := horiz(pos, game), horiz(src, game)
	dx, dz := b[0]-a[0], b[1]-a[1]
	l := math.Hypot(dx, dz)
	if l < 1 {
		return 0, 0
	}
	dx, dz = dx/l, dz/l
	// both games: facing +forward, right-hand side is (f.z, -f.x)
	pan = dx*head[1] - dz*head[0]
	if fw := dx*head[0] + dz*head[1]; fw < 0 {
		behind = -fw
	}
	return pan, behind
}

// trackHeading updates your heading from the car's movement (needs ~1.5 m of travel)
func (v *Voice) trackHeadingLocked(pos *vec3, game byte, prevGame byte) {
	if pos == nil {
		return
	}
	h := horiz(pos, game)
	if game != prevGame || !v.haveHead && v.headAt == [2]float64{} {
		v.headAt = h
		if game != prevGame {
			v.haveHead = false
		}
		return
	}
	dx, dz := h[0]-v.headAt[0], h[1]-v.headAt[1]
	l := math.Hypot(dx, dz)
	if l > 50 { // teleported (reset to pits etc.)
		v.headAt, v.haveHead = h, false
		return
	}
	if l >= 1.5 {
		nx, nz := dx/l, dz/l
		if v.haveHead { // smooth a little so wiggles don't swing voices around
			nx, nz = 0.6*nx+0.4*v.myHead[0], 0.6*nz+0.4*v.myHead[1]
			n := math.Hypot(nx, nz)
			nx, nz = nx/n, nz/n
		}
		v.myHead, v.haveHead, v.headAt = [2]float64{nx, nz}, true, h
	}
}

// cutoff for the low-pass filter: ~11 kHz (clear) down to ~250 Hz (behind a wall).
func muffleCutoff(m float64) float64 {
	return 11000 * math.Pow(250.0/11000, m)
}

func (v *Voice) Start() {
	v.mu.Lock()
	if v.running {
		v.mu.Unlock()
		return
	}
	v.running = true
	v.errText = ""
	v.stop = make(chan struct{})
	stop := v.stop
	v.mu.Unlock()

	v.mu.Lock()
	micDev, spkDev := v.micDev, v.spkDev
	v.mu.Unlock()
	mic, mname, err := openMic(micDev)
	if err != nil {
		v.fail("couldn't open the microphone: %v", err)
		return
	}
	spk, sname, err := openSpeaker(spkDev)
	if err != nil {
		mic.Close()
		v.fail("couldn't open the speakers: %v", err)
		return
	}
	v.mu.Lock()
	v.backend = mname + " / " + sname
	v.mu.Unlock()
	v.logf("Voice chat on (mic: %s, output: %s).", mname, sname)
	go v.captureLoop(mic, stop)
	go v.playLoop(spk, stop)
	go v.posLoop(stop)
	v.notify()
}

func (v *Voice) fail(f string, a ...any) {
	msg := fmt.Sprintf(f, a...)
	v.mu.Lock()
	v.running = false
	v.errText = msg
	v.mu.Unlock()
	v.logf("Voice chat: %s", msg)
	v.notify()
}

func (v *Voice) Stop() {
	v.mu.Lock()
	if !v.running {
		v.mu.Unlock()
		return
	}
	v.running = false
	close(v.stop)
	for _, p := range v.peers {
		p.queue = nil
	}
	v.mu.Unlock()
	v.micLevel.Store(0)
	v.talking.Store(false)
	v.logf("Voice chat off.")
	v.notify()
}

// ---------- capture ----------

func (v *Voice) captureLoop(mic micSource, stop chan struct{}) {
	defer mic.Close()
	frame := make([]int16, voiceFrame)
	pkt := make([]byte, 3+voiceFrame)
	noise := 200.0 // running noise-floor estimate (RMS)
	// the last couple of quiet frames, sent when the gate opens so word starts aren't clipped
	var pre [][]int16
	var lowEnc lowEncoder // the same voice in low quality, for people far away in big rooms
	wasTalking := false
	var pttAt time.Time
	for {
		select {
		case <-stop:
			return
		default:
		}
		if err := mic.Read(frame); err != nil {
			select {
			case <-stop:
				return
			default:
			}
			v.fail("microphone stopped: %v", err)
			return
		}
		var sum float64
		for _, s := range frame {
			f := float64(s)
			sum += f * f
		}
		rms := math.Sqrt(sum / voiceFrame)
		lvl := int32(math.Min(100, math.Max(0, (20*math.Log10(rms+1)-20)*100/70)))
		v.micLevel.Store(lvl)
		// adaptive gate: open when clearly above the noise floor
		if rms < noise*1.5 {
			noise = noise*0.98 + rms*0.02
		} else {
			noise = noise*0.999 + rms*0.001
		}
		if noise < 30 {
			noise = 30
		}
		sens := float64(v.Sens.Load())           // 0..100
		thr := math.Max(noise*2.5, 1500-sens*14) // 100 -> ~100, 0 -> 1500
		if rms > thr {
			v.lastLoud = time.Now()
		}
		var talking bool
		if v.PTTMode.Load() {
			// push to talk: while the key is held (holding broadcast talks too), plus a
			// short tail so the last word isn't clipped
			if v.PTTKey.Load() || v.BcastKey.Load() {
				pttAt = time.Now()
			}
			talking = time.Since(pttAt) < 150*time.Millisecond
		} else {
			talking = time.Since(v.lastLoud) < 350*time.Millisecond
		}
		talking = talking && !v.Muted.Load() && !v.PTMKey.Load() && !v.HostMute.Load()
		v.talking.Store(talking)
		if !talking {
			if wasTalking { // stopped talking: send the rest of the low-quality packet
				if low := lowEnc.flush(); low != nil {
					v.sendLow(low)
				}
			}
			wasTalking = false
			pre = append(pre, append([]int16(nil), frame...))
			if len(pre) > 2 {
				pre = pre[1:]
			}
			continue
		}
		send := v.sender()
		if send == nil {
			continue
		}
		out := [][]int16{frame}
		if !wasTalking {
			out = append(pre, frame)
			pre = nil
		}
		wasTalking = true
		for _, fr := range out {
			pkt[0] = vkAudio24
			binary.BigEndian.PutUint16(pkt[1:], v.seq)
			v.seq++
			for i, s := range fr {
				pkt[3+i] = linearToULaw(s)
			}
			send(pkt)
			if low := lowEnc.add(fr); low != nil {
				v.sendLow(low)
			}
			if v.Replay.Enabled() && !v.NoRecord.Load() {
				v.mu.Lock()
				mp := v.myPos
				v.mu.Unlock()
				v.Replay.Record(0, pkt[3:], mp)
			}
		}
	}
}

// ---------- receive / playback ----------

func (v *Voice) peerLocked(id uint64) *vpeer {
	p := v.peers[id]
	if p == nil {
		p = &vpeer{priming: true, gain: 1}
		v.peers[id] = p
	}
	return p
}

// onFrames: decoded 20 ms frames from someone: play them and save them for replays.
func (v *Voice) onFrames(from uint64, frs [][]int16) {
	for _, fr := range frs {
		loud := false
		for _, x := range fr {
			if x > 2000 || x < -2000 {
				loud = true
				break
			}
		}
		v.mu.Lock()
		p := v.peerLocked(from)
		if loud {
			p.talkAt = time.Now()
		}
		// saved for replays whether or not you can hear it right now (voice off, too far
		// away), unless they've opted out of being recorded
		if v.Replay.Enabled() && !p.noRecord {
			ul := make([]byte, voiceFrame)
			for i, x := range fr {
				ul[i] = linearToULaw(x)
			}
			var pp *vec3
			if p.pos != nil && p.game == gameLMU && time.Since(p.posSeen) < 3*time.Second {
				if pp = p.mt.now(time.Now()); pp == nil {
					pp = p.pos
				}
			}
			v.Replay.Record(from, ul, pp)
		}
		if v.running {
			p.queue = append(p.queue, fr)
			if len(p.queue) > jitterMax {
				p.queue = p.queue[len(p.queue)-jitterTarget:]
			}
		}
		v.mu.Unlock()
	}
}

// OnPacket handles a voice payload from someone in the room.
func (v *Voice) OnPacket(from uint64, b []byte) {
	if len(b) < 1 {
		return
	}
	switch b[0] {
	case vkAudio, vkAudio24:
		var fr []int16
		switch {
		case b[0] == vkAudio24 && len(b) == 3+voiceFrame:
			fr = make([]int16, voiceFrame)
			for i := range fr {
				fr[i] = uLawToLinear(b[3+i])
			}
		case b[0] == vkAudio && len(b) == 3+legacyFrame: // older RingLink: 16 kHz, upsample x1.5
			src := make([]float64, legacyFrame+1)
			for i := 0; i < legacyFrame; i++ {
				src[i] = float64(uLawToLinear(b[3+i]))
			}
			src[legacyFrame] = src[legacyFrame-1]
			fr = make([]int16, voiceFrame)
			for i := range fr {
				t := float64(i) * legacyFrame / voiceFrame
				k := int(t)
				fr[i] = int16(src[k] + (src[k+1]-src[k])*(t-float64(k)))
			}
		default:
			return
		}
		v.mu.Lock()
		v.peerLocked(from).fullAt = time.Now()
		v.mu.Unlock()
		v.onFrames(from, [][]int16{fr})
	case vkLow: // low-quality voice: they're far away in a big room
		v.mu.Lock()
		dup := time.Since(v.peerLocked(from).fullAt) < 100*time.Millisecond // switching from full quality: no repeats
		v.mu.Unlock()
		if frs := decodeLow(b); frs != nil && !dup {
			v.mu.Lock()
			v.peerLocked(from).lowAt = time.Now()
			v.mu.Unlock()
			v.onFrames(from, frs)
		}
	case vkPresence:
		if len(b) >= 3 && len(b) >= 3+int(b[2]) {
			v.mu.Lock()
			p := v.peerLocked(from)
			p.inGarage, p.bcasting, p.sub, p.presAt = b[1]&1 != 0, b[1]&2 != 0, string(b[3:3+int(b[2])]), time.Now()
			p.noRecord = b[1]&4 != 0
			v.mu.Unlock()
		}
	case vkSettings:
		var vs VoiceSettings
		if json.Unmarshal(b[1:], &vs) == nil {
			v.mu.Lock()
			v.peerLocked(from).shared = &vs
			v.mu.Unlock()
		}
	case vkPos:
		v.mu.Lock()
		p := v.peerLocked(from)
		if len(b) == 2+24 {
			p.game = b[1]
			p.pos = &vec3{
				math.Float64frombits(binary.BigEndian.Uint64(b[2:])),
				math.Float64frombits(binary.BigEndian.Uint64(b[10:])),
				math.Float64frombits(binary.BigEndian.Uint64(b[18:])),
			}
		} else {
			p.pos, p.game = nil, gameNone
		}
		p.posSeen = time.Now()
		p.mt.update(p.pos, p.game, p.posSeen)
		v.mu.Unlock()
	}
}

type mixJob struct {
	p          *vpeer
	fr         []int16
	gain, muff float64
	pan        float64
}

func (v *Voice) playLoop(spk speakerSink, stop chan struct{}) {
	defer spk.Close()
	out := make([]int16, voiceFrame*spkChannels)
	mixL := make([]float64, voiceFrame)
	mixR := make([]float64, voiceFrame)
	var jobs []mixJob
	for {
		select {
		case <-stop:
			return
		default:
		}
		jobs = jobs[:0]
		v.mu.Lock()
		now := time.Now()
		dop := float64(v.Doppler.Load()) / 100
		for id, p := range v.peers {
			d := v.distLocked(p)
			fr := p.nextFrameLocked(p.dopplerRateLocked(d, now, dop))
			pan, behind := v.dirLocked(p)
			m := v.muffleFor(d)
			m = 1 - (1-m)*(1-0.3*behind) // voices from behind you are a bit duller
			jobs = append(jobs, mixJob{p, fr, v.peerGainLocked(id, p, d), m, pan})
		}
		// voices recorded at this moment of the replay you're watching: queued per speaker
		// (so they play back smoothly) and heard from where their car was, relative to
		// the car the replay camera follows
		if v.rpPeers == nil {
			v.rpPeers = map[uint64]*vpeer{}
		}
		rpSpeed := v.Replay.Speed()
		rpMax := int(float64(jitterMax) * math.Max(1, rpSpeed))
		for _, f := range v.Replay.Frames(now) {
			q := v.rpPeers[f.Speaker]
			if q == nil {
				q = &vpeer{priming: true}
				v.rpPeers[f.Speaker] = q
			}
			q.queue = append(q.queue, f.PCM)
			if len(q.queue) > rpMax {
				q.queue = q.queue[len(q.queue)-rpMax/2:]
			}
			q.pos = f.Pos
		}
		if len(v.rpPeers) > 0 {
			lp, lh, lok := v.Replay.Listener()
			for _, q := range v.rpPeers {
				// slow motion stretches the voice (lower), fast forward squeezes it (higher)
				fr := q.nextFrameLocked(rpSpeed)
				if fr == nil {
					continue
				}
				d := -1.0
				if lp != nil && q.pos != nil {
					dx, dy, dz := lp.X-q.pos.X, lp.Y-q.pos.Y, lp.Z-q.pos.Z
					d = math.Sqrt(dx*dx + dy*dy + dz*dz)
				}
				pan, behind := 0.0, 0.0
				if lok {
					pan, behind = v.dirFrom(lp, lh, gameLMU, q.pos)
				}
				m := 1 - (1-v.muffleFor(d))*(1-0.3*behind)
				// replays: distance still fades and muffles, but never below 40 % so
				// teammates far from the camera car stay audible
				g := math.Max(v.gainFor(d), 0.4*float64(v.VolPct.Load())/100)
				jobs = append(jobs, mixJob{q, fr, g, m, pan})
			}
		}
		v.mu.Unlock()
		for i := range mixL {
			mixL[i], mixR[i] = 0, 0
		}
		for _, j := range jobs {
			p := j.p
			// two one-pole low-passes (12 dB/octave); coefficient glides toward its target
			a := 1 - math.Exp(-2*math.Pi*muffleCutoff(j.muff)/voiceRate)
			if p.lpA == 0 {
				p.lpA = a
			}
			if j.fr == nil { // silent this frame: keep filter/gain state moving
				p.gain, p.lpA, p.pan = j.gain, a, j.pan
				p.lp1, p.lp2 = 0, 0
				continue
			}
			dp := (j.pan - p.pan) / voiceFrame
			pn := p.pan
			da := (a - p.lpA) / voiceFrame
			step := (j.gain - p.gain) / voiceFrame // ramp so volume changes don't click
			g, la := p.gain, p.lpA
			for i, s := range j.fr {
				g += step
				la += da
				x := float64(s)
				p.lp1 += la * (x - p.lp1)
				p.lp2 += la * (p.lp1 - p.lp2)
				if j.muff > 0.01 || la < 0.94 {
					x = p.lp2
				}
				x *= g * (1 + 0.5*j.muff) // muffled voices sound quieter; make up some loudness
				// equal-power pan, 1:1 in the middle
				pn += dp
				th := (pn + 1) * math.Pi / 4
				mixL[i] += x * math.Min(math.Cos(th)*math.Sqrt2, 1.25)
				mixR[i] += x * math.Min(math.Sin(th)*math.Sqrt2, 1.25)
			}
			p.gain, p.lpA, p.pan = j.gain, a, j.pan
		}
		for i := range mixL {
			for c, x := range [2]float64{mixL[i], mixR[i]} {
				// gentle limiter so several people talking at once don't distort harshly
				if ax := math.Abs(x); ax > 24000 {
					x = math.Copysign(24000+(ax-24000)*0.25, x)
				}
				out[2*i+c] = int16(math.Max(-32768, math.Min(32767, x)))
			}
		}
		if err := spk.Write(out); err != nil {
			select {
			case <-stop:
				return
			default:
			}
			v.fail("speaker output stopped: %v", err)
			return
		}
	}
}

// PeerIDs returns the ids voice currently knows about (for tests/UI ordering).
func (v *Voice) PeerIDs() []uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	var ids []uint64
	for id := range v.peers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ---------- positions from LMU ----------

type lmuVec struct{ X, Y, Z float64 }

type lmuStanding struct {
	Player        bool    `json:"player"`
	HasFocus      bool    `json:"hasFocus"`
	InGarageStall bool    `json:"inGarageStall"`
	CarPosition   *lmuVec `json:"carPosition"`
	CarVelocity   *lmuVec `json:"carVelocity"`
}

// lmuState is what one look at LMU's standings tells us.
type lmuState struct {
	pos, vel       *vec3 // your car
	inGarage       bool
	focus, focusVl *vec3 // the car the camera follows (the replay's car while watching one)
	ok             bool
}

func (v *Voice) posLoop(stop chan struct{}) {
	cl := &http.Client{Timeout: 400 * time.Millisecond}
	bng := startBeamNGListener(stop, v.logf)
	t := time.NewTicker(100 * time.Millisecond) // LMU updates positions every 200 ms: catch each one quickly
	defer t.Stop()
	tick := 0
	var lastPres []byte
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-v.posKick:
		}
		v.mu.Lock()
		pres := []byte{vkPresence, 0, byte(len(v.mySub))}
		if v.inGarage {
			pres[1] |= 1
		}
		v.mu.Unlock()
		if v.Broadcasting() {
			pres[1] |= 2
		}
		if v.NoRecord.Load() {
			pres[1] |= 4 // please don't save my voice in your replays
		}
		pres = append(pres, v.Sub()...)
		if send := v.sender(); send != nil && (tick%10 == 0 || string(pres) != string(lastPres)) {
			send(pres)
			lastPres = pres
		}
		if tick++; tick%30 == 1 { // every 3 s: share your voice settings so others can copy them
			if send := v.sender(); send != nil {
				js, _ := json.Marshal(v.mySettings())
				send(append([]byte{vkSettings}, js...))
			}
		}
		// BeamNG only sends while you're driving, so a recent packet wins;
		// otherwise ask LMU.
		var game byte
		var pos, vel *vec3
		broadcast := false // in the garage or watching a replay: talk to everyone at full volume
		mode := byte(v.GameMode.Load())
		if mode != gameLMU {
			if pos = bng.recent(time.Second); pos != nil {
				game = gameBeamNG
			}
		}
		if pos == nil && mode != gameBeamNG {
			if st := readLMU(cl); st.ok {
				inReplay := v.Replay.InReplay()
				v.Replay.SetListener(st.focus, st.focusVl, inReplay)
				v.mu.Lock()
				pitRadio := v.pitRadio
				v.inGarage = st.inGarage && !inReplay
				v.mu.Unlock()
				switch {
				case inReplay || st.inGarage && pitRadio:
					broadcast = true
				case st.pos != nil: // pit radio off: the garage is just a place on the map
					pos, vel, game = st.pos, st.vel, gameLMU
				case st.inGarage:
					broadcast = true
				}
			}
		}
		_ = vel // LMU's carVelocity can be relative to the car, so the heading comes from movement
		now := time.Now()
		v.mu.Lock()
		v.me.update(pos, game, now)
		if h, ok := v.me.heading(); ok {
			if v.haveHead && game == v.myGame { // smooth a little so wiggles don't swing voices around
				h = [2]float64{0.7*h[0] + 0.3*v.myHead[0], 0.7*h[1] + 0.3*v.myHead[1]}
				n := math.Hypot(h[0], h[1])
				h = [2]float64{h[0] / n, h[1] / n}
			}
			v.myHead, v.haveHead = h, true
		} else if pos == nil || game != v.myGame {
			v.haveHead = false
		}
		v.myPos, v.myGame, v.broadcast = pos, game, broadcast
		if pos != nil {
			pos = v.me.now(now) // send where the car is now, not where LMU last said it was
		}
		v.mu.Unlock()
		if send := v.sender(); send != nil {
			if pos == nil || v.Broadcasting() {
				send([]byte{vkPos}) // no position = everyone hears you at full volume
			} else {
				b := make([]byte, 26)
				b[0], b[1] = vkPos, game
				binary.BigEndian.PutUint64(b[2:], math.Float64bits(pos.X))
				binary.BigEndian.PutUint64(b[10:], math.Float64bits(pos.Y))
				binary.BigEndian.PutUint64(b[18:], math.Float64bits(pos.Z))
				send(b)
			}
		}
	}
}

func readLMU(cl *http.Client) lmuState {
	var out lmuState
	r, err := cl.Get("http://127.0.0.1:6397/rest/watch/standings")
	if err != nil {
		return out
	}
	defer r.Body.Close()
	var st []lmuStanding
	if json.NewDecoder(r.Body).Decode(&st) != nil {
		return out
	}
	out.ok = true
	conv := func(p *lmuVec) *vec3 {
		if p == nil || p.X == 0 && p.Y == 0 && p.Z == 0 {
			return nil
		}
		return &vec3{p.X, p.Y, p.Z}
	}
	for _, s := range st {
		if s.Player {
			out.pos, out.vel, out.inGarage = conv(s.CarPosition), conv(s.CarVelocity), s.InGarageStall
		}
		if s.HasFocus {
			out.focus, out.focusVl = conv(s.CarPosition), conv(s.CarVelocity)
		}
	}
	return out
}

// ---------- G.711 mu-law ----------

func linearToULaw(s int16) byte {
	const bias, clip = 0x84, 32635
	x := int(s)
	sign := 0
	if x < 0 {
		x = -x
		sign = 0x80
	}
	if x > clip {
		x = clip
	}
	x += bias
	exp := 7
	for mask := 0x4000; x&mask == 0 && exp > 0; mask >>= 1 {
		exp--
	}
	mant := (x >> (exp + 3)) & 0x0F
	return ^byte(sign | exp<<4 | mant)
}

func uLawToLinear(u byte) int16 {
	u = ^u
	sign := u & 0x80
	exp := int(u>>4) & 7
	mant := int(u & 0x0F)
	x := ((mant << 3) + 0x84) << exp
	x -= 0x84
	if sign != 0 {
		return int16(-x)
	}
	return int16(x)
}
