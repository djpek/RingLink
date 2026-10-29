//go:build windows

package main

// Windows audio through the classic waveIn/waveOut API (winmm.dll), which needs
// no C compiler and uses the default recording/playback devices set in Windows.

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	winmm                = windows.NewLazySystemDLL("winmm.dll")
	pWaveInOpen          = winmm.NewProc("waveInOpen")
	pWaveInPrepareHeader = winmm.NewProc("waveInPrepareHeader")
	pWaveInUnprepare     = winmm.NewProc("waveInUnprepareHeader")
	pWaveInAddBuffer     = winmm.NewProc("waveInAddBuffer")
	pWaveInStart         = winmm.NewProc("waveInStart")
	pWaveInReset         = winmm.NewProc("waveInReset")
	pWaveInClose         = winmm.NewProc("waveInClose")
	pWaveOutOpen         = winmm.NewProc("waveOutOpen")
	pWaveOutPrepare      = winmm.NewProc("waveOutPrepareHeader")
	pWaveOutUnprepare    = winmm.NewProc("waveOutUnprepareHeader")
	pWaveOutWrite        = winmm.NewProc("waveOutWrite")
	pWaveOutReset        = winmm.NewProc("waveOutReset")
	pWaveOutClose        = winmm.NewProc("waveOutClose")
	pWaveInGetNumDevs    = winmm.NewProc("waveInGetNumDevs")
	pWaveInGetDevCapsW   = winmm.NewProc("waveInGetDevCapsW")
	pWaveOutGetNumDevs   = winmm.NewProc("waveOutGetNumDevs")
	pWaveOutGetDevCapsW  = winmm.NewProc("waveOutGetDevCapsW")
)

const (
	waveMapper   = 0xFFFFFFFF
	whdrDone     = 0x1
	nBufs        = 4
	frameBytes   = voiceFrame * 2
	mmsyserrNone = 0
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CbSize         uint16
}

type waveHdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

func pcmFormat() *waveFormatEx {
	return &waveFormatEx{FormatTag: 1, Channels: 1, SamplesPerSec: voiceRate,
		AvgBytesPerSec: voiceRate * 2, BlockAlign: 2, BitsPerSample: 16}
}

// buffers live in memory allocated outside the Go heap so Windows can write
// to them at any time.
type winBufs struct {
	mem  uintptr
	hdrs [nBufs]*waveHdr
	data [nBufs][]int16
}

func newWinBufs() (*winBufs, error) {
	hsz := unsafe.Sizeof(waveHdr{})
	total := uintptr(nBufs) * (hsz + frameBytes)
	mem, err := windows.VirtualAlloc(0, total, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if err != nil {
		return nil, err
	}
	b := &winBufs{mem: mem}
	for i := 0; i < nBufs; i++ {
		h := (*waveHdr)(unsafe.Pointer(mem + uintptr(i)*hsz))
		d := mem + uintptr(nBufs)*hsz + uintptr(i)*frameBytes
		h.Data = d
		h.BufferLength = frameBytes
		b.hdrs[i] = h
		b.data[i] = unsafe.Slice((*int16)(unsafe.Pointer(d)), voiceFrame)
	}
	return b, nil
}

func (b *winBufs) free() { windows.VirtualFree(b.mem, 0, windows.MEM_RELEASE) }

func mmErr(name string, r uintptr) error {
	if r == mmsyserrNone {
		return nil
	}
	switch r {
	case 2:
		return fmt.Errorf("%s: no audio device found", name)
	case 4:
		return fmt.Errorf("%s: device is busy", name)
	case 32:
		return fmt.Errorf("%s: format not supported", name)
	}
	return fmt.Errorf("%s failed (code %d)", name, r)
}

// ---------- microphone ----------

type winMic struct {
	h    uintptr
	b    *winBufs
	next int
}

func openMic(device string) (micSource, string, error) {
	b, err := newWinBufs()
	if err != nil {
		return nil, "", err
	}
	m := &winMic{b: b}
	id, label := findDevice(listMics(), device, "Windows default mic")
	r, _, _ := pWaveInOpen.Call(uintptr(unsafe.Pointer(&m.h)), id, uintptr(unsafe.Pointer(pcmFormat())), 0, 0, 0)
	if err := mmErr("waveInOpen", r); err != nil {
		b.free()
		return nil, "", fmt.Errorf("%v (check Windows Settings > Privacy > Microphone allows desktop apps)", err)
	}
	for _, h := range b.hdrs {
		pWaveInPrepareHeader.Call(m.h, uintptr(unsafe.Pointer(h)), unsafe.Sizeof(*h))
		pWaveInAddBuffer.Call(m.h, uintptr(unsafe.Pointer(h)), unsafe.Sizeof(*h))
	}
	if r, _, _ := pWaveInStart.Call(m.h); r != 0 {
		m.Close()
		return nil, "", mmErr("waveInStart", r)
	}
	return m, label, nil
}

func (m *winMic) Read(frame []int16) error {
	h := m.b.hdrs[m.next]
	deadline := time.Now().Add(2 * time.Second)
	for h.Flags&whdrDone == 0 {
		if m.h == 0 {
			return errors.New("closed")
		}
		if time.Now().After(deadline) {
			return errors.New("microphone isn't delivering audio")
		}
		time.Sleep(3 * time.Millisecond)
	}
	copy(frame, m.b.data[m.next])
	h.Flags &^= whdrDone
	h.BytesRecorded = 0
	pWaveInAddBuffer.Call(m.h, uintptr(unsafe.Pointer(h)), unsafe.Sizeof(*h))
	m.next = (m.next + 1) % nBufs
	return nil
}

func (m *winMic) Close() {
	if m.h == 0 {
		return
	}
	h := m.h
	m.h = 0
	pWaveInReset.Call(h)
	for _, hd := range m.b.hdrs {
		pWaveInUnprepare.Call(h, uintptr(unsafe.Pointer(hd)), unsafe.Sizeof(*hd))
	}
	pWaveInClose.Call(h)
	m.b.free()
}

// ---------- speakers ----------

type winSpk struct {
	h      uintptr
	b      *winBufs
	next   int
	queued int
}

func openSpeaker(device string) (speakerSink, string, error) {
	b, err := newWinBufs()
	if err != nil {
		return nil, "", err
	}
	s := &winSpk{b: b}
	id, label := findDevice(listSpeakers(), device, "Windows default output")
	r, _, _ := pWaveOutOpen.Call(uintptr(unsafe.Pointer(&s.h)), id, uintptr(unsafe.Pointer(pcmFormat())), 0, 0, 0)
	if err := mmErr("waveOutOpen", r); err != nil {
		b.free()
		return nil, "", err
	}
	for _, h := range b.hdrs {
		pWaveOutPrepare.Call(s.h, uintptr(unsafe.Pointer(h)), unsafe.Sizeof(*h))
		h.Flags |= whdrDone // all free at start
	}
	return s, label, nil
}

func (s *winSpk) Write(frame []int16) error {
	h := s.b.hdrs[s.next]
	deadline := time.Now().Add(2 * time.Second)
	for h.Flags&whdrDone == 0 {
		if s.h == 0 {
			return errors.New("closed")
		}
		if time.Now().After(deadline) {
			return errors.New("audio output stalled")
		}
		time.Sleep(3 * time.Millisecond)
	}
	copy(s.b.data[s.next], frame)
	h.Flags &^= whdrDone
	if r, _, _ := pWaveOutWrite.Call(s.h, uintptr(unsafe.Pointer(h)), unsafe.Sizeof(*h)); r != 0 {
		return mmErr("waveOutWrite", r)
	}
	s.next = (s.next + 1) % nBufs
	return nil
}

func (s *winSpk) Close() {
	if s.h == 0 {
		return
	}
	h := s.h
	s.h = 0
	pWaveOutReset.Call(h)
	for _, hd := range s.b.hdrs {
		pWaveOutUnprepare.Call(h, uintptr(unsafe.Pointer(hd)), unsafe.Sizeof(*hd))
	}
	pWaveOutClose.Call(h)
	s.b.free()
}

// ---------- device lists ----------
// Devices are identified by name (indexes change when things are plugged in).

type waveCaps struct {
	Mid, Pid      uint16
	DriverVersion uint32
	Pname         [32]uint16
	Formats       uint32
	Channels      uint16
	Reserved1     uint16
	Support       uint32 // waveOut only; harmless extra space for waveIn
}

func listWave(num, caps *windows.LazyProc) []AudioDevice {
	n, _, _ := num.Call()
	var out []AudioDevice
	for i := uintptr(0); i < n; i++ {
		var c waveCaps
		if r, _, _ := caps.Call(i, uintptr(unsafe.Pointer(&c)), unsafe.Sizeof(c)); r != 0 {
			continue
		}
		name := windows.UTF16ToString(c.Pname[:])
		out = append(out, AudioDevice{ID: name, Name: name})
	}
	return out
}

func listMics() []AudioDevice     { return listWave(pWaveInGetNumDevs, pWaveInGetDevCapsW) }
func listSpeakers() []AudioDevice { return listWave(pWaveOutGetNumDevs, pWaveOutGetDevCapsW) }

// findDevice turns a saved device name into a winmm device index; unknown or
// empty names use the Windows default.
func findDevice(list []AudioDevice, want, def string) (uintptr, string) {
	if want != "" {
		for i, d := range list {
			if d.ID == want {
				return uintptr(i), d.Name
			}
		}
	}
	return waveMapper, def
}
