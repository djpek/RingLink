package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func TestCodes(t *testing.T) {
	for i := 0; i < 50; i++ {
		c := NewRoomCode()
		if len(c) != 9 || c[4] != '-' {
			t.Fatal(c)
		}
		n, err := NormalizeCode(" " + c[:4] + " " + c[5:] + " ")
		if err != nil || FormatCode(n) != c {
			t.Fatal(c, n, err)
		}
	}
	if n, err := NormalizeCode("k7qm-o9il"); err != nil || n != "K7QM0911" {
		t.Fatal(n, err)
	}
	if _, err := NormalizeCode("ABC"); err == nil {
		t.Fatal("short code accepted")
	}
	k1, t1 := roomSecrets("K7QM0911")
	k2, t2 := roomSecrets("K7QM0912")
	if string(k1) == string(k2) || t1 == t2 {
		t.Fatal("codes not distinct")
	}
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func connectedCount(r *Room) int {
	n := 0
	for _, m := range r.Members() {
		if !m.Self && m.Connected {
			n++
		}
	}
	return n
}

func memSig(h *memHub, code string) Signaler {
	c, _ := NormalizeCode(code)
	_, topic := roomSecrets(c)
	return &memSignal{hub: h, topic: topic}
}

func TestRoomMesh(t *testing.T) {
	stunServers = nil // no internet needed: LAN addresses only
	h := newMemHub()
	code := NewRoomCode()
	logf := func(f string, v ...any) { t.Logf(f, v...) }
	a, err := OpenRoom(code, "alice", 3, true, memSig(h, code), logf)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Leave()
	waitFor(t, "creator in room", 10*time.Second, func() bool { s, _ := a.State(); return s == "in room" })

	var mu sync.Mutex
	got := map[string]int{}
	a.OnVoice = func(from uint64, p []byte) { mu.Lock(); got[string(p)]++; mu.Unlock() }

	b, _ := OpenRoom(code, "bob", 0, false, memSig(h, code), logf)
	defer b.Leave()
	c, _ := OpenRoom(code, "carl", 0, false, memSig(h, code), logf)
	defer c.Leave()
	waitFor(t, "full mesh", 20*time.Second, func() bool {
		return connectedCount(a) == 2 && connectedCount(b) == 2 && connectedCount(c) == 2
	})
	if _, max := b.State(); max != 3 {
		t.Fatalf("joiner didn't adopt room size: %d", max)
	}
	for _, m := range c.Members() {
		if !m.Self && m.Name != "alice" && m.Name != "bob" {
			t.Fatalf("bad name %q", m.Name)
		}
	}
	b.SendVoice([]byte("from-bob"))
	c.SendVoice([]byte("from-carl"))
	waitFor(t, "voice delivered", 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got["from-bob"] == 1 && got["from-carl"] == 1
	})

	// room of 3 is full: a fourth person is turned away
	d, _ := OpenRoom(code, "dave", 0, false, memSig(h, code), logf)
	defer d.Leave()
	waitFor(t, "room full", 15*time.Second, func() bool { s, _ := d.State(); return s == "room is full (3 max)" })

	// someone leaves: the others notice
	c.Leave()
	waitFor(t, "leave noticed", 10*time.Second, func() bool { return len(a.Members()) == 2 && len(b.Members()) == 2 })

	// wrong code sees nobody
	w, _ := OpenRoom(NewRoomCode(), "eve", 0, false, memSig(h, code), logf)
	defer w.Leave()
	time.Sleep(5 * time.Second)
	if connectedCount(w) != 0 || len(w.Members()) != 1 {
		t.Fatal("wrong code joined")
	}
}

// Real relay test (needs internet): RINGLINK_NTFY=1 go test -run TestRealRelay
func TestRealRelay(t *testing.T) {
	if os.Getenv("RINGLINK_NTFY") == "" {
		t.Skip("set RINGLINK_NTFY=1 to test against ntfy.sh")
	}
	stunServers = nil
	code := NewRoomCode()
	logf := func(f string, v ...any) { t.Logf(f, v...) }
	a, _ := OpenRoom(code, "alice", 4, true, nil, logf)
	defer a.Leave()
	waitFor(t, "creator in room", 15*time.Second, func() bool { s, _ := a.State(); return s == "in room" })
	time.Sleep(2 * time.Second)
	b, _ := OpenRoom(code, "bob", 0, false, nil, logf)
	defer b.Leave()
	waitFor(t, "connected via ntfy", 30*time.Second, func() bool { return connectedCount(a) == 1 && connectedCount(b) == 1 })
	_ = context.Background
}
