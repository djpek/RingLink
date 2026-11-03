package main

import (
	"testing"
	"time"
)

func TestDirectionalPan(t *testing.T) {
	v := NewVoice(func(string, ...any) {}, func() {})
	for _, g := range []struct {
		game  byte
		ahead func(float64) *vec3 // a point d metres "forward" along the game's horizontal forward axis
		right *vec3                // a point to the right of a car driving that way
	}{
		// LMU: y up, left-handed; driving +z, +x is on your right
		{gameLMU, func(d float64) *vec3 { return &vec3{0, 0, d} }, &vec3{10, 0, 3}},
		// BeamNG: z up, right-handed; driving +y, +x is on your right
		{gameBeamNG, func(d float64) *vec3 { return &vec3{0, d, 0} }, &vec3{10, 3, 0}},
	} {
		v.mu.Lock()
		v.myPos, v.myGame, v.haveHead, v.headAt = nil, gameNone, false, [2]float64{}
		for d := 0.0; d <= 6; d += 2 { // drive forward a few metres
			p := g.ahead(d)
			v.trackHeadingLocked(p, g.game, v.myGame)
			v.myPos, v.myGame = p, g.game
		}
		v.myPos = g.ahead(3)
		peer := &vpeer{pos: g.right, game: g.game, posSeen: time.Now()}
		pan, behind := v.dirLocked(peer)
		peerL := &vpeer{pos: &vec3{-g.right.X, g.right.Y, g.right.Z}, game: g.game, posSeen: time.Now()}
		panL, _ := v.dirLocked(peerL)
		back := &vpeer{pos: g.ahead(-20), game: g.game, posSeen: time.Now()}
		_, behindB := v.dirLocked(back)
		v.mu.Unlock()
		if !(pan > 0.9 && panL < -0.9 && behind < 0.1 && behindB > 0.9) {
			t.Fatalf("game %d: right pan %.2f, left pan %.2f, behind(side) %.2f, behind(back) %.2f", g.game, pan, panL, behind, behindB)
		}
	}
}

func TestLegacyFramesUpsampled(t *testing.T) {
	v := NewVoice(func(string, ...any) {}, func() {})
	v.running = true
	pkt := make([]byte, 3+legacyFrame)
	pkt[0] = vkAudio
	for i := range pkt[3:] {
		pkt[3+i] = linearToULaw(8000)
	}
	v.OnPacket(7, pkt)
	v.mu.Lock()
	q := v.peers[7].queue
	v.mu.Unlock()
	if len(q) != 1 || len(q[0]) != voiceFrame || q[0][100] < 7000 {
		t.Fatalf("legacy frame not decoded to %d samples: %d frames", voiceFrame, len(q))
	}
}
