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
	rpET      float64 // replay clock: session ET shown at rpAt (smoothed)
	rpAt      time.Time
	rpSpeed   float64
	rpSeen    time.Time // last position message
	rpHist    []rpSample
	loadStart float64 // replay start ET the loaded recording was picked for
	rpStart   float64 // the replay's first session ET and length: tells which session it is
	rpLen     float64
	loadKey   string // track the loaded recording was picked for
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
					Len   float64 `json:"replayLength"`
				} `json:"body"`
			}
			if json.Unmarshal([]byte(msg), &m) != nil {
				continue
			}
			r.mu.Lock()
			r.rpStart, r.rpLen = m.Body.Start, m.Body.Len
			r.mu.Unlock()
			r.onReplayPos(m.Body.Start+m.Body.Pos, time.Now())
		}
		ws.Close()
	}
}

// snapSpeed: LMU's replay speeds are fixed steps, and the measured trend wobbles a few
// percent, so round to the nearest step (otherwise the voice pitch would waver).
func snapSpeed(sp float64) float64 {
	if math.Abs(sp) < 0.08 {
		return 0
	}
	for _, st := range []float64{0.125, 0.25, 0.5, 1, 2, 4, 8} {
		if math.Abs(sp-st) <= 0.12*st {
			return st
		}
	}
	return sp
}

type rpSample struct {
	at time.Time
	et float64
}

// onReplayPos turns LMU's replay position messages into a smooth clock. The messages
// can come unevenly and the position can move in steps, so the speed is the trend over
// the last 2 s and small errors are eased out instead of jumping (a jump back would
// stop the voice). Real seeks (more than about half a second off) jump straight there.
func (r *ReplayVoice) onReplayPos(et float64, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rpHist = append(r.rpHist, rpSample{now, et})
	for len(r.rpHist) > 2 && now.Sub(r.rpHist[0].at) > 2*time.Second {
		r.rpHist = r.rpHist[1:]
	}
	first := r.rpSeen.IsZero() || now.Sub(r.rpSeen) > 2*time.Second
	r.rpSeen = now
	if first {
		r.rpET, r.rpAt, r.rpSpeed = et, now, 0
		r.rpHist = r.rpHist[len(r.rpHist)-1:]
		return
	}
	pred := r.rpET + r.rpSpeed*now.Sub(r.rpAt).Seconds()
	errT := et - pred
	if math.Abs(errT) > math.Max(0.6, 0.3*math.Abs(r.rpSpeed)) { // seek / jump
		r.rpET, r.rpAt = et, now
		r.rpHist = r.rpHist[len(r.rpHist)-1:]
		return
	}
	r.rpET, r.rpAt = pred+0.15*errT, now
	// speed: least-squares slope of the recent positions
	h := r.rpHist
	if span := h[len(h)-1].at.Sub(h[0].at).Seconds(); span >= 0.4 && len(h) >= 3 {
		var sx, sy, sxx, sxy float64
		for _, q := range h {
			x := q.at.Sub(h[0].at).Seconds()
			sx, sy, sxx, sxy = sx+x, sy+q.et, sxx+x*x, sxy+x*q.et
		}
		n := float64(len(h))
		if d := n*sxx - sx*sx; d > 1e-9 {
			r.rpSpeed = snapSpeed((n*sxy - sx*sy) / d)
		}
	}
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
	if r.realtime || r.rpSeen.IsZero() || now.Sub(r.rpSeen) > 800*time.Millisecond {
		return 0, false, false
	}
	sp := r.rpSpeed
	return r.rpET + sp*now.Sub(r.rpAt).Seconds(), sp >= minReplaySpeed && sp <= maxReplaySpeed, true
}

// voice plays along with slow motion and fast forward (pitch follows, like a tape);
// paused, rewinding or very fast skipping stays quiet
const (
	minReplaySpeed = 0.2
	maxReplaySpeed = 4.0
)

// Speed: how fast the replay is playing (1 = normal), for the mixer's resampler.
func (r *ReplayVoice) Speed() float64 {
	if r == nil {
		return 1
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sp := r.rpSpeed
	if sp < minReplaySpeed || sp > maxReplaySpeed {
		return 1
	}
	// snap near-normal speed to exactly 1 so normal replays aren't resampled
	if math.Abs(sp-1) < 0.04 {
		return 1
	}
	return sp
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
	if t < t0 && t > t0-0.15 { // the clock eased back a touch: just wait for it
		return nil
	}
	r.lastT = t
	if !normal || t < t0 || t-t0 > 0.5*math.Max(1, r.rpSpeed) { // paused, rewind or a jump: stay quiet and re-aim
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
		if f.et < t-0.5*math.Max(1, r.rpSpeed) {
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

// ensureLoadedLocked picks the recording made during the session this replay shows.
// Every session's clock starts near zero, so time alone can't tell sessions apart:
// recordings must be from the same track and fit inside the replay's time span,
// and the one that fills the span best wins (newest on a tie). Works after
// restarting the game or RingLink, since recordings are files.
func (r *ReplayVoice) ensureLoadedLocked(t float64) bool {
	n := len(r.frames)
	covers := n > 0 && t >= r.frames[0].et-30 && t <= r.frames[n-1].et+30
	// keep the loaded recording while it covers this moment of the same replay
	// (LMU's numbers wobble a little, so only a clearly different replay re-picks)
	if covers && r.loadKey == safeName(r.track) && math.Abs(r.loadStart-r.rpStart) < 5 {
		return true
	}
	if time.Since(r.loadTry) < 3*time.Second {
		return covers
	}
	r.loadTry = time.Now()
	m, _ := filepath.Glob(filepath.Join(r.dir, "*.rlv2"))
	best, bestScore := "", -1.0
	sameTrack := 0
	for _, p := range m {
		if strings.HasSuffix(filepath.Base(p), "_"+safeName(r.track)+".rlv2") {
			sameTrack++
		}
	}
	for _, p := range m {
		// only this track's recordings (unless LMU didn't give a name we have files for)
		if r.track != "" && sameTrack > 0 && !strings.HasSuffix(filepath.Base(p), "_"+safeName(r.track)+".rlv2") {
			continue
		}
		a, b, ok := recordingSpan(p)
		if !ok || t < a-30 || t > b+30 {
			continue
		}
		score := 1.0
		if r.rpLen > 0 { // how much of the recording lies inside the replay
			lo, hi := math.Max(a, r.rpStart-5), math.Min(b, r.rpStart+r.rpLen+5)
			inside := math.Max(0, hi-lo) / math.Max(1, b-a)
			if inside < 0.5 {
				continue // mostly outside this replay: another session
			}
			// and how much of the replay it spans (talk only covers part, but the right
			// session's recording almost always spans the most)
			score = inside + math.Min(1, (b-a)/r.rpLen)
		}
		if score > bestScore+1e-9 || math.Abs(score-bestScore) <= 1e-9 && p > best {
			best, bestScore = p, score
		}
	}
	if best == "" {
		return covers
	}
	if best != r.loaded || !covers {
		fr := readRecording(best)
		if len(fr) == 0 {
			return covers
		}
		r.loaded, r.frames = best, fr
		r.cursor = sort.Search(len(fr), func(i int) bool { return fr[i].et >= t })
	}
	r.loadKey, r.loadStart = safeName(r.track), r.rpStart
	return true
}

// recordingSpan: first and last session time in a recording, without reading it all.
func recordingSpan(p string) (first, last float64, ok bool) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < recFrameBytes {
		return 0, 0, false
	}
	b := make([]byte, 8)
	if _, err := f.ReadAt(b, 0); err != nil {
		return 0, 0, false
	}
	first = math.Float64frombits(binary.LittleEndian.Uint64(b))
	lastOff := (st.Size()/recFrameBytes - 1) * recFrameBytes
	if _, err := f.ReadAt(b, lastOff); err != nil {
		return 0, 0, false
	}
	last = math.Float64frombits(binary.LittleEndian.Uint64(b))
	if last < first {
		first, last = last, first
	}
	return first, last, true
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
