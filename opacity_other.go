//go:build !linux && !windows

package main

import "gioui.org/app"

func setWindowOpacity(v app.ViewEvent, frac float64) bool { return false }
