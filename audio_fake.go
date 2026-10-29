//go:build fakeaudio

package main

// Fake audio devices for UI screenshots and tests (go build -tags fakeaudio):
// the "mic" says something every few seconds, the "speakers" discard audio.

import (
	"math"
	"time"
)

type fakeMicDev struct{ n int }

func (m *fakeMicDev) Read(f []int16) error {
	time.Sleep(20 * time.Millisecond)
	t := float64(m.n) / voiceRate
	on := math.Mod(t, 4) < 1.6 // talk 1.6 s out of every 4 s
	for i := range f {
		x := 0.0
		if on {
			tt := float64(m.n+i) / voiceRate
			x = 6000 * math.Sin(2*math.Pi*180*tt) * (0.6 + 0.4*math.Sin(2*math.Pi*3*tt))
		}
		f[i] = int16(x)
	}
	m.n += len(f)
	return nil
}
func (m *fakeMicDev) Close() {}

type fakeSpkDev struct{}

func (fakeSpkDev) Write(f []int16) error { time.Sleep(20 * time.Millisecond); return nil }
func (fakeSpkDev) Close()                {}

func openMic() (micSource, string, error)       { return &fakeMicDev{}, "test mic", nil }
func openSpeaker() (speakerSink, string, error) { return fakeSpkDev{}, "test output", nil }
