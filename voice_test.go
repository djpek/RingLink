package main

import (
	"math"
	"testing"
	"time"
)

func TestULaw(t *testing.T) {
	for _, x := range []int16{0, 1, -1, 100, -100, 1000, -5000, 20000, 32767, -32768} {
		y := uLawToLinear(linearToULaw(x))
		if d := math.Abs(float64(x) - float64(y)); d > math.Abs(float64(x))*0.07+8 {
			t.Errorf("%d -> %d", x, y)
		}
	}
}

func TestProximityGain(t *testing.T) {
	if proximityGain(-1, 150) != 1 || proximityGain(5, 150) != 1 || proximityGain(200, 150) != 0 {
		t.Fatal("bounds")
	}
	if g := proximityGain(80, 150); g <= 0 || g >= 1 {
		t.Fatal(g)
	}
	if muffleCutoff(0) < 7000 || muffleCutoff(1) > 300 {
		t.Fatal("cutoff")
	}
}

type toneMic struct {
	n    int
	freq float64
}

func (m *toneMic) Read(f []int16) error {
	time.Sleep(20 * time.Millisecond)
	for i := range f {
		f[i] = int16(8000 * math.Sin(float64(m.n+i)*2*math.Pi*m.freq/voiceRate))
	}
	m.n += len(f)
	return nil
}
func (m *toneMic) Close() {}

type fakeSpk struct{ peak chan int16 }

func (s *fakeSpk) Write(f []int16) error {
	time.Sleep(20 * time.Millisecond)
	var p int16
	for _, x := range f {
		if x > p {
			p = x
		}
	}
	select {
	case s.peak <- p:
	default:
	}
	return nil
}
func (s *fakeSpk) Close() {}

func peakOver(spk *fakeSpk, d time.Duration) int16 {
	var p int16
	end := time.After(d)
	for {
		select {
		case x := <-spk.peak:
			if x > p {
				p = x
			}
		case <-end:
			return p
		}
	}
}

// two talkers into one listener: both are mixed; a far one goes silent
func TestVoiceMix(t *testing.T) {
	l := NewVoice(t.Logf, func() {})
	a := NewVoice(t.Logf, func() {})
	b := NewVoice(t.Logf, func() {})
	a.SetSender(func(p []byte) { l.OnPacket(1, append([]byte(nil), p...)) })
	b.SetSender(func(p []byte) { l.OnPacket(2, append([]byte(nil), p...)) })
	stop := make(chan struct{})
	defer close(stop)
	l.running = true
	spk := &fakeSpk{peak: make(chan int16, 1000)}
	go l.playLoop(spk, stop)
	go a.captureLoop(&toneMic{freq: 440}, stop)
	peakOver(spk, 300*time.Millisecond)
	one := peakOver(spk, time.Second)
	if one < 6000 {
		t.Fatalf("one talker: peak %d", one)
	}
	go b.captureLoop(&toneMic{freq: 660}, stop)
	peakOver(spk, 300*time.Millisecond)
	two := peakOver(spk, time.Second)
	if two <= one+2000 {
		t.Fatalf("two talkers not mixed: %d vs %d", two, one)
	}
	if len(l.Status().Peers) != 2 {
		t.Fatal("expected 2 peers")
	}
	// put talker 2 far away (positions) -> only talker 1 heard
	l.mu.Lock()
	l.myPos = &vec3{}
	l.peerLocked(2).pos, l.peerLocked(2).posSeen = &vec3{X: 500}, time.Now().Add(time.Hour)
	l.peerLocked(1).pos, l.peerLocked(1).posSeen = &vec3{X: 5}, time.Now().Add(time.Hour)
	l.mu.Unlock()
	peakOver(spk, 300*time.Millisecond)
	far := peakOver(spk, time.Second)
	if far > one+500 || far < 6000 {
		t.Fatalf("far talker should be silent: %d (one=%d)", far, one)
	}
	st := l.Status()
	if st.Peers[2].Volume != 0 || st.Peers[1].Volume != 100 {
		t.Fatalf("status %+v", st.Peers)
	}
}

// Low quality for far-away people: a 300 Hz tone survives the trip (still recognisable),
// three frames come back per packet, and a packet is about a fifth of the full-quality bytes.
func TestLowQualityVoice(t *testing.T) {
	var e lowEncoder
	var pkts [][]byte
	ph := 0.0
	var in []int16
	for f := 0; f < 9; f++ {
		fr := make([]int16, voiceFrame)
		for i := range fr {
			fr[i] = int16(8000 * math.Sin(ph))
			ph += 2 * math.Pi * 300 / voiceRate
		}
		in = append(in, fr...)
		if p := e.add(fr); p != nil {
			pkts = append(pkts, p)
		}
	}
	if len(pkts) != 3 {
		t.Fatalf("%d packets for 9 frames", len(pkts))
	}
	var out []int16
	for _, p := range pkts {
		frs := decodeLow(p)
		if len(frs) != 3 {
			t.Fatal("bad decode")
		}
		for _, f := range frs {
			out = append(out, f...)
		}
	}
	// compare after the filters settle, allowing for the low-pass delay
	best := 0.0
	for lag := 0; lag < 20; lag++ {
		var num, den float64
		for i := 2000; i < len(in)-lag; i++ {
			num += float64(in[i]) * float64(out[i+lag])
			den += float64(in[i]) * float64(in[i])
		}
		best = math.Max(best, num/den)
	}
	if best < 0.8 {
		t.Fatalf("tone came back too different: correlation %.2f", best)
	}
	full := 3 * (3 + voiceFrame + 80) // 3 full packets incl. ~80 bytes overhead each
	low := lowPktBytes + 80
	if float64(low)/float64(full) > 0.25 {
		t.Fatalf("low quality uses %d bytes vs %d", low, full)
	}
	t.Logf("low quality: %d bytes per 60 ms vs %d (%.0f%%)", low, full, 100*float64(low)/float64(full))
}
