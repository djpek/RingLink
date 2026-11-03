//go:build linux && cgo

package main

// Keyboard keys on Linux: ask the X server which keys are down (works while the game
// has focus, no root needed). Wayland sessions get nothing here; wheel buttons still work.

/*
#cgo LDFLAGS: -lX11
#include <X11/Xlib.h>
#include <X11/XKBlib.h>
#include <stdlib.h>

static Display *rl_dpy;
static int rl_open(void) { if (!rl_dpy) rl_dpy = XOpenDisplay(NULL); return rl_dpy != NULL; }
static void rl_keys(char *out) { XQueryKeymap(rl_dpy, out); }
static const char *rl_name(int kc) {
	KeySym ks = XkbKeycodeToKeysym(rl_dpy, kc, 0, 0);
	return ks ? XKeysymToString(ks) : NULL;
}
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

var x11Once sync.Once
var x11OK bool

func x11Keys(down map[string]bool) {
	x11Once.Do(func() { x11OK = C.rl_open() != 0 })
	if !x11OK {
		return
	}
	var km [32]C.char
	C.rl_keys((*C.char)(unsafe.Pointer(&km[0])))
	for i := 0; i < 32; i++ {
		b := byte(km[i])
		for bit := 0; bit < 8; bit++ {
			if b&(1<<bit) != 0 {
				down[fmt.Sprintf("xkey:%d", i*8+bit)] = true
			}
		}
	}
}

func x11KeyName(kc int) string {
	if !x11OK {
		return ""
	}
	n := C.rl_name(C.int(kc))
	if n == nil {
		return ""
	}
	return C.GoString(n)
}

const haveKeyboard = true
