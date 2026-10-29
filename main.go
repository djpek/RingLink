package main

import (
	"os"
	"runtime"

	"gioui.org/app"
)

func main() {
	// Linux/X11: give the window an alpha channel so only the background can
	// be see-through (text and buttons stay solid). See third_party/gioui.
	app.TransparentBackground = runtime.GOOS == "linux"
	go func() {
		runUI()
		os.Exit(0)
	}()
	app.Main()
}
