//go:build linux

package main

/*
#cgo LDFLAGS: -lX11
#include <X11/Xlib.h>
#include <X11/Xatom.h>

static void set_opacity(void *dpy, unsigned long win, unsigned long val, int opaque) {
	Display *d = (Display *)dpy;
	Atom a = XInternAtom(d, "_NET_WM_WINDOW_OPACITY", False);
	if (opaque) {
		XDeleteProperty(d, (Window)win, a);
	} else {
		XChangeProperty(d, (Window)win, a, XA_CARDINAL, 32, PropModeReplace, (unsigned char *)&val, 1);
	}
	XFlush(d);
}

// KDE's blur-behind hint: an empty region means "blur behind the whole window".
static int set_blur(void *dpy, unsigned long win, int on) {
	Display *d = (Display *)dpy;
	Atom a = XInternAtom(d, "_KDE_NET_WM_BLUR_BEHIND_REGION", False);
	if (on) {
		XChangeProperty(d, (Window)win, a, XA_CARDINAL, 32, PropModeReplace, NULL, 0);
	} else {
		XDeleteProperty(d, (Window)win, a);
	}
	XFlush(d);
	return 1;
}
*/
import "C"

import (
	"os"
	"strings"

	"gioui.org/app"
)

func isKDE() bool {
	return strings.Contains(strings.ToUpper(os.Getenv("XDG_CURRENT_DESKTOP")), "KDE")
}

// setWindowOpacity makes the whole window see-through (X11 compositors such
// as Cinnamon/Muffin, Mutter, KWin, Xfwm and picom honour this property).
// Returns false where it can't be done (e.g. a native Wayland window).
func setWindowOpacity(v app.ViewEvent, frac float64) bool {
	x, ok := v.(app.X11ViewEvent)
	if !ok || x.Display == nil || x.Window == 0 {
		return false
	}
	val := C.ulong(frac * 0xffffffff)
	opaque := C.int(0)
	if frac >= 0.999 {
		opaque = 1
	}
	C.set_opacity(x.Display, C.ulong(x.Window), val, opaque)
	return true
}

// setBlurBehind asks the compositor to blur what's behind the window. Only some
// desktops do this (KDE Plasma); Cinnamon/Mint ignore it. Returns true when
// the desktop says it supports it.
func setBlurBehind(v app.ViewEvent, on bool) bool {
	x, ok := v.(app.X11ViewEvent)
	if !ok || x.Display == nil || x.Window == 0 {
		return false
	}
	C.set_blur(x.Display, C.ulong(x.Window), boolInt(on))
	return isKDE()
}

func boolInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}
