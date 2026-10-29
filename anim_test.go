package main

import (
	"testing"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
)

// Growing overshoots a little then settles; shrinking eases back without undershoot.
func TestBounce(t *testing.T) {
	var a animator
	var ops op.Ops
	now := time.Unix(0, 0)
	gtx := layout.Context{Ops: &ops, Now: now}
	key := new(int)
	a.bounce(gtx, key, 1)
	max, v := float32(0), float32(0)
	for i := 0; i < 120; i++ { // 2 s at 60 fps
		now = now.Add(time.Second / 60)
		gtx.Now = now
		v = a.bounce(gtx, key, 1.05)
		if v > max {
			max = v
		}
	}
	if max < 1.055 || max > 1.08 {
		t.Fatalf("overshoot peak %.4f, want a small pop above 1.05", max)
	}
	if v < 1.0495 || v > 1.0505 {
		t.Fatalf("didn't settle at 1.05: %.4f", v)
	}
	min := float32(9)
	for i := 0; i < 120; i++ {
		now = now.Add(time.Second / 60)
		gtx.Now = now
		v = a.bounce(gtx, key, 1)
		if v < min {
			min = v
		}
	}
	if min < 0.999 || v != 1 {
		t.Fatalf("shrink undershot or didn't settle: min %.4f end %.4f", min, v)
	}
}
