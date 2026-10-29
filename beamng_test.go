package main

import (
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"
)

func bngPacket(x, y, z float32) []byte {
	b := make([]byte, 88)
	copy(b, "BNG1")
	binary.LittleEndian.PutUint32(b[4:], math.Float32bits(x))
	binary.LittleEndian.PutUint32(b[8:], math.Float32bits(y))
	binary.LittleEndian.PutUint32(b[12:], math.Float32bits(z))
	return b
}

func TestBeamNGListener(t *testing.T) {
	BeamNGPort = 44455
	stop := make(chan struct{})
	defer close(stop)
	l := startBeamNGListener(stop, t.Logf)
	if l == nil {
		t.Fatal("listener didn't start")
	}
	c, _ := net.Dial("udp4", "127.0.0.1:44455")
	c.Write([]byte("junk"))
	c.Write(bngPacket(12.5, -3, 100))
	waitFor(t, "position", 2*time.Second, func() bool { return l.recent(time.Second) != nil })
	p := l.recent(time.Second)
	if p.X != 12.5 || p.Y != -3 || p.Z != 100 {
		t.Fatalf("got %+v", p)
	}
	if l.recent(0) != nil {
		t.Fatal("stale position returned")
	}
}

func TestDifferentGamesNoDistance(t *testing.T) {
	v := NewVoice(t.Logf, func() {})
	v.myPos, v.myGame = &vec3{}, gameBeamNG
	pk := make([]byte, 26)
	pk[0], pk[1] = vkPos, gameLMU
	binary.BigEndian.PutUint64(pk[2:], math.Float64bits(10))
	v.OnPacket(7, pk)
	if d := v.Status().Peers[7].Dist; d != -1 {
		t.Fatalf("LMU vs BeamNG should have no distance, got %v", d)
	}
	pk[1] = gameBeamNG
	v.OnPacket(7, pk)
	if d := v.Status().Peers[7].Dist; d != 10 {
		t.Fatalf("same game distance = %v", d)
	}
}
