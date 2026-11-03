//go:build linux && !fakeaudio

package main

import (
	"testing"
	"time"
)

func TestPlaybackPacing(t *testing.T) {
	p, _, err := startProc([][]string{{"sh", "-c", "cat >/dev/null"}}, false, "")
	if err != nil {
		t.Skip(err)
	}
	defer p.Close()
	fr := make([]int16, voiceFrame)
	t0 := time.Now()
	for i := 0; i < 50; i++ { // 1 s of audio
		if err := p.Write(fr); err != nil {
			t.Fatal(err)
		}
	}
	el := time.Since(t0)
	if el < 850*time.Millisecond || el > 1200*time.Millisecond {
		t.Fatalf("50 frames took %v, want ~0.94 s (paced to real time)", el)
	}
}
