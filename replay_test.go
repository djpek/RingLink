package main

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplayVoiceRoundTrip(t *testing.T) {
	r := NewReplayVoice()
	r.dir = t.TempDir()
	start := time.Now()
	// driving: session clock at 100 s, record 50 frames (1 s) from speaker 7
	r.mu.Lock()
	r.et, r.etAt, r.realtime, r.track = 100, start, true, "Road America"
	r.mu.Unlock()
	ul := make([]byte, voiceFrame)
	for i := range ul {
		ul[i] = linearToULaw(5000)
	}
	for i := 0; i < 50; i++ {
		r.mu.Lock()
		r.etAt = start.Add(-time.Duration(i) * 20 * time.Millisecond) // clock advances 20 ms per frame
		r.mu.Unlock()
		r.Record(7, ul, &vec3{1, 2, 3})
	}
	r.mu.Lock()
	r.closeRecLocked()
	r.realtime = false // now watching the replay
	r.mu.Unlock()
	// watching: LMU sends the replay position ~15 times a second, unevenly, and the
	// position only moves in 0.2 s steps. Replay starts at 99 s.
	base := time.Now()
	got, last := 0, -1
	gaps := 0
	nextMsg := 0.0
	for k := 0; k < 150; k++ { // 3 s of mixer frames
		w := float64(k) * 0.02
		at := base.Add(time.Duration(w * float64(time.Second)))
		if w >= nextMsg {
			et := 99 + math.Floor(w/0.2)*0.2
			r.onReplayPos(et, at)
			nextMsg += 0.05 + 0.04*float64(k%3) // 50..130 ms apart
		}
		for _, f := range r.Frames(at) {
			if f.PCM[10] > 4000 && f.Pos != nil && f.Pos.Y == 2 {
				got++
				if last >= 0 && k-last > 3 {
					gaps++
				}
				last = k
			}
		}
	}
	if got < 40 || gaps > 0 {
		t.Fatalf("played back %d of 50 recorded frames, %d gaps", got, gaps)
	}
	// paused replay: nothing plays
	r.mu.Lock()
	r.rpSpeed = 0
	r.mu.Unlock()
	if f := r.Frames(time.Now()); len(f) != 0 {
		t.Fatalf("paused replay still played %d frames", len(f))
	}
}

// LMU's clock moves in 0.2 s steps; recorded frames must still come out evenly spaced.
func TestReplayStampsEvenWithSteppedClock(t *testing.T) {
	r := NewReplayVoice()
	r.dir = t.TempDir()
	ul := make([]byte, voiceFrame)
	base := time.Now()
	var ets []float64
	for i := 0; i < 100; i++ { // 2 s of talking
		now := base.Add(time.Duration(i) * 20 * time.Millisecond)
		// RingLink polls every 0.5 s; LMU answers with its clock rounded down to 0.2 s
		pollT := float64(int(float64(i)*0.02/0.5)) * 0.5
		polled := base.Add(time.Duration(pollT * float64(time.Second)))
		r.mu.Lock()
		r.et, r.etAt, r.realtime = 50+float64(int(pollT/0.2))*0.2, polled, true
		r.mu.Unlock()
		r.recordAt(now, 1, ul, nil)
		r.mu.Lock()
		ets = append(ets, r.spkLast[1])
		r.mu.Unlock()
	}
	for i := 1; i < len(ets); i++ {
		if d := ets[i] - ets[i-1]; d < 0.019 || d > 0.021 {
			t.Fatalf("frame %d spaced %.3f s from the previous one", i, d)
		}
	}
}

// Two sessions on the same track whose clocks overlap: the replay's span picks the right one,
// even after a restart (only the files are there).
func TestReplayPicksTheRightSession(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, from, to float64, tag byte) {
		var b []byte
		for et := from; et < to; et += 0.02 {
			f := make([]byte, recFrameBytes)
			binary.LittleEndian.PutUint64(f, math.Float64bits(et))
			binary.LittleEndian.PutUint32(f[16:], math.Float32bits(float32(math.NaN())))
			for k := 28; k < recFrameBytes; k++ {
				f[k] = tag
			}
			b = append(b, f...)
		}
		os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	write("2026-11-01_20-00-00_Nordschleife.rlv2", 100, 1700, 1) // older race, 100..1700 s
	write("2026-11-03_14-00-00_Nordschleife.rlv2", 300, 500, 2)  // newer short practice, 300..500 s
	write("2026-11-03_15-00-00_Spa.rlv2", 100, 1700, 3)          // other track
	r := NewReplayVoice()
	r.dir = dir
	r.track = "Nordschleife"
	r.rpStart, r.rpLen = 90, 1620 // watching the old race
	if !r.ensureLoadedLocked(400) || r.frames[0].data[0] != 1 {
		t.Fatalf("race replay loaded %q", r.loaded)
	}
	r.loadTry = time.Time{}
	r.rpStart, r.rpLen = 290, 220 // now the practice
	if !r.ensureLoadedLocked(400) || r.frames[0].data[0] != 2 {
		t.Fatalf("practice replay loaded %q", r.loaded)
	}
}

// Slow motion and fast forward: the voice keeps playing, stretched or squeezed in step
// with the replay, with no gaps once it's going.
func TestReplayVoiceAtOtherSpeeds(t *testing.T) {
	for _, sp := range []float64{0.25, 0.5, 2, 4} {
		q := &vpeer{priming: true}
		ph := 0.0
		in := func() {
			fr := make([]int16, voiceFrame)
			for k := range fr {
				fr[k] = int16(8000 * math.Sin(ph))
				ph += 2 * math.Pi * 200 / voiceRate
			}
			q.queue = append(q.queue, fr)
		}
		// the replay hands out sp frames' worth of recorded audio per 20 ms
		acc, got, gaps := 0.0, 0, 0
		for tick := 0; tick < 300; tick++ {
			for acc += sp; acc >= 1; acc-- {
				in()
			}
			fr := q.nextFrameLocked(sp)
			if fr == nil {
				if got > 0 {
					gaps++
				}
				continue
			}
			got++
		}
		if got < 250 || gaps > 0 {
			t.Errorf("speed %.2f: %d frames played, %d gaps", sp, got, gaps)
		}
	}
}

// Opt-out: someone who ticked "Keep my voice out of replays" isn't saved in your
// recording; everyone else still is.
func TestReplayOptOut(t *testing.T) {
	v := NewVoice(func(string, ...any) {}, func() {})
	r := v.Replay
	r.dir = t.TempDir()
	r.track = "Test"
	r.mu.Lock()
	r.et, r.etAt, r.realtime = 100, time.Now(), true
	r.mu.Unlock()
	presence := func(from uint64, flags byte) { v.OnPacket(from, []byte{vkPresence, flags, 0}) }
	presence(1, 4) // opted out
	presence(2, 0) // didn't
	fr := make([]int16, voiceFrame)
	for i := range fr {
		fr[i] = 5000
	}
	for k := 0; k < 10; k++ {
		v.onFrames(1, [][]int16{fr})
		v.onFrames(2, [][]int16{fr})
	}
	r.mu.Lock()
	r.closeRecLocked()
	r.mu.Unlock()
	files, _ := filepath.Glob(filepath.Join(r.dir, "*.rlv2"))
	if len(files) != 1 {
		t.Fatalf("%d recordings", len(files))
	}
	got := map[uint64]int{}
	for _, f := range readRecording(files[0]) {
		got[f.speaker]++
	}
	if got[1] != 0 || got[2] != 10 {
		t.Fatalf("recorded per speaker: %v (want none from 1, all 10 from 2)", got)
	}
	// and your own voice stays out of your recording when you opt out
	v.NoRecord.Store(true)
	if !v.NoRecord.Load() {
		t.Fatal("flag")
	}
}
