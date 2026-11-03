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
	recFrameBytes  = 8 + 8 + voiceFrame // ET (float64), speaker id, mu-law samples
	maxReplayFiles = 30
)

type recFrame struct {
	et      float64
	speaker uint64
	data    []byte
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
	return r.et + now.Sub(r.etAt).Seconds(), true
}

// Record stores one 20 ms voice frame (mu-law) heard or spoken live.
func (r *ReplayVoice) Record(speaker uint64, ulaw []byte) {
	if r == nil || !r.Enabled() || len(ulaw) != voiceFrame {
		return
	}
	now := time.Now()
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
		name := now.Format("2006-01-02_15-04-05") + "_" + safeName(r.track) + ".rlv"
		f, err := os.Create(filepath.Join(r.dir, name))
		if err != nil {
			return
		}
		r.rec = f
		r.pruneLocked()
	}
	b := make([]byte, recFrameBytes)
	binary.LittleEndian.PutUint64(b, math.Float64bits(et))
	binary.LittleEndian.PutUint64(b[8:], speaker)
	copy(b[16:], ulaw)
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
	m, _ := filepath.Glob(filepath.Join(r.dir, "*.rlv"))
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
func (r *ReplayVoice) Frames(now time.Time) [][]int16 {
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
	if !normal || t < t0 || t-t0 > 0.5 { // paused, fast-forward/rewind or a jump: stay quiet
		r.playingNow = false
		return nil
	}
	i := sort.Search(len(r.frames), func(i int) bool { return r.frames[i].et >= t0 })
	var out [][]int16
	seen := map[uint64]bool{}
	for ; i < len(r.frames) && r.frames[i].et < t; i++ {
		f := r.frames[i]
		if seen[f.speaker] {
			continue
		}
		seen[f.speaker] = true
		fr := make([]int16, voiceFrame)
		for k := range fr {
			fr[k] = uLawToLinear(f.data[k])
		}
		out = append(out, fr)
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
	m, _ := filepath.Glob(filepath.Join(r.dir, "*.rlv"))
	sort.Sort(sort.Reverse(sort.StringSlice(m)))
	for _, p := range m {
		fr := readRecording(p)
		if len(fr) > 0 && t >= fr[0].et-30 && t <= fr[len(fr)-1].et+30 {
			r.loaded, r.frames = p, fr
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
		out = append(out, recFrame{
			et:      math.Float64frombits(binary.LittleEndian.Uint64(b[off:])),
			speaker: binary.LittleEndian.Uint64(b[off+8:]),
			data:    b[off+16 : off+recFrameBytes],
		})
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
