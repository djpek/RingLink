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
	// replay shows session time 100.2 s, playing at normal speed
	now := time.Now()
	r.onReplayPos(100.2, now)
	r.onReplayPos(100.22, now.Add(20*time.Millisecond))
	got := 0
	for k := 0; k < 25; k++ { // 0.5 s of mixer frames
		at := now.Add(time.Duration(20+20*k) * time.Millisecond)
		r.mu.Lock()
		r.rpAt = at // keep the replay feed "fresh"
		r.rpET = 100.22 + float64(k)*0.02
		r.mu.Unlock()
		for _, f := range r.Frames(at) {
			if f.PCM[10] > 4000 && f.Pos != nil && f.Pos.Y == 2 {
				got++
			}
		}
	}
	if got < 18 {
		t.Fatalf("played back %d recorded frames during 0.5 s of replay, want ~24", got)
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
