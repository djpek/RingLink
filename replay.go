package main

// Voice in LMU replays.
//
// While you drive, every voice frame (yours and everyone else's) is saved with the
// session time it happened at (sessionInfo.currentEventTime, which is the session's
// elapsed time - "ET"). When you watch a replay, LMU streams the replay position on
// ws://127.0.0.1:6398/websocket/replaymetrics (replayStartET + currentReplayPos = the
// session ET being shown), and the frames recorded at that moment are played again.
// Only talking is stored (~500 bytes per 20 ms of speech), one file per session.

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/websocket"
)

const (
	recFrameBytes  = 8 + 8 + 12 + voiceFrame // ET (float64), speaker id, car position (3x float32, NaN = none), mu-law
	maxReplayFiles = 30
)

type recFrame struct {
	et      float64
	speaker uint64
	pos     *vec3 // where their car was (nil: garage / unknown -> heard at full volume)
	data    []byte
}

// ReplayVoiceFrame is one recorded frame to play now, with where it came from.
type ReplayVoiceFrame struct {
	Speaker uint64
	Pos     *vec3
	PCM     []int16
}

type ReplayVoice struct {
	off atomic.Bool // record + play disabled (UI setting)

	dir string
	mu  sync.Mutex
	// live session clock from sessionInfo
	et       float64
	etAt     time.Time
	realtime bool
	track    string
	// recording
	rec     *os.File
	recLast float64
	// replay position from the websocket
	rpET    float64 // session ET shown by the replay at rpAt
	rpAt    time.Time
	rpSpeed float64
	// loaded recording for playback
	loaded     string
	frames     []recFrame // sorted by et
	lastT      float64
	playingNow bool
	loadTry    time.Time
	spkLast    map[uint64]float64 // last timestamp written per speaker (keeps a burst evenly spaced)
	cursor     int                // next recorded frame to hand out during a replay
	// the car the replay camera is following (the listener while watching)
	lisPos  *vec3
	lisHead [2]float64
	lisMot  motion
	lisOK   bool
}

// InReplay: LMU says you're not live (watching a replay).
func (r *ReplayVoice) InReplay() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.realtime && !r.etAt.IsZero() && time.Since(r.etAt) < 2*time.Second
}

// SetListener records where the replay camera's car is and which way it's heading.
func (r *ReplayVoice) SetListener(pos, vel *vec3, inReplay bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !inReplay || pos == nil {
		r.lisPos, r.lisOK = nil, false
		return
	}
	_ = vel // can be relative to the car: use how the camera car moves instead
	now := time.Now()
	r.lisMot.update(pos, gameLMU, now)
	if h, ok := r.lisMot.heading(); ok {
		r.lisHead, r.lisOK = h, true
	}
	r.lisPos = r.lisMot.now(now)
}

// Listener: the replay camera car's position and heading (ok=false if unknown).
func (r *ReplayVoice) Listener() (pos *vec3, head [2]float64, headOK bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lisPos, r.lisHead, r.lisOK
}

func (r *ReplayVoice) Enabled() bool      { return r != nil && !r.off.Load() }
func (r *ReplayVoice) SetEnabled(on bool) { r.off.Store(!on) }

func NewReplayVoice() *ReplayVoice {
	d, _ := os.UserConfigDir()
	return &ReplayVoice{dir: filepath.Join(d, "RingLink", "voice-replays"), rpSpeed: 1}
}

// Run polls the session clock and listens for replay positions until stop closes.
func (r *ReplayVoice) Run(stop chan struct{}) {
	go r.clockLoop(stop)
	go r.replayLoop(stop)
}

func (r *ReplayVoice) clockLoop(stop chan struct{}) {
	cl := &http.Client{Timeout: 400 * time.Millisecond}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			r.mu.Lock()
			r.closeRecLocked()
			r.mu.Unlock()
			return
		case <-t.C:
		}
		var si struct {
			CurrentEventTime float64 `json:"currentEventTime"`
			InRealtime       bool    `json:"inRealtime"`
			TrackName        string  `json:"trackName"`
		}
		resp, err := cl.Get("http://127.0.0.1:6397/rest/watch/sessionInfo")
		if err != nil {
			continue
		}
		ok := json.NewDecoder(resp.Body).Decode(&si) == nil
		resp.Body.Close()
		if !ok {
			continue
		}
		r.mu.Lock()
		r.et, r.etAt, r.realtime, r.track = si.CurrentEventTime, time.Now(), si.InRealtime, si.TrackName
		r.mu.Unlock()
	}
}

func (r *ReplayVoice) replayLoop(stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		ws, err := websocket.Dial("ws://127.0.0.1:6398/websocket/replaymetrics", "", "http://127.0.0.1/")
		if err != nil {
			select {
			case <-stop:
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		go func() { <-stop; ws.Close() }()
		for {
			var msg string
			if err := websocket.Message.Receive(ws, &msg); err != nil {
				break
			}
			if !strings.Contains(msg[:min(len(msg), 40)], "replayMetrics") {
				continue
			}
			var m struct {
				Body struct {
					Start float64 `json:"replayStartET"`
					Pos   float64 `json:"currentReplayPos"`
				} `json:"body"`
			}
			if json.Unmarshal([]byte(msg), &m) != nil {
				continue
			}
			r.onReplayPos(m.Body.Start+m.Body.Pos, time.Now())
		}
		ws.Close()
	}
}

func (r *ReplayVoice) onReplayPos(et float64, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.rpAt.IsZero() {
		if dt := now.Sub(r.rpAt).Seconds(); dt > 0.01 && dt < 1 {
			s := (et - r.rpET) / dt
			r.rpSpeed = 0.7*r.rpSpeed + 0.3*s
		}
	}
	r.rpET, r.rpAt = et, now
}

// liveETLocked is the current session time while driving (false when unknown / in a replay).
func (r *ReplayVoice) liveETLocked(now time.Time) (float64, bool) {
	if !r.realtime || r.etAt.IsZero() || now.Sub(r.etAt) > 2*time.Second {
		return 0, false
	}
	// the reported value lags the real clock by up to one 0.2 s step: aim for the middle
	return r.et + 0.1 + now.Sub(r.etAt).Seconds(), true
}

// Record stores one 20 ms voice frame (mu-law) heard or spoken live.
func (r *ReplayVoice) Record(speaker uint64, ulaw []byte, pos *vec3) {
	r.recordAt(time.Now(), speaker, ulaw, pos)
}

func (r *ReplayVoice) recordAt(now time.Time, speaker uint64, ulaw []byte, pos *vec3) {
	if r == nil || !r.Enabled() || len(ulaw) != voiceFrame {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	et, ok := r.liveETLocked(now)
	if !ok {
		return
	}
	// new session: time went backwards or a long gap
	if r.rec != nil && (et < r.recLast-5 || et > r.recLast+900) {
		r.closeRecLocked()
	}
	if r.rec == nil {
		os.MkdirAll(r.dir, 0o755)
		name := now.Format("2006-01-02_15-04-05") + "_" + safeName(r.track) + ".rlv2"
		f, err := os.Create(filepath.Join(r.dir, name))
		if err != nil {
			return
		}
		r.rec = f
		r.pruneLocked()
	}
	// LMU's session clock only moves in 0.2 s steps, so within a stretch of talking
	// stamp frames exactly 20 ms apart and only resync if we drift far off
	if r.spkLast == nil {
		r.spkLast = map[uint64]float64{}
	}
	if last, ok := r.spkLast[speaker]; ok && et-last < 0.3 && et-last > -0.3 {
		if next := last + 0.02; math.Abs(et-next) < 0.25 {
			et = next
		}
	}
	r.spkLast[speaker] = et
	b := make([]byte, recFrameBytes)
	binary.LittleEndian.PutUint64(b, math.Float64bits(et))
	binary.LittleEndian.PutUint64(b[8:], speaker)
	px, py, pz := float32(math.NaN()), float32(0), float32(0)
	if pos != nil {
		px, py, pz = float32(pos.X), float32(pos.Y), float32(pos.Z)
	}
	binary.LittleEndian.PutUint32(b[16:], math.Float32bits(px))
	binary.LittleEndian.PutUint32(b[20:], math.Float32bits(py))
	binary.LittleEndian.PutUint32(b[24:], math.Float32bits(pz))
	copy(b[28:], ulaw)
	r.rec.Write(b)
	r.recLast = et
}

func (r *ReplayVoice) closeRecLocked() {
	if r.rec != nil {
		r.rec.Close()
		r.rec = nil
	}
}

func (r *ReplayVoice) pruneLocked() {
	m, _ := filepath.Glob(filepath.Join(r.dir, "*.rlv2"))
	sort.Strings(m)
	for len(m) > maxReplayFiles {
		os.Remove(m[0])
		m = m[1:]
	}
}

func safeName(s string) string {
	s = strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' {
			return c
		}
		return '_'
	}, s)
	if s == "" {
		s = "session"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

// replayTimeLocked: the session time the replay is showing right now, and whether
// it's playing at (about) normal speed.
func (r *ReplayVoice) replayTimeLocked(now time.Time) (et float64, normal, active bool) {
	if r.realtime || r.rpAt.IsZero() || now.Sub(r.rpAt) > 600*time.Millisecond {
		return 0, false, false
	}
	sp := r.rpSpeed
	return r.rpET + sp*now.Sub(r.rpAt).Seconds(), sp > 0.85 && sp < 1.2, true
}

// Frames returns the recorded frames to play during the next 20 ms of the replay.
// Called from the mixer every frame.
func (r *ReplayVoice) Frames(now time.Time) []ReplayVoiceFrame {
	if r == nil || !r.Enabled() {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, normal, active := r.replayTimeLocked(now)
	if !active {
		r.playingNow = false
		return nil
	}
	if !r.ensureLoadedLocked(t) {
		r.lastT, r.playingNow = t, false
		return nil
	}
	t0 := r.lastT
	r.lastT = t
	if !normal || t < t0 || t-t0 > 0.5 { // paused, fast-forward/rewind or a jump: stay quiet and re-aim
		r.cursor = sort.Search(len(r.frames), func(i int) bool { return r.frames[i].et >= t })
		r.playingNow = false
		return nil
	}
	if r.cursor > len(r.frames) {
		r.cursor = len(r.frames)
	}
	// hand out every frame whose time has come, in order; the mixer queues them per
	// speaker and plays them back to back, so timing wobble doesn't drop or bunch audio
	var out []ReplayVoiceFrame
	for ; r.cursor < len(r.frames) && r.frames[r.cursor].et < t; r.cursor++ {
		f := r.frames[r.cursor]
		if f.et < t-0.5 {
			continue // stale (just loaded or caught up after a hiccup)
		}
		fr := make([]int16, voiceFrame)
		for k := range fr {
			fr[k] = uLawToLinear(f.data[k])
		}
		out = append(out, ReplayVoiceFrame{f.speaker, f.pos, fr})
	}
	r.playingNow = len(out) > 0
	return out
}

// ensureLoadedLocked loads the newest recording that covers session time t
// (rescanning at most every 3 s, and re-reading the file still being recorded).
func (r *ReplayVoice) ensureLoadedLocked(t float64) bool {
	n := len(r.frames)
	if n > 0 && t >= r.frames[0].et-30 && t <= r.frames[n-1].et+30 {
		return true
	}
	if time.Since(r.loadTry) < 3*time.Second {
		return false
	}
	r.loadTry = time.Now()
	m, _ := filepath.Glob(filepath.Join(r.dir, "*.rlv2"))
	sort.Sort(sort.Reverse(sort.StringSlice(m)))
	for _, p := range m {
		fr := readRecording(p)
		if len(fr) > 0 && t >= fr[0].et-30 && t <= fr[len(fr)-1].et+30 {
			r.loaded, r.frames, r.cursor = p, fr, sort.Search(len(fr), func(i int) bool { return fr[i].et >= t })
			return true
		}
	}
	return false
}

func readRecording(p string) []recFrame {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []recFrame
	for off := 0; off+recFrameBytes <= len(b); off += recFrameBytes {
		f := recFrame{
			et:      math.Float64frombits(binary.LittleEndian.Uint64(b[off:])),
			speaker: binary.LittleEndian.Uint64(b[off+8:]),
			data:    b[off+28 : off+recFrameBytes],
		}
		x := math.Float32frombits(binary.LittleEndian.Uint32(b[off+16:]))
		if !math.IsNaN(float64(x)) {
			f.pos = &vec3{float64(x), float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off+20:]))),
				float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off+24:])))}
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].et < out[j].et })
	return out
}

// Status for the UI.
func (r *ReplayVoice) Status() (recording, inReplay bool) {
	if r == nil {
		return false, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _, active := r.replayTimeLocked(time.Now())
	return r.rec != nil && r.realtime, active
}
