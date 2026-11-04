//go:build !windows

package main

// Linux: wheel / joystick buttons through the joystick devices (/dev/input/js*), which
// normal users can read, plus keyboard keys from the X server (bind_x11.go).

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
	out := make(map[string]bool, len(j.down)+2)
	x11Keys(out)
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
	if haveKeyboard {
		return "Push-to-broadcast: everyone hears you at full volume, wherever they are on track. Keys and wheel / button-box buttons all work while the game is focused."
	}
	return "Push-to-broadcast: everyone hears you at full volume, wherever they are on track. Use buttons on your wheel / button box (keyboard keys need an X11 desktop)."
}

func hidName(string) string { return "" }
