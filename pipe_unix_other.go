//go:build !linux && !windows && !fakeaudio

package main

import "os"

func shrinkPipe(f *os.File)      {}
func pipeBacklog(f *os.File) int { return 0 }
