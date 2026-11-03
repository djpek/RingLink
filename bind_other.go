//go:build !windows

package main

// Linux: wheel / joystick buttons through the joystick devices (/dev/input/js*), which
// normal users can read. Global keyboard keys would need root, so they aren't offered.

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type jsInput struct {
	mu    sync.Mutex
	down  map[string]bool
	open  map[string]bool
	check time.Time
}

func newInputPoller() *jsInput { return &jsInput{down: map[string]bool{}, open: map[string]bool{}} }

func (j *jsInput) pressed() map[string]bool {
	if time.Since(j.check) > 2*time.Second {
		j.check = time.Now()
		devs, _ := filepath.Glob("/dev/input/js*")
		for _, d := range devs {
			j.mu.Lock()
			isOpen := j.open[d]
			j.mu.Unlock()
			if isOpen {
				continue
			}
			var n int
			if _, err := fmt.Sscanf(filepath.Base(d), "js%d", &n); err != nil {
				continue
			}
			f, err := os.Open(d)
			if err != nil {
				continue
			}
			j.mu.Lock()
			j.open[d] = true
			j.mu.Unlock()
			go j.read(d, n, f)
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string]bool, len(j.down))
	for k, v := range j.down {
		if v {
			out[k] = true
		}
	}
	return out
}

// js_event: u32 time, s16 value, u8 type, u8 number; type 1 = button (0x80 = initial state)
func (j *jsInput) read(path string, n int, f *os.File) {
	defer f.Close()
	ev := make([]byte, 8)
	for {
		if _, err := f.Read(ev); err != nil {
			break
		}
		if ev[6]&^0x80 != 1 {
			continue
		}
		id := fmt.Sprintf("joy%d:b%d", n, ev[7])
		j.mu.Lock()
		j.down[id] = int16(binary.LittleEndian.Uint16(ev[4:])) != 0
		j.mu.Unlock()
	}
	j.mu.Lock()
	delete(j.open, path)
	for k := range j.down {
		if strings.HasPrefix(k, fmt.Sprintf("joy%d:", n)) {
			delete(j.down, k)
		}
	}
	j.mu.Unlock()
}

func bindCaption() string {
	return "Hold it and everyone hears you at full volume, wherever they are on track. Use a button on your wheel / button box (keyboard keys can't be read while the game is focused on Linux)."
}
