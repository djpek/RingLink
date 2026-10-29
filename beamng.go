package main

// BeamNG.drive position: the game's "Motion Simulator" UDP output (enable "MotionSim" in
// Options > Other, IP 127.0.0.1, port BeamNGPort). Packets are 88
// bytes: "BNG1" then little-endian float32s, the first three being the
// vehicle's world position. In BeamMP everyone on the same map shares one
// coordinate system, so distances between players are real distances.

import (
	"encoding/binary"
	"math"
	"net"
	"sync"
	"time"
)

var BeamNGPort = 4444

type beamngListener struct {
	mu   sync.Mutex
	pos  *vec3
	seen time.Time
}

func (b *beamngListener) recent(d time.Duration) *vec3 {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pos == nil || time.Since(b.seen) > d {
		return nil
	}
	p := *b.pos
	return &p
}

func parseBeamNG(pkt []byte) (*vec3, bool) {
	if len(pkt) < 16 || string(pkt[:4]) != "BNG1" {
		return nil, false
	}
	f := func(o int) float64 { return float64(math.Float32frombits(binary.LittleEndian.Uint32(pkt[o:]))) }
	p := &vec3{f(4), f(8), f(12)}
	if math.IsNaN(p.X) || math.IsNaN(p.Y) || math.IsNaN(p.Z) {
		return nil, false
	}
	return p, true
}

// startBeamNGListener listens until stop is closed. If the port is taken
// (another telemetry app), BeamNG proximity just stays off.
func startBeamNGListener(stop chan struct{}, logf func(string, ...any)) *beamngListener {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: BeamNGPort})
	if err != nil {
		if logf != nil {
			logf("BeamNG positions unavailable: port %d is in use (%v).", BeamNGPort, err)
		}
		return nil
	}
	b := &beamngListener{}
	go func() { <-stop; c.Close() }()
	go func() {
		buf := make([]byte, 256)
		for {
			n, _, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if p, ok := parseBeamNG(buf[:n]); ok {
				b.mu.Lock()
				b.pos, b.seen = p, time.Now()
				b.mu.Unlock()
			}
		}
	}()
	return b
}
