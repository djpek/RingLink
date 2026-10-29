// SPDX-License-Identifier: Unlicense OR MIT

package app

// RingLink patch: per-pixel window transparency.

// TransparentBackground asks for windows with an alpha channel, cleared to
// fully transparent each frame, so anything not painted shows the desktop
// (needs a compositor). Set it before creating windows. Currently X11 only.
var TransparentBackground bool

// TransparentActive reports whether the last X11 window really got a 32-bit
// visual with alpha.
var TransparentActive bool

// x11ARGBVisual is the visual id of the ARGB window, for the EGL config choice.
var x11ARGBVisual int
