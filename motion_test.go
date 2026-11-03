package main

import (
	"math"
	"testing"
	"time"
)

// Two cars driving side by side round a big circle, LMU-style: positions change only
// every 200 ms and are polled every 100 ms. The friend is always on your right, so
// the voice must stay on the right whichever way the pair is heading.
func TestDirectionStaysRightWhileTurning(t *testing.T) {
	v := NewVoice(func(string, ...any) {}, func() {})
	p := &vpeer{}
	v.peers[1] = p
	base := time.Now()
	const R, speed = 300.0, 50.0 // m, m/s
	at := func(r, s float64) *vec3 {
		a := s / R // angle along the circle, driving anticlockwise seen from above (x east, z north)
		return &vec3{r * math.Cos(a), 0, r * math.Sin(a)}
	}
	bad := 0
	checked := 0
	for i := 0; i < 400; i++ {
		now := base.Add(time.Duration(i) * 100 * time.Millisecond)
		game := float64(i/2*2) * 0.1 // what LMU reports: only updated every 200 ms
		s := speed * game
		// anticlockwise: the outside of the circle is on your right
		me, friend := at(R, s), at(R+8, s)
		v.mu.Lock()
		v.me.update(me, gameLMU, now)
		if h, ok := v.me.heading(); ok {
			v.myHead, v.haveHead = h, true
		}
		v.myPos, v.myGame = me, gameLMU
		p.pos, p.game, p.posSeen = friend, gameLMU, now
		p.mt.update(friend, gameLMU, now)
		pan, _ := v.dirFrom(v.mePosLocked(now), v.myHead, gameLMU, p.mt.now(now))
		v.mu.Unlock()
		if i > 10 {
			checked++
			if pan < 0.8 {
				bad++
				if bad < 5 {
					t.Logf("t=%.1fs pan %.2f", float64(i)*0.1, pan)
				}
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d of %d checks had the friend not clearly on the right", bad, checked)
	}
}

// Prediction: a car passing you at 30 m/s more than you should be placed where it is
// now, not where LMU said it was up to 200 ms ago.
func TestMotionPredictsBetweenUpdates(t *testing.T) {
	var m motion
	base := time.Now()
	for i := 0; i <= 10; i++ {
		m.update(&vec3{30 * 0.2 * float64(i), 0, 0}, gameLMU, base.Add(time.Duration(i)*200*time.Millisecond))
	}
	got := m.now(base.Add(2*time.Second + 150*time.Millisecond))
	if math.Abs(got.X-(60+4.5)) > 0.2 {
		t.Fatalf("predicted x %.2f, want 64.5", got.X)
	}
	if h, ok := m.heading(); !ok || h[0] < 0.99 {
		t.Fatalf("heading %v %v", h, ok)
	}
}
