package main

// Push-to-broadcast: hold a key or a wheel/joystick button and everyone in the room
// hears you at full volume, centred, wherever their car is (like being in the garage).

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Binder watches keyboard / controller buttons and reports whether the bound one is held.
type Binder struct {
	mu        sync.Mutex
	bind      string // e.g. "joy0:b5" or "key:66"; "" = none
	capturing bool   // waiting for the next button press to become the binding
	capBase   map[string]bool
	capUntil  time.Time
	held      bool
	onChange  func(held bool)
	onCapture func(id string)
}

func NewBinder() *Binder { return &Binder{} }

func (b *Binder) Bind() string { b.mu.Lock(); defer b.mu.Unlock(); return b.bind }
func (b *Binder) SetBind(id string) {
	b.mu.Lock()
	b.bind = id
	b.mu.Unlock()
}
func (b *Binder) Capturing() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.capturing && time.Now().Before(b.capUntil)
}

// Capture: the next button pressed (within 8 s) becomes the binding.
func (b *Binder) Capture() {
	b.mu.Lock()
	b.capturing, b.capBase, b.capUntil = true, nil, time.Now().Add(8*time.Second)
	b.mu.Unlock()
}
func (b *Binder) CancelCapture() { b.mu.Lock(); b.capturing = false; b.mu.Unlock() }

// step handles one poll of what's pressed right now.
func (b *Binder) step(down map[string]bool) {
	b.mu.Lock()
	var captured string
	if b.capturing {
		if time.Now().After(b.capUntil) {
			b.capturing = false
		} else if b.capBase == nil {
			b.capBase = down // ignore whatever was already held (e.g. the mouse click)
		} else {
			var fresh []string
			for id := range down {
				if !b.capBase[id] {
					fresh = append(fresh, id)
				}
			}
			for id := range b.capBase {
				if !down[id] {
					delete(b.capBase, id)
				}
			}
			if len(fresh) > 0 {
				sort.Strings(fresh)
				captured, b.bind, b.capturing = fresh[0], fresh[0], false
			}
		}
	}
	held := b.bind != "" && !b.capturing && captured == "" && down[b.bind]
	changed := held != b.held
	b.held = held
	onC, onCap := b.onChange, b.onCapture
	b.mu.Unlock()
	if captured != "" && onCap != nil {
		onCap(captured)
	}
	if changed && onC != nil {
		onC(held)
	}
}

func (b *Binder) Run(stop <-chan struct{}) {
	in := newInputPoller()
	t := time.NewTicker(15 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		b.step(in.pressed())
	}
}

// bindName turns an id into something readable.
func bindName(id string) string {
	switch {
	case id == "":
		return "Not set"
	case strings.HasPrefix(id, "joy"):
		var dev, btn int
		if n, _ := sscan2(id, "joy%d:b%d", &dev, &btn); n == 2 {
			return "Controller " + itoa(dev+1) + " button " + itoa(btn+1)
		}
	case strings.HasPrefix(id, "xkey:"):
		var kc int
		if n, _ := sscan1(id, "xkey:%d", &kc); n == 1 {
			if nm := x11KeyName(kc); nm != "" {
				return xName(nm)
			}
			return "Key " + itoa(kc)
		}
	case strings.HasPrefix(id, "key:"):
		var vk int
		if n, _ := sscan1(id, "key:%d", &vk); n == 1 {
			return keyName(vk)
		}
	}
	return id
}

func keyName(vk int) string {
	switch {
	case vk >= 'A' && vk <= 'Z', vk >= '0' && vk <= '9':
		return "Key " + string(rune(vk))
	case vk >= 0x70 && vk <= 0x87:
		return "F" + itoa(vk-0x6f)
	case vk >= 0x60 && vk <= 0x69:
		return "Numpad " + itoa(vk-0x60)
	}
	names := map[int]string{0x05: "Mouse 4", 0x06: "Mouse 5", 0x08: "Backspace", 0x09: "Tab", 0x0d: "Enter", 0x10: "Shift", 0x11: "Ctrl", 0x12: "Alt",
		0x14: "Caps Lock", 0x1b: "Esc", 0x20: "Space", 0x21: "Page Up", 0x22: "Page Down", 0x23: "End", 0x24: "Home",
		0x25: "Left", 0x26: "Up", 0x27: "Right", 0x28: "Down", 0x2d: "Insert", 0x2e: "Delete",
		0xa0: "Left Shift", 0xa1: "Right Shift", 0xa2: "Left Ctrl", 0xa3: "Right Ctrl", 0xa4: "Left Alt", 0xa5: "Right Alt",
		0xc0: "` key", 0xbd: "- key", 0xbb: "= key", 0xdb: "[ key", 0xdd: "] key", 0xdc: "\\ key", 0xba: "; key", 0xde: "' key",
		0xbc: ", key", 0xbe: ". key", 0xbf: "/ key", 0x6a: "Numpad *", 0x6b: "Numpad +", 0x6d: "Numpad -", 0x6e: "Numpad .", 0x6f: "Numpad /"}
	if s, ok := names[vk]; ok {
		return s
	}
	return "Key " + itoa(vk)
}

// xName tidies X keysym names: "a" -> "Key A", "Control_L" -> "Left Ctrl", "KP_1" -> "Numpad 1".
func xName(n string) string {
	if len(n) == 1 {
		return "Key " + strings.ToUpper(n)
	}
	side := ""
	if strings.HasSuffix(n, "_L") {
		side, n = "Left ", n[:len(n)-2]
	} else if strings.HasSuffix(n, "_R") {
		side, n = "Right ", n[:len(n)-2]
	}
	switch n {
	case "Control":
		n = "Ctrl"
	case "Super":
		n = "Windows key"
	case "Prior":
		n = "Page Up"
	case "Next":
		n = "Page Down"
	case "Return":
		n = "Enter"
	case "Escape":
		n = "Esc"
	case "grave":
		n = "` key"
	case "space":
		n = "Space"
	}
	if strings.HasPrefix(n, "KP_") {
		n = "Numpad " + n[3:]
	}
	return side + n
}
