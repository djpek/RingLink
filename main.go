package main

import (
	"os"

	"gioui.org/app"
)

func main() {
	go func() {
		runUI()
		os.Exit(0)
	}()
	app.Main()
}
