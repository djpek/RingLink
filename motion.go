package main

import (
	"math"
	"time"
)

// motion follows one car from its position samples: which way it's really going
// (in world coordinates, unlike LMU's carVelocity, which can be relative to the car)
// and where it most likely is right now. LMU only updates positions 5 times a second,
// so between samples the position is moved forward along the car's velocity.
type motion struct {
	pos   *vec3     // last new sample
	at    time.Time // when it arrived
	vel   vec3      // m/s, world frame
	velOK bool
	game  byte
}

const maxLead = 400 * time.Millisecond // never guess further ahead than this

func (m *motion) reset() { *m = motion{} }

// update with a sample seen at now. Repeats of the same position (polled faster than
// the game updates it) are ignored so the velocity isn't dragged toward zero.
func (m *motion) update(p *vec3, game byte, now time.Time) {
	if p == nil {
		m.reset()
		return
	}
	if m.pos == nil || game != m.game {
		*m = motion{pos: p, at: now, game: game}
		return
	}
	if *p == *m.pos {
		return
	}
	dt := now.Sub(m.at).Seconds()
	nv := vec3{(p.X - m.pos.X) / dt, (p.Y - m.pos.Y) / dt, (p.Z - m.pos.Z) / dt}
	sp := math.Sqrt(nv.X*nv.X + nv.Y*nv.Y + nv.Z*nv.Z)
	switch {
	case dt <= 0.03 || dt > 1.5 || sp > 150: // burst / long gap / teleport: restart from here
		m.velOK = false
		if sp > 150 || dt > 1.5 {
			m.vel = vec3{}
		}
	case !m.velOK:
		m.vel, m.velOK = nv, true
	default:
		m.vel = vec3{0.6*nv.X + 0.4*m.vel.X, 0.6*nv.Y + 0.4*m.vel.Y, 0.6*nv.Z + 0.4*m.vel.Z}
	}
	m.pos, m.at = p, now
}

// now: best guess of the position at time t.
func (m *motion) now(t time.Time) *vec3 {
	if m.pos == nil {
		return nil
	}
	if !m.velOK {
		return m.pos
	}
	lead := t.Sub(m.at)
	if lead > maxLead {
		lead = maxLead
	} else if lead < 0 {
		lead = 0
	}
	s := lead.Seconds()
	return &vec3{m.pos.X + m.vel.X*s, m.pos.Y + m.vel.Y*s, m.pos.Z + m.vel.Z*s}
}

// heading: unit direction of travel in the horizontal plane, once moving.
func (m *motion) heading() ([2]float64, bool) {
	if !m.velOK || m.pos == nil {
		return [2]float64{}, false
	}
	h := horiz(&m.vel, m.game)
	n := math.Hypot(h[0], h[1])
	if n < 2 {
		return [2]float64{}, false
	}
	return [2]float64{h[0] / n, h[1] / n}, true
}
