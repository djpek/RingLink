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
	"sync"
	"sync/atomic"
	"time"
)

const (
	voiceRate    = 16000
	voiceFrame   = 320 // samples per 20 ms
	vkAudio      = 1
	vkPos        = 2
	jitterTarget = 3  // frames buffered before a talker starts playing (60 ms)
	jitterMax    = 12 // drop old frames beyond this (240 ms)
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
	talkAt   time.Time
	gain     float64 // playback goroutine only
	lp1, lp2 float64
	lpA      float64
}

type Voice struct {
	send   func([]byte) // sends a voice payload to everyone in the room
	logf   func(string, ...any)
	notify func()

	mu      sync.Mutex
	running bool
	stop    chan struct{}
	peers   map[uint64]*vpeer
	myPos   *vec3
	myGame  byte
	micDev  string
	spkDev  string
	backend string
	errText string

	GameMode atomic.Int32 // gameNone = auto, gameLMU, gameBeamNG
	Muted    atomic.Bool
	Sens     atomic.Int32 // 0..100, higher = more sensitive
	VolPct   atomic.Int32 // everyone's volume, 0..200 %
	RangeM   atomic.Int32 // distance (m) where voices fade to silence
	Muffle   atomic.Int32 // 0..100: how muffled voices get with distance
	micLevel atomic.Int32 // 0..100 for the meter
	talking  atomic.Bool
	seq      uint16
	lastLoud time.Time
}

func NewVoice(logf func(string, ...any), notify func()) *Voice {
	v := &Voice{logf: logf, notify: notify, peers: map[uint64]*vpeer{}}
	v.Sens.Store(60)
	v.VolPct.Store(100)
	v.RangeM.Store(150)
	v.Muffle.Store(50)
	return v
}

func (v *Voice) SetSender(f func([]byte)) {
	v.mu.Lock()
	v.send = f
	if f == nil {
		v.peers = map[uint64]*vpeer{}
	}
	v.mu.Unlock()
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
			Volume:  int(math.Round(v.gainFor(d) * 100)),
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
	dx, dy, dz := v.myPos.X-p.pos.X, v.myPos.Y-p.pos.Y, v.myPos.Z-p.pos.Z
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
	return farFrac(d, float64(v.RangeM.Load())) * float64(v.Muffle.Load()) / 100
}

// cutoff for the low-pass filter: ~7.5 kHz (clear) down to ~250 Hz (behind a wall).
func muffleCutoff(m float64) float64 {
	return 7500 * math.Pow(250.0/7500, m)
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
		talking := time.Since(v.lastLoud) < 350*time.Millisecond && !v.Muted.Load()
		v.talking.Store(talking)
		if !talking {
			continue
		}
		send := v.sender()
		if send == nil {
			continue
		}
		pkt[0] = vkAudio
		binary.BigEndian.PutUint16(pkt[1:], v.seq)
		v.seq++
		for i, s := range frame {
			pkt[3+i] = linearToULaw(s)
		}
		send(pkt)
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

// OnPacket handles a voice payload from someone in the room.
func (v *Voice) OnPacket(from uint64, b []byte) {
	if len(b) < 1 {
		return
	}
	switch b[0] {
	case vkAudio:
		if len(b) != 3+voiceFrame {
			return
		}
		fr := make([]int16, voiceFrame)
		loud := false
		for i := range fr {
			fr[i] = uLawToLinear(b[3+i])
			if fr[i] > 2000 || fr[i] < -2000 {
				loud = true
			}
		}
		v.mu.Lock()
		p := v.peerLocked(from)
		if loud {
			p.talkAt = time.Now()
		}
		if v.running {
			p.queue = append(p.queue, fr)
			if len(p.queue) > jitterMax {
				p.queue = p.queue[len(p.queue)-jitterTarget:]
			}
		}
		v.mu.Unlock()
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
		v.mu.Unlock()
	}
}

type mixJob struct {
	p          *vpeer
	fr         []int16
	gain, muff float64
}

func (v *Voice) playLoop(spk speakerSink, stop chan struct{}) {
	defer spk.Close()
	out := make([]int16, voiceFrame)
	mix := make([]float64, voiceFrame)
	var jobs []mixJob
	for {
		select {
		case <-stop:
			return
		default:
		}
		jobs = jobs[:0]
		v.mu.Lock()
		for _, p := range v.peers {
			var fr []int16
			if p.priming && len(p.queue) >= jitterTarget {
				p.priming = false
			}
			if !p.priming && len(p.queue) > 0 {
				fr = p.queue[0]
				p.queue = p.queue[1:]
			} else if !p.priming {
				p.priming = true // ran dry: rebuffer before their next sentence
			}
			d := v.distLocked(p)
			jobs = append(jobs, mixJob{p, fr, v.gainFor(d), v.muffleFor(d)})
		}
		v.mu.Unlock()
		for i := range mix {
			mix[i] = 0
		}
		for _, j := range jobs {
			p := j.p
			// two one-pole low-passes (12 dB/octave); coefficient glides toward its target
			a := 1 - math.Exp(-2*math.Pi*muffleCutoff(j.muff)/voiceRate)
			if p.lpA == 0 {
				p.lpA = a
			}
			if j.fr == nil { // silent this frame: keep filter/gain state moving
				p.gain, p.lpA = j.gain, a
				p.lp1, p.lp2 = 0, 0
				continue
			}
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
				mix[i] += x * g * (1 + 0.5*j.muff) // muffled voices sound quieter; make up some loudness
			}
			p.gain, p.lpA = j.gain, a
		}
		for i, x := range mix {
			// gentle limiter so several people talking at once don't distort harshly
			if ax := math.Abs(x); ax > 24000 {
				x = math.Copysign(24000+(ax-24000)*0.25, x)
			}
			out[i] = int16(math.Max(-32768, math.Min(32767, x)))
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

type lmuStanding struct {
	Player      bool `json:"player"`
	CarPosition *struct {
		X, Y, Z float64
	} `json:"carPosition"`
}

func (v *Voice) posLoop(stop chan struct{}) {
	cl := &http.Client{Timeout: 400 * time.Millisecond}
	bng := startBeamNGListener(stop, v.logf)
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		// BeamNG only sends while you're driving, so a recent packet wins;
		// otherwise ask LMU.
		var game byte
		var pos *vec3
		mode := byte(v.GameMode.Load())
		if mode != gameLMU {
			if pos = bng.recent(time.Second); pos != nil {
				game = gameBeamNG
			}
		}
		if pos == nil && mode != gameBeamNG {
			if pos = readLMUPos(cl); pos != nil {
				game = gameLMU
			}
		}
		v.mu.Lock()
		v.myPos, v.myGame = pos, game
		v.mu.Unlock()
		if send := v.sender(); send != nil {
			if pos == nil {
				send([]byte{vkPos})
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

func readLMUPos(cl *http.Client) *vec3 {
	r, err := cl.Get("http://127.0.0.1:6397/rest/watch/standings")
	if err != nil {
		return nil
	}
	defer r.Body.Close()
	var st []lmuStanding
	if json.NewDecoder(r.Body).Decode(&st) != nil {
		return nil
	}
	for _, s := range st {
		if s.Player && s.CarPosition != nil {
			p := s.CarPosition
			if p.X == 0 && p.Y == 0 && p.Z == 0 {
				return nil
			}
			return &vec3{p.X, p.Y, p.Z}
		}
	}
	return nil
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
