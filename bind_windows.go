//go:build windows

package main

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	u32bind           = windows.NewLazySystemDLL("user32.dll")
	pGetAsyncKeyState = u32bind.NewProc("GetAsyncKeyState")
	pJoyGetPosEx      = winmm.NewProc("joyGetPosEx")
)

type joyInfoEx struct {
	size, flags, x, y, z, r, u, v, buttons, buttonNum, pov, res1, res2 uint32
}

type winInput struct {
	joyOK   [16]bool
	recheck int
	hid     *hidReader
}

func newInputPoller() *winInput {
	w := &winInput{hid: newHIDReader()}
	for i := range w.joyOK {
		w.joyOK[i] = true
	}
	return w
}

func (w *winInput) pressed() map[string]bool {
	down := map[string]bool{}
	// keyboard + side mouse buttons; left/right/middle mouse (1, 2, 4) are skipped so clicking the app doesn't bind
	for vk := 5; vk <= 0xfe; vk++ {
		if vk == 0x10 || vk == 0x11 || vk == 0x12 { // generic Shift/Ctrl/Alt: the left/right ones are reported too
			continue
		}
		r, _, _ := pGetAsyncKeyState.Call(uintptr(vk))
		if r&0x8000 != 0 {
			down[fmt.Sprintf("key:%d", vk)] = true
		}
	}
	// wheels, button boxes, joysticks: up to 32 buttons each
	w.recheck++
	for j := 0; j < 16; j++ {
		if !w.joyOK[j] && w.recheck%130 != 0 { // look for newly plugged-in devices every ~2 s
			continue
		}
		var ji joyInfoEx
		ji.size = uint32(unsafe.Sizeof(ji))
		ji.flags = 0x80 // JOY_RETURNBUTTONS
		r, _, _ := pJoyGetPosEx.Call(uintptr(j), uintptr(unsafe.Pointer(&ji)))
		w.joyOK[j] = r == 0
		if r != 0 {
			continue
		}
		for b := 0; b < 32; b++ {
			if ji.buttons&(1<<b) != 0 {
				down[fmt.Sprintf("joy%d:b%d", j, b)] = true
			}
		}
	}
	w.hid.pressed(down) // every button of every wheel / button box, beyond the 32 above
	return down
}

func bindCaption() string {
	return "Push-to-broadcast: everyone (or just your sub-room, if you picked one) hears you at full volume, wherever they are. Keys and wheel / button-box buttons all work while the game is focused."
}

func x11KeyName(int) string { return "" }
