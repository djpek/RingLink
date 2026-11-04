//go:build windows

package main

// Wheel / button-box / joystick buttons read straight from the HID device. The old
// joystick API (joyGetPosEx) only reports 32 buttons and doesn't list every device,
// so buttons on bigger wheels and button boxes never showed up. This reads every
// button of every game controller, with the game focused or not.

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	hidDLL                 = windows.NewLazySystemDLL("hid.dll")
	pHidD_GetHidGuid       = hidDLL.NewProc("HidD_GetHidGuid")
	pHidD_GetPreparsedData = hidDLL.NewProc("HidD_GetPreparsedData")
	pHidD_FreePreparsed    = hidDLL.NewProc("HidD_FreePreparsedData")
	pHidD_GetProductString = hidDLL.NewProc("HidD_GetProductString")
	pHidP_GetCaps          = hidDLL.NewProc("HidP_GetCaps")
	pHidP_GetUsages        = hidDLL.NewProc("HidP_GetUsages")
	pHidP_MaxUsageListLen  = hidDLL.NewProc("HidP_MaxUsageListLength")
)

const (
	hidpStatusSuccess = 0x00110000
	hidpInput         = 0
	usagePageGeneric  = 0x01
	usagePageButton   = 0x09
)

// HIDP_CAPS (only the start is used; the rest is padding to its real size)
type hidpCaps struct {
	Usage, UsagePage                                                uint16
	InputReportByteLength, OutputReportByteLength, FeatureReportLen uint16
	_                                                               [17 + 10]uint16
}

type hidReader struct {
	mu      sync.Mutex
	open    map[string]bool              // device paths being read
	down    map[string]map[byte][]uint16 // device id -> report id -> pressed buttons
	checked time.Time
}

var hidNames sync.Map // device id -> product name, for bindName

func newHIDReader() *hidReader {
	return &hidReader{open: map[string]bool{}, down: map[string]map[byte][]uint16{}}
}

// pressed adds the held controller buttons as "hid:<vid_pid>:b<n>".
func (h *hidReader) pressed(out map[string]bool) {
	if time.Since(h.checked) > 3*time.Second { // pick up newly plugged-in devices
		h.checked = time.Now()
		h.scan()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for dev, reps := range h.down {
		for _, btns := range reps {
			for _, b := range btns {
				out[fmt.Sprintf("hid:%s:b%d", dev, b)] = true
			}
		}
	}
}

func (h *hidReader) scan() {
	if hidDLL.Load() != nil {
		return
	}
	var g windows.GUID
	pHidD_GetHidGuid.Call(uintptr(unsafe.Pointer(&g)))
	paths, err := windows.CM_Get_Device_Interface_List("", &g, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil {
		return
	}
	for _, p := range paths {
		h.mu.Lock()
		busy := h.open[p]
		h.mu.Unlock()
		if !busy {
			h.try(p)
		}
	}
}

// try opens one HID device and, if it's a game controller with buttons, reads it.
func (h *hidReader) try(path string) {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	hd, err := windows.CreateFile(p16, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return // keyboards and mice can't be opened (and don't need to be)
	}
	var pp uintptr
	if r, _, _ := pHidD_GetPreparsedData.Call(uintptr(hd), uintptr(unsafe.Pointer(&pp))); r == 0 {
		windows.CloseHandle(hd)
		return
	}
	var caps hidpCaps
	r, _, _ := pHidP_GetCaps.Call(pp, uintptr(unsafe.Pointer(&caps)))
	nmax, _, _ := pHidP_MaxUsageListLen.Call(hidpInput, usagePageButton, pp)
	// joystick (4), gamepad (5), multi-axis controller (8) - wheels and button boxes are these
	isCtl := r == hidpStatusSuccess && caps.UsagePage == usagePageGeneric &&
		(caps.Usage == 4 || caps.Usage == 5 || caps.Usage == 8) && nmax > 0 && caps.InputReportByteLength > 0
	if !isCtl {
		pHidD_FreePreparsed.Call(pp)
		windows.CloseHandle(hd)
		return
	}
	id := hidDevID(path)
	var name [128]uint16
	if r, _, _ := pHidD_GetProductString.Call(uintptr(hd), uintptr(unsafe.Pointer(&name[0])), unsafe.Sizeof(name)); r != 0 {
		if s := strings.TrimSpace(windows.UTF16ToString(name[:])); s != "" {
			hidNames.Store(id, s)
		}
	}
	h.mu.Lock()
	h.open[path] = true
	h.mu.Unlock()
	go h.read(path, id, hd, pp, int(caps.InputReportByteLength), int(nmax))
}

func (h *hidReader) read(path, id string, hd windows.Handle, pp uintptr, repLen, nmax int) {
	defer func() {
		pHidD_FreePreparsed.Call(pp)
		windows.CloseHandle(hd)
		h.mu.Lock()
		delete(h.open, path)
		delete(h.down, id)
		h.mu.Unlock()
	}()
	buf := make([]byte, repLen)
	usages := make([]uint16, nmax)
	for {
		var n uint32
		if err := windows.ReadFile(hd, buf, &n, nil); err != nil || n == 0 {
			return // unplugged
		}
		ln := uint32(nmax)
		r, _, _ := pHidP_GetUsages.Call(hidpInput, usagePageButton, 0,
			uintptr(unsafe.Pointer(&usages[0])), uintptr(unsafe.Pointer(&ln)), pp,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(n))
		if r != hidpStatusSuccess {
			continue // a report without buttons (e.g. only axes, or another report id)
		}
		btns := append([]uint16(nil), usages[:ln]...)
		h.mu.Lock()
		if h.down[id] == nil {
			h.down[id] = map[byte][]uint16{}
		}
		h.down[id][buf[0]] = btns
		h.mu.Unlock()
	}
}

// hidDevID: a stable id from the device path, e.g. "346e_0004" from ...vid_346e&pid_0004...
func hidDevID(path string) string {
	l := strings.ToLower(path)
	vid, pid := "", ""
	if i := strings.Index(l, "vid_"); i >= 0 && i+8 <= len(l) {
		vid = l[i+4 : i+8]
	}
	if i := strings.Index(l, "pid_"); i >= 0 && i+8 <= len(l) {
		pid = l[i+4 : i+8]
	}
	if vid == "" {
		return fmt.Sprintf("%08x", fnv32(l))
	}
	id := vid + "_" + pid
	if i := strings.Index(l, "&col"); i >= 0 && i+6 <= len(l) { // one physical device can expose several
		id += "_" + l[i+4:i+6]
	}
	return id
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * 16777619
	}
	return h
}

func hidName(dev string) string {
	if v, ok := hidNames.Load(dev); ok {
		return v.(string)
	}
	return ""
}
