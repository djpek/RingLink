package main

// Push-to-broadcast: hold a key or a wheel/joystick button and everyone in the room
// hears you at full volume, centred, wherever their car is (like being in the garage).

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// actions you can bind a key / wheel button to
const (
	actBroadcast = "broadcast" // everyone hears you at full volume while held
	actPTT       = "ptt"       // push to talk
	actPTM       = "ptm"       // push to mute
)

// Binder watches keyboard / controller buttons and reports which bound actions are held.
type Binder struct {
	mu        sync.Mutex
	binds     map[string]string // action -> button id, e.g. "joy0:b5", "key:66"
	capFor    string            // action waiting for its next button press ("" = none)
	capBase   map[string]bool
	capUntil  time.Time
	held      map[string]bool
	onChange  func(action string, held bool)
	onCapture func(action, id string)
}

func NewBinder() *Binder { return &Binder{binds: map[string]string{}, held: map[string]bool{}} }

func (b *Binder) Bind(action string) string { b.mu.Lock(); defer b.mu.Unlock(); return b.binds[action] }
func (b *Binder) SetBind(action, id string) {
	b.mu.Lock()
	if id == "" {
		delete(b.binds, action)
	} else {
		b.binds[action] = id
	}
	b.mu.Unlock()
}

// CapturingFor: the action waiting for a button press, or "".
func (b *Binder) CapturingFor() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capFor != "" && time.Now().Before(b.capUntil) {
		return b.capFor
	}
	return ""
}

// Capture: the next button pressed (within 8 s) becomes action's binding.
func (b *Binder) Capture(action string) {
	b.mu.Lock()
	b.capFor, b.capBase, b.capUntil = action, nil, time.Now().Add(8*time.Second)
	b.mu.Unlock()
}
func (b *Binder) CancelCapture() { b.mu.Lock(); b.capFor = ""; b.mu.Unlock() }

// step handles one poll of what's pressed right now.
func (b *Binder) step(down map[string]bool) {
	b.mu.Lock()
	var capAct, captured string
	if b.capFor != "" {
		if time.Now().After(b.capUntil) {
			b.capFor = ""
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
				capAct, captured = b.capFor, fresh[0]
				b.binds[capAct] = captured
				b.capFor = ""
				b.capBase = map[string]bool{captured: true} // don't fire until it's let go
			}
		}
	}
	type chg struct {
		act  string
		held bool
	}
	var changes []chg
	for _, act := range []string{actBroadcast, actPTT, actPTM} {
		id := b.binds[act]
		held := id != "" && b.capFor == "" && down[id] && !b.capBase[id]
		if held != b.held[act] {
			b.held[act] = held
			changes = append(changes, chg{act, held})
		}
	}
	if b.capFor == "" && len(b.capBase) > 0 {
		for id := range b.capBase {
			if !down[id] {
				delete(b.capBase, id)
			}
		}
	}
	onC, onCap := b.onChange, b.onCapture
	b.mu.Unlock()
	if captured != "" && onCap != nil {
		onCap(capAct, captured)
	}
	for _, c := range changes {
		if onC != nil {
			onC(c.act, c.held)
		}
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
	case strings.HasPrefix(id, "hid:"):
		parts := strings.Split(id, ":")
		if len(parts) == 3 {
			var btn int
			if n, _ := sscan1(parts[2], "b%d", &btn); n == 1 {
				dev := hidName(parts[1])
				if dev == "" {
					dev = "Controller"
				}
				return dev + " button " + itoa(btn)
			}
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
