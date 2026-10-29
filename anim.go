package main

// Small animation helpers: values glide toward their targets each frame and
// ask for another frame until they've settled.

import (
	"image/color"
	"math"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
)

type tween struct {
	v    float32
	last time.Time
	init bool
}

type animator struct {
	vals  map[any]*tween
	start time.Time
}

// to moves the value for key toward target. rate is roughly "1/seconds";
// 12-18 feels snappy.
func (a *animator) to(gtx layout.Context, key any, target, rate float32) float32 {
	if a.vals == nil {
		a.vals = map[any]*tween{}
	}
	t := a.vals[key]
	if t == nil {
		t = &tween{v: target, last: gtx.Now, init: true}
		a.vals[key] = t
		return target
	}
	dt := float32(gtx.Now.Sub(t.last).Seconds())
	t.last = gtx.Now
	if dt > 0.05 {
		dt = 1.0 / 60 // first frame after being idle: start the motion gently
	}
	t.v += (target - t.v) * (1 - float32(math.Exp(float64(-dt*rate))))
	if d := target - t.v; d > 0.002 || d < -0.002 {
		gtx.Execute(op.InvalidateCmd{})
	} else {
		t.v = target
	}
	return t.v
}

// intro returns 0..1 for an element appearing delay seconds after start.
func (a *animator) intro(gtx layout.Context, delay, dur float64) float32 {
	if a.start.IsZero() {
		a.start = gtx.Now
	}
	p := (gtx.Now.Sub(a.start).Seconds() - delay) / dur
	if p < 1 {
		gtx.Execute(op.InvalidateCmd{})
	}
	if p <= 0 {
		return 0
	}
	if p >= 1 {
		return 1
	}
	return float32(1 - math.Pow(1-p, 3)) // ease-out cubic
}

// pulse is a 0..1 wave (for breathing dots and talking rings); keeps frames coming.
func pulse(gtx layout.Context, hz float64) float32 {
	gtx.Execute(op.InvalidateCmd{})
	s := float64(gtx.Now.UnixNano()) / 1e9
	return float32(0.5 + 0.5*math.Sin(s*hz*2*math.Pi))
}

func lerpC(a, b color.NRGBA, t float32) color.NRGBA {
	l := func(x, y uint8) uint8 { return uint8(float32(x) + (float32(y)-float32(x))*t) }
	return color.NRGBA{l(a.R, b.R), l(a.G, b.G), l(a.B, b.B), l(a.A, b.A)}
}

// set jumps a value (e.g. to restart a fade).
func (a *animator) set(key any, v float32) {
	if a.vals == nil {
		a.vals = map[any]*tween{}
	}
	if t := a.vals[key]; t != nil {
		t.v = v
	} else {
		a.vals[key] = &tween{v: v, last: time.Now(), init: true}
	}
}
