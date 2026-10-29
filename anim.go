package main

// Small animation helpers: values glide toward their targets each frame and
// ask for another frame until they've settled.

import (
	"image"
	"image/color"
	"math"
	"time"

	"gioui.org/f32"
	"gioui.org/layout"
	"gioui.org/op"
)

type tween struct {
	v    float32
	last time.Time
	init bool
}

type animator struct {
	vals    map[any]*tween
	springs map[any]*spring
	start   time.Time
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

// ---------- springs (bouncy scale) ----------

type spring struct {
	x, v float32
	last time.Time
}

// bounce moves a scale factor toward target on a spring. Growing is a little
// under-damped, so it overshoots and settles (the "pop"); shrinking back is
// critically damped, so it just eases down to normal size.
func (a *animator) bounce(gtx layout.Context, key any, target float32) float32 {
	if a.springs == nil {
		a.springs = map[any]*spring{}
	}
	s := a.springs[key]
	if s == nil {
		s = &spring{x: target, last: gtx.Now}
		a.springs[key] = s
		return target
	}
	dt := float32(gtx.Now.Sub(s.last).Seconds())
	s.last = gtx.Now
	if dt > 0.05 {
		dt = 1.0 / 60
	}
	const k = 520 // stiffness
	zeta := float32(0.38)
	if target < s.x {
		zeta = 1
	}
	c := 2 * zeta * float32(math.Sqrt(k))
	for steps := int(dt*480) + 1; steps > 0; steps-- {
		h := dt / float32(int(dt*480)+1)
		acc := -k*(s.x-target) - c*s.v
		s.v += acc * h
		s.x += s.v * h
	}
	if d := s.x - target; d < 0.0005 && d > -0.0005 && s.v < 0.002 && s.v > -0.002 {
		s.x, s.v = target, 0
	} else {
		gtx.Execute(op.InvalidateCmd{})
	}
	return s.x
}

// scaleAbout returns a transform that scales by s around the centre of size.
func scaleAbout(size image.Point, s float32) f32.Affine2D {
	c := f32.Pt(float32(size.X)/2, float32(size.Y)/2)
	return f32.Affine2D{}.Scale(c, f32.Pt(s, s))
}
