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
*/
import "C"

import (
	"gioui.org/app"
)

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
