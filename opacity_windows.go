//go:build windows

package main

import (
	"gioui.org/app"
	"golang.org/x/sys/windows"
)

var (
	user32                      = windows.NewLazySystemDLL("user32.dll")
	pGetWindowLongPtrW          = user32.NewProc("GetWindowLongPtrW")
	pSetWindowLongPtrW          = user32.NewProc("SetWindowLongPtrW")
	pSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
)

const (
	gwlExStyle  = ^uintptr(19) // -20
	wsExLayered = 0x00080000
	lwaAlpha    = 0x00000002
)

// setWindowOpacity makes the whole window see-through using a layered window.
func setWindowOpacity(v app.ViewEvent, frac float64) bool {
	w, ok := v.(app.Win32ViewEvent)
	if !ok || w.HWND == 0 {
		return false
	}
	style, _, _ := pGetWindowLongPtrW.Call(w.HWND, gwlExStyle)
	if frac >= 0.999 {
		if style&wsExLayered != 0 {
			pSetWindowLongPtrW.Call(w.HWND, gwlExStyle, style&^wsExLayered)
		}
		return true
	}
	if style&wsExLayered == 0 {
		pSetWindowLongPtrW.Call(w.HWND, gwlExStyle, style|wsExLayered)
	}
	a := uintptr(frac * 255)
	r, _, _ := pSetLayeredWindowAttributes.Call(w.HWND, 0, a, lwaAlpha)
	return r != 0
}
