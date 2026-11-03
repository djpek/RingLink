//go:build linux && !fakeaudio

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// shrinkPipe sets the pipe to its minimum size (one page, 128 ms of 16 kHz mono)
func shrinkPipe(f *os.File) { unix.FcntlInt(f.Fd(), unix.F_SETPIPE_SZ, 4096) }

// pipeBacklog returns how many bytes are waiting to be read
func pipeBacklog(f *os.File) int {
	n, err := unix.IoctlGetInt(int(f.Fd()), unix.TIOCINQ)
	if err != nil {
		return 0
	}
	return n
}
