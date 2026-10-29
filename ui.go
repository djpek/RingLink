package main

import (
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"io"
	"math"
	"strings"
	"time"

	"gioui.org/app"
	"gioui.org/f32"
	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/io/clipboard"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
	"golang.org/x/exp/shiny/materialdesign/icons"
)

// ---------- palette ----------

var (
	colBg      = rgb(0x1c2026)
	colCard    = glassCard // frosted panel (see setPalette)
	colBorder  = color.NRGBA{255, 255, 255, 0x2e}
	colField   = glassField
	colHover   = glassHover
	colSelect  = color.NRGBA{255, 255, 255, 0xe6} // selected segment
	colText    = rgb(0xf4f6f9)
	colMuted   = color.NRGBA{235, 240, 246, 0xb0}
	colFaint   = color.NRGBA{235, 240, 246, 0x78}
	colAccent  = rgb(0x6cc4f5)
	colAccentD = rgb(0x0b1a26) // text on accent / on light surfaces
	colGood    = rgb(0x5ee39a)
	colWarn    = rgb(0xf5c451)
	colBad     = rgb(0xff7b7b)
)

var (
	glassCard  = color.NRGBA{255, 255, 255, 0x17}
	glassField = color.NRGBA{255, 255, 255, 0x14}
	glassHover = color.NRGBA{255, 255, 255, 0x26}
	segTrack   = color.NRGBA{0, 0, 0, 0x26}
)

// setPalette: over the real desktop, panels and buttons are dark tints whose
// opacity the user picks; otherwise they're frosted white over the backdrop.
func setPalette(seeThrough bool, panelPct int) {
	if !seeThrough {
		colCard, colField, colHover, segTrack = glassCard, glassField, glassHover, color.NRGBA{0, 0, 0, 0x26}
		return
	}
	a := func(f float64) uint8 { return uint8(float64(panelPct) * 2.55 * f) }
	colCard = color.NRGBA{22, 26, 33, a(0.7)}
	colField = color.NRGBA{44, 50, 60, a(1)}
	colHover = color.NRGBA{62, 70, 82, a(1)}
	segTrack = color.NRGBA{30, 35, 43, a(1)}
}

func rgb(c uint32) color.NRGBA {
	return color.NRGBA{R: uint8(c >> 16), G: uint8(c >> 8), B: uint8(c), A: 0xff}
}

func withAlpha(c color.NRGBA, a uint8) color.NRGBA { c.A = a; return c }

func mustIcon(b []byte) *widget.Icon {
	ic, err := widget.NewIcon(b)
	if err != nil {
		panic(err)
	}
	return ic
}

var (
	icMic     = mustIcon(icons.AVMic)
	icMicOff  = mustIcon(icons.AVMicOff)
	icHeadset = mustIcon(icons.HardwareHeadset)
	icCopy    = mustIcon(icons.ContentContentCopy)
	icCheck   = mustIcon(icons.ActionDone)
	icExit    = mustIcon(icons.ActionExitToApp)
	icAdd     = mustIcon(icons.ContentAdd)
	icRemove  = mustIcon(icons.ContentRemove)
	icGroup   = mustIcon(icons.SocialGroup)
	icCar     = mustIcon(icons.MapsDirectionsCar)
	icTune    = mustIcon(icons.ImageTune)
	icList    = mustIcon(icons.ActionList)
	icLogin   = mustIcon(icons.ActionInput)
	icVolume  = mustIcon(icons.AVVolumeUp)
	icPower   = mustIcon(icons.ActionPowerSettingsNew)
	icRestore = mustIcon(icons.ActionRestore)
	icExpand  = mustIcon(icons.NavigationExpandMore)
	icLess    = mustIcon(icons.NavigationExpandLess)
)

var gameOptions = []struct {
	label string
	mode  int
}{{"Auto", gameNone}, {"Le Mans Ultimate", gameLMU}, {"BeamNG", gameBeamNG}}

// ---------- ui state ----------

type ui struct {
	w   *app.Window
	th  *material.Theme
	app *App

	page                       layout.List
	nameEd, codeEd             widget.Editor
	createBtn, joinBtn         widget.Clickable
	copyBtn, leaveBtn          widget.Clickable
	voiceBtn, muteBtn, resetFX widget.Clickable
	minusBtn, plusBtn, logBtn  widget.Clickable
	gameBtns                   [3]widget.Clickable
	sens, vol, rng, muff       widget.Float
	formErr                    string
	copiedAt                   time.Time
	showLog                    bool
	backdrop                   paint.ImageOp
	an                         animator
	micSel, spkSel             widget.Clickable
	micOpen, spkOpen           bool
	micList, spkList           []AudioDevice
	micOpts, spkOpts           []widget.Clickable
	view                       app.ViewEvent
	opacityF, panelF           widget.Float
	appliedOpacity             int
	opacityOK                  bool
}

func runUI() {
	u := &ui{w: new(app.Window)}
	u.w.Option(app.Title("RingLink"), app.Size(unit.Dp(560), unit.Dp(900)), app.MinSize(unit.Dp(440), unit.Dp(480)))
	u.th = material.NewTheme()
	u.th.Shaper = text.NewShaper(text.NoSystemFonts(), text.WithCollection(append(uiFonts(), gofont.Collection()...)))
	u.th.Face = "SN Pro"
	u.th.Palette = material.Palette{Fg: colText, Bg: colBg, ContrastBg: colAccent, ContrastFg: colAccentD}
	u.th.TextSize = 14
	u.app = NewApp(u.w.Invalidate)
	u.backdrop = paint.NewImageOp(makeBackdrop(900, 1400))
	u.page.Axis = layout.Vertical
	u.nameEd.SingleLine = true
	u.codeEd.SingleLine = true
	u.nameEd.SetText(u.app.S.DisplayName)
	u.sens.Value = float32(u.app.Voice.Sens.Load()) / 100
	u.micList = append([]AudioDevice{{"", "System default"}}, listMics()...)
	u.spkList = append([]AudioDevice{{"", "System default"}}, listSpeakers()...)
	u.micOpts = make([]widget.Clickable, len(u.micList))
	u.spkOpts = make([]widget.Clickable, len(u.spkList))
	u.opacityF.Value = float32(u.opacity()) / 100
	u.panelF.Value = float32(u.panelOpacity()) / 100
	u.syncFX()
	go func() {
		for range time.Tick(250 * time.Millisecond) {
			u.w.Invalidate()
		}
	}()
	var ops op.Ops
	for {
		switch e := u.w.Event().(type) {
		case app.DestroyEvent:
			u.app.Save()
			u.app.LeaveRoom()
			return
		case app.ViewEvent:
			u.view = e
			u.appliedOpacity = -1
		case app.FrameEvent:
			u.applyOpacity()
			setPalette(u.seeThrough(), u.panelOpacity())
			gtx := app.NewContext(&ops, e)
			u.drawBackdrop(gtx)
			u.update(gtx)
			u.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

func (u *ui) syncFX() {
	v := u.app.Voice
	u.vol.Value = float32(v.VolPct.Load()) / 200
	u.rng.Value = float32(v.RangeM.Load()-20) / 480
	u.muff.Value = float32(v.Muffle.Load()) / 100
}

// ---------- input ----------

func (u *ui) update(gtx layout.Context) {
	a := u.app
	if n := strings.TrimSpace(u.nameEd.Text()); n != "" && n != a.S.DisplayName {
		a.S.DisplayName = n
	}
	for i := range u.gameBtns {
		if u.gameBtns[i].Clicked(gtx) {
			a.SetGame(gameOptions[i].mode)
		}
	}
	if u.minusBtn.Clicked(gtx) && a.S.MaxPeople > 2 {
		a.S.MaxPeople--
		a.Save()
	}
	if u.plusBtn.Clicked(gtx) && a.S.MaxPeople < MaxRoomSize {
		a.S.MaxPeople++
		a.Save()
	}
	if u.createBtn.Clicked(gtx) {
		u.formErr = ""
		a.Save()
		go a.CreateRoom()
	}
	if u.joinBtn.Clicked(gtx) {
		u.formErr = ""
		a.Save()
		code := u.codeEd.Text()
		if _, err := NormalizeCode(code); err != nil {
			u.formErr = err.Error()
		} else {
			go a.JoinRoom(code)
		}
	}
	if u.leaveBtn.Clicked(gtx) {
		go a.LeaveRoom()
	}
	if u.copyBtn.Clicked(gtx) {
		if r := a.Room(); r != nil {
			gtx.Execute(clipboard.WriteCmd{Type: "application/text", Data: io.NopCloser(strings.NewReader(r.Code))})
			u.copiedAt = time.Now()
		}
	}
	if u.voiceBtn.Clicked(gtx) {
		go a.SetVoice(!a.Voice.Running())
	}
	if u.muteBtn.Clicked(gtx) {
		a.Voice.Muted.Store(!a.Voice.Muted.Load())
	}
	if u.opacityF.Update(gtx) {
		a.S.Opacity = int(u.opacityF.Value*100 + 0.5)
		if a.S.Opacity < 1 {
			a.S.Opacity = 1 // 0 means "unset" in the settings file
		}
		if !u.opacityF.Dragging() {
			a.Save()
		}
	}
	if u.panelF.Update(gtx) {
		a.S.PanelAlpha = int(u.panelF.Value*100 + 0.5)
		if a.S.PanelAlpha < 10 {
			a.S.PanelAlpha = 10
		}
		if !u.panelF.Dragging() {
			a.Save()
		}
	}
	if u.micSel.Clicked(gtx) {
		u.micOpen, u.spkOpen = !u.micOpen, false
		if u.micOpen {
			u.an.set("open:Microphone", 0)
			u.micList = append([]AudioDevice{{"", "System default"}}, listMics()...)
			u.micOpts = make([]widget.Clickable, len(u.micList))
		}
	}
	if u.spkSel.Clicked(gtx) {
		u.spkOpen, u.micOpen = !u.spkOpen, false
		if u.spkOpen {
			u.an.set("open:Output", 0)
			u.spkList = append([]AudioDevice{{"", "System default"}}, listSpeakers()...)
			u.spkOpts = make([]widget.Clickable, len(u.spkList))
		}
	}
	for i := range u.micOpts {
		if u.micOpts[i].Clicked(gtx) {
			a.SetDevices(u.micList[i].ID, a.S.SpkDevice)
			u.micOpen = false
		}
	}
	for i := range u.spkOpts {
		if u.spkOpts[i].Clicked(gtx) {
			a.SetDevices(a.S.MicDevice, u.spkList[i].ID)
			u.spkOpen = false
		}
	}
	if u.logBtn.Clicked(gtx) {
		u.showLog = !u.showLog
	}
	if u.sens.Update(gtx) {
		v := int32(u.sens.Value*100 + 0.5)
		if v < 1 {
			v = 1
		}
		a.Voice.Sens.Store(v)
		a.S.VoiceSens = int(v)
		if !u.sens.Dragging() {
			a.Save()
		}
	}
	save := false
	if u.vol.Update(gtx) {
		x := int(u.vol.Value*200 + 0.5)
		a.Voice.VolPct.Store(int32(x))
		a.S.VoiceVol = &x
		save = !u.vol.Dragging()
	}
	if u.rng.Update(gtx) {
		x := int(20+u.rng.Value*480) / 5 * 5
		a.Voice.RangeM.Store(int32(x))
		a.S.VoiceRange = &x
		save = !u.rng.Dragging()
	}
	if u.muff.Update(gtx) {
		x := int(u.muff.Value*100 + 0.5)
		a.Voice.Muffle.Store(int32(x))
		a.S.VoiceMuff = &x
		save = !u.muff.Dragging()
	}
	if u.resetFX.Clicked(gtx) {
		a.S.VoiceVol, a.S.VoiceRange, a.S.VoiceMuff = nil, nil, nil
		a.Voice.VolPct.Store(100)
		a.Voice.RangeM.Store(150)
		a.Voice.Muffle.Store(50)
		u.syncFX()
		save = true
	}
	if save {
		a.Save()
	}
}

// ---------- building blocks ----------

func rrect(gtx layout.Context, sz image.Point, r int, c color.NRGBA) {
	defer clip.UniformRRect(image.Rectangle{Max: sz}, r).Push(gtx.Ops).Pop()
	paint.Fill(gtx.Ops, c)
}

// shadow: soft drop shadow made of a few stacked, growing, faint layers,
// offset downward. elev is in dp, alpha is the strength of the darkest layer.
func shadow(gtx layout.Context, sz image.Point, r, elev int, alpha uint8) {
	e := gtx.Dp(unit.Dp(elev))
	if e <= 0 {
		return
	}
	const layers = 5
	for i := layers; i >= 1; i-- {
		grow := e * i / layers
		a := uint8(int(alpha) / (i + 1))
		rect := image.Rectangle{Min: image.Pt(-grow/2, e/2-grow/3), Max: image.Pt(sz.X+grow/2, sz.Y+e/2+grow/2)}
		st := clip.UniformRRect(rect, r+grow/2).Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{0, 0, 0, a})
		st.Pop()
	}
}

// glass: translucent rounded fill with a hairline edge.
func glass(gtx layout.Context, sz image.Point, r int, fill, edge color.NRGBA) {
	rect := image.Rectangle{Max: sz}
	rrect(gtx, sz, r, fill)
	path := clip.UniformRRect(rect, r).Path(gtx.Ops)
	paint.FillShape(gtx.Ops, edge, clip.Stroke{Path: path, Width: float32(gtx.Dp(1))}.Op())
}

func (u *ui) opacity() int {
	o := u.app.S.Opacity
	if o <= 0 || o > 100 {
		return 100
	}
	if !perPixel() && o < 40 {
		return 40 // fading the whole window further makes it unreadable
	}
	return o
}

func (u *ui) panelOpacity() int {
	p := u.app.S.PanelAlpha
	if p <= 0 || p > 100 {
		return 90
	}
	return p
}

func (u *ui) seeThrough() bool { return perPixel() && u.opacity() < 100 }

var blurHinted bool

// perPixel: only the background is see-through (Linux/X11 with the patched Gio).
func perPixel() bool { return app.TransparentActive }

func (u *ui) applyOpacity() {
	o := u.opacity()
	key := o
	if perPixel() {
		key += 1000
	}
	if u.view == nil || key == u.appliedOpacity {
		return
	}
	u.appliedOpacity = key
	if perPixel() {
		u.opacityOK = true // handled when painting the background
		blurHinted = setBlurBehind(u.view, o < 100)
		return
	}
	u.opacityOK = setWindowOpacity(u.view, float64(o)/100)
}

func (u *ui) drawBackdrop(gtx layout.Context) {
	if o := u.opacity(); o < 100 && u.opacityOK {
		// see-through: a plain dark tint reads best over the desktop
		c := rgb(0x12161c)
		if perPixel() {
			c.A = uint8(o * 255 / 100) // only the background fades
		}
		paint.Fill(gtx.Ops, c)
		return
	}
	sz := u.backdrop.Size()
	W, H := gtx.Constraints.Max.X, gtx.Constraints.Max.Y
	sc := float32(W) / float32(sz.X)
	if s2 := float32(H) / float32(sz.Y); s2 > sc {
		sc = s2
	}
	defer op.Affine(f32Scale(sc)).Push(gtx.Ops).Pop()
	u.backdrop.Add(gtx.Ops)
	paint.PaintOp{}.Add(gtx.Ops)
}

// card draws a bordered panel with an icon + title header.
func (u *ui) card(gtx layout.Context, ic *widget.Icon, title string, trailing layout.Widget, body layout.Widget) layout.Dimensions {
	return layout.Inset{Left: 16, Right: 16, Bottom: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		macro := op.Record(gtx.Ops)
		dims := layout.UniformInset(18).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							gtx.Constraints.Min = image.Pt(gtx.Dp(18), gtx.Dp(18))
							gtx.Constraints.Max = gtx.Constraints.Min
							return ic.Layout(gtx, colAccent)
						}),
						layout.Rigid(layout.Spacer{Width: 10}.Layout),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							l := material.Label(u.th, 16, title)
							l.Font.Weight = font.SemiBold
							return l.Layout(gtx)
						}),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							if trailing == nil {
								return layout.Dimensions{}
							}
							return trailing(gtx)
						}),
					)
				}),
				layout.Rigid(layout.Spacer{Height: 14}.Layout),
				layout.Rigid(body),
			)
		})
		call := macro.Stop()
		shadow(gtx, dims.Size, gtx.Dp(22), 8, 0x22)
		glass(gtx, dims.Size, gtx.Dp(22), colCard, colBorder)
		call.Add(gtx.Ops)
		return dims
	})
}

type btnKind int

const (
	btnPrimary btnKind = iota
	btnSecondary
	btnGhost
	btnDanger
)

// button: rounded, optional leading icon, hover state, pointer cursor.
func (u *ui) button(c *widget.Clickable, ic *widget.Icon, label string, kind btnKind, full bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		bg, fg := colAccent, colAccentD
		switch kind {
		case btnSecondary:
			bg, fg = colField, colText
		case btnGhost:
			bg, fg = color.NRGBA{}, colMuted
		case btnDanger:
			bg, fg = withAlpha(colBad, 0x22), colBad
		}
		hv := map[bool]float32{false: 0, true: 1}[c.Hovered()]
		h := u.an.to(gtx, c, hv, 16)
		switch kind {
		case btnPrimary:
			bg = lerpC(bg, rgb(0x8fd3fa), h)
		case btnSecondary, btnGhost:
			bg = lerpC(bg, colHover, h)
		case btnDanger:
			bg = lerpC(bg, withAlpha(colBad, 0x38), h)
		}
		return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			pointer.CursorPointer.Add(gtx.Ops)
			macro := op.Record(gtx.Ops)
			dims := layout.Inset{Top: 10, Bottom: 10, Left: 18, Right: 18}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				if full {
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
				}
				return layout.Flex{Alignment: layout.Middle, Spacing: layout.SpaceSides}.Layout(gtx,
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if ic == nil {
							return layout.Dimensions{}
						}
						sz := gtx.Dp(17)
						gtx.Constraints.Min, gtx.Constraints.Max = image.Pt(sz, sz), image.Pt(sz, sz)
						return ic.Layout(gtx, fg)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if ic == nil || label == "" {
							return layout.Dimensions{}
						}
						return layout.Spacer{Width: 8}.Layout(gtx)
					}),
					layout.Rigid(func(gtx layout.Context) layout.Dimensions {
						if label == "" {
							return layout.Dimensions{}
						}
						l := material.Label(u.th, 14, label)
						l.Color = fg
						l.Font.Weight = font.Medium
						return l.Layout(gtx)
					}),
				)
			})
			call := macro.Stop()
			if kind != btnGhost {
				lift := 3 + int(2*h+0.5) // hover lifts the button a little
				shadow(gtx, dims.Size, dims.Size.Y/2, lift, 0x30)
			}
			rrect(gtx, dims.Size, dims.Size.Y/2, bg)
			call.Add(gtx.Ops)
			return dims
		})
	}
}

func (u *ui) label(size unit.Sp, s string, c color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Label(u.th, size, s)
		l.Color = c
		return l.Layout(gtx)
	}
}

func (u *ui) caption(s string) layout.Widget { return u.label(12.5, s, colMuted) }

func (u *ui) sectionLabel(s string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Label(u.th, 11.5, strings.ToUpper(s))
		l.Color = colFaint
		l.Font.Weight = font.SemiBold
		return layout.Inset{Bottom: 6}.Layout(gtx, l.Layout)
	}
}

func (u *ui) input(ed *widget.Editor, hint string, mono bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		macro := op.Record(gtx.Ops)
		dims := layout.Inset{Top: 11, Bottom: 11, Left: 16, Right: 16}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			e := material.Editor(u.th, ed, hint)
			e.HintColor = colFaint
			e.TextSize = 15
			if mono { // room code: a little bolder so it's easy to read
				e.Font.Weight = font.Medium
			}
			return e.Layout(gtx)
		})
		call := macro.Stop()
		bc := color.NRGBA{255, 255, 255, 0x1c}
		if gtx.Focused(ed) {
			bc = colAccent
		}
		glass(gtx, dims.Size, dims.Size.Y/2, colField, bc)
		call.Add(gtx.Ops)
		return dims
	}
}

func vgap(h unit.Dp) layout.Widget { return layout.Spacer{Height: h}.Layout }

func col(gtx layout.Context, ws ...layout.Widget) layout.Dimensions {
	ch := make([]layout.FlexChild, 0, len(ws))
	for _, w := range ws {
		if w != nil {
			ch = append(ch, layout.Rigid(w))
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, ch...)
}

func row(gtx layout.Context, gap unit.Dp, ws ...layout.Widget) layout.Dimensions {
	var ch []layout.FlexChild
	for i, w := range ws {
		if i > 0 {
			ch = append(ch, layout.Rigid(layout.Spacer{Width: gap}.Layout))
		}
		ch = append(ch, layout.Rigid(w))
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx, ch...)
}

// pill: small rounded status badge with a dot.
func (u *ui) pill(s string, c color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		macro := op.Record(gtx.Ops)
		dims := layout.Inset{Top: 5, Bottom: 5, Left: 10, Right: 11}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			dc := c
			if c == colWarn { // "connecting" breathes
				dc = withAlpha(c, uint8(110+145*pulse(gtx, 0.9)))
			}
			return row(gtx, 6, dot(dc, 7), u.label(12, s, c))
		})
		call := macro.Stop()
		rrect(gtx, dims.Size, dims.Size.Y/2, withAlpha(c, 0x1f))
		call.Add(gtx.Ops)
		return dims
	}
}

func dot(c color.NRGBA, d unit.Dp) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		sz := gtx.Dp(d)
		defer clip.Ellipse(image.Rectangle{Max: image.Pt(sz, sz)}).Push(gtx.Ops).Pop()
		paint.Fill(gtx.Ops, c)
		return layout.Dimensions{Size: image.Pt(sz, sz)}
	}
}

// segmented control
func (u *ui) segmented(gtx layout.Context, sel int) layout.Dimensions {
	pos := u.an.to(gtx, &u.gameBtns, float32(sel), 14) // the highlight slides
	macro := op.Record(gtx.Ops)
	var segSize image.Point
	dims := layout.UniformInset(3).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		var ch []layout.FlexChild
		for i := range gameOptions {
			i := i
			ch = append(ch, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				c := &u.gameBtns[i]
				return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					pointer.CursorPointer.Add(gtx.Ops)
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					macro := op.Record(gtx.Ops)
					d := layout.Inset{Top: 9, Bottom: 9}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						return layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
							l := material.Label(u.th, 13, gameOptions[i].label)
							l.Font.Weight = font.Medium
							near := 1 - float32(math.Min(1, math.Abs(float64(pos)-float64(i))))
							base := colMuted
							if c.Hovered() && i != sel {
								base = colText
							}
							l.Color = lerpC(base, colAccentD, near)
							if i == sel {
								l.Font.Weight = font.SemiBold
							}
							return l.Layout(gtx)
						})
					})
					call := macro.Stop()
					segSize = d.Size
					if i != sel {
						hv := u.an.to(gtx, c, map[bool]float32{false: 0, true: 1}[c.Hovered()], 16)
						if hv > 0.01 {
							rrect(gtx, d.Size, d.Size.Y/2, withAlpha(colHover, uint8(float32(colHover.A)*hv)))
						}
					}
					call.Add(gtx.Ops)
					return d
				})
			}))
		}
		// labels are recorded first so the sliding highlight can go underneath
		rows := op.Record(gtx.Ops)
		d := layout.Flex{}.Layout(gtx, ch...)
		rowsCall := rows.Stop()
		if segSize.X > 0 {
			off := op.Offset(image.Pt(int(pos*float32(d.Size.X)/float32(len(gameOptions))+0.5), 0)).Push(gtx.Ops)
			sz := image.Pt(d.Size.X/len(gameOptions), segSize.Y)
			shadow(gtx, sz, sz.Y/2, 2, 0x38)
			rrect(gtx, sz, sz.Y/2, colSelect)
			off.Pop()
		}
		rowsCall.Add(gtx.Ops)
		return d
	})
	call := macro.Stop()
	glass(gtx, dims.Size, dims.Size.Y/2, segTrack, color.NRGBA{255, 255, 255, 0x1c})
	call.Add(gtx.Ops)
	return dims
}

// avatar: initials on a colour picked from the name
var avatarCols = []color.NRGBA{rgb(0x6cc4f5), rgb(0xb18cf7), rgb(0xf59e6c), rgb(0x45d483), rgb(0xf06c9b), rgb(0xf0c84a), rgb(0x5ed6c9), rgb(0x8ea2ff)}

func (u *ui) avatar(name string, talking bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		h := fnv.New32a()
		h.Write([]byte(name))
		c := avatarCols[h.Sum32()%uint32(len(avatarCols))]
		sz := gtx.Dp(36)
		full := gtx.Dp(40)
		inset := (full - sz) / 2
		tk := u.an.to(gtx, "talk:"+name, map[bool]float32{false: 0, true: 1}[talking], 12)
		if tk > 0.01 { // pulsing green ring around whoever is talking
			glow := 0.65 + 0.35*pulse(gtx, 1.6)
			path := clip.Ellipse(image.Rectangle{Min: image.Pt(1, 1), Max: image.Pt(full-1, full-1)}).Path(gtx.Ops)
			paint.FillShape(gtx.Ops, withAlpha(colGood, uint8(255*tk*glow)), clip.Stroke{Path: path, Width: float32(gtx.Dp(2))}.Op())
		}
		defer op.Offset(image.Pt(inset, inset)).Push(gtx.Ops).Pop()
		cl := clip.Ellipse(image.Rectangle{Max: image.Pt(sz, sz)}).Push(gtx.Ops)
		paint.Fill(gtx.Ops, color.NRGBA{18, 22, 28, 0xb0})
		paint.Fill(gtx.Ops, withAlpha(c, 0x30))
		cl.Pop()
		in := initials(name)
		gtx.Constraints = layout.Exact(image.Pt(sz, sz))
		layout.Center.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			l := material.Label(u.th, 13, in)
			l.Color = c
			l.Font.Weight = font.Bold
			return l.Layout(gtx)
		})
		return layout.Dimensions{Size: image.Pt(gtx.Dp(40), gtx.Dp(40))}
	}
}

func initials(name string) string {
	f := strings.Fields(name)
	switch {
	case len(f) == 0:
		return "?"
	case len(f) == 1:
		r := []rune(f[0])
		if len(r) > 1 {
			return strings.ToUpper(string(r[:2]))
		}
		return strings.ToUpper(string(r))
	}
	return strings.ToUpper(string([]rune(f[0])[:1]) + string([]rune(f[1])[:1]))
}

func (u *ui) bar(frac float32, c color.NRGBA, w unit.Dp) layout.Widget {
	return u.barK(nil, frac, c, w)
}

// barK: like bar, but glides to new values when key is set.
func (u *ui) barK(key any, frac float32, c color.NRGBA, w unit.Dp) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		if key != nil {
			frac = u.an.to(gtx, key, frac, 14)
		}
		W, H := gtx.Dp(w), gtx.Dp(6)
		if w == 0 {
			W = gtx.Constraints.Max.X
		}
		rrect(gtx, image.Pt(W, H), H/2, colField)
		if frac > 1 {
			frac = 1
		}
		if fw := int(float32(W) * frac); fw > 0 {
			rrect(gtx, image.Pt(fw, H), H/2, c)
		}
		return layout.Dimensions{Size: image.Pt(W, H)}
	}
}

// slider with a label on the left and the value on the right
func (u *ui) sliderRow(name, value string, f *widget.Float) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return col(gtx,
			func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, u.label(13.5, name, colText)),
					layout.Rigid(u.label(13, value, colAccent)),
				)
			},
			func(gtx layout.Context) layout.Dimensions {
				s := material.Slider(u.th, f)
				s.Color = colAccent
				return s.Layout(gtx)
			},
		)
	}
}

func divider(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Top: 14, Bottom: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		sz := image.Pt(gtx.Constraints.Max.X, 1)
		paint.FillShape(gtx.Ops, colBorder, clip.Rect{Max: sz}.Op())
		return layout.Dimensions{Size: sz}
	})
}

// ---------- page ----------

func (u *ui) layout(gtx layout.Context) {
	sections := []layout.Widget{u.header, u.gameCard, u.roomCard, u.voiceCard, u.lookCard, u.logCard, vgap(8)}
	u.page.Layout(gtx, len(sections), func(gtx layout.Context, i int) layout.Dimensions {
		t := u.an.intro(gtx, 0.05+0.07*float64(i), 0.45)
		if t >= 1 {
			return sections[i](gtx)
		}
		// slide up and fade in on startup, one card after another
		defer paint.PushOpacity(gtx.Ops, t).Pop()
		defer op.Offset(image.Pt(0, int((1-t)*float32(gtx.Dp(18))))).Push(gtx.Ops).Pop()
		return sections[i](gtx)
	})
}

func (u *ui) header(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Left: 18, Right: 18, Top: 20, Bottom: 16}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				sz := gtx.Dp(44)
				glass(gtx, image.Pt(sz, sz), sz/2, color.NRGBA{255, 255, 255, 0x22}, color.NRGBA{255, 255, 255, 0x40})
				defer op.Offset(image.Pt(gtx.Dp(11), gtx.Dp(11))).Push(gtx.Ops).Pop()
				is := gtx.Dp(22)
				gtx.Constraints = layout.Exact(image.Pt(is, is))
				icHeadset.Layout(gtx, colAccent)
				return layout.Dimensions{Size: image.Pt(sz, sz)}
			}),
			layout.Rigid(layout.Spacer{Width: 12}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return col(gtx,
					func(gtx layout.Context) layout.Dimensions {
						l := material.Label(u.th, 22, "RingLink")
						l.Font.Weight = font.Bold
						return l.Layout(gtx)
					},
					u.caption("Proximity voice chat for sim racing"),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				r := u.app.Room()
				if r == nil {
					return u.pill("Not in a room", colFaint)(gtx)
				}
				st, max := r.State()
				n := len(r.Members())
				switch {
				case strings.HasPrefix(st, "room is full"):
					return u.pill("Room full", colBad)(gtx)
				case st == "in room":
					return u.pill(fmt.Sprintf("In room · %d/%d", n, max), colGood)(gtx)
				}
				return u.pill("Connecting", colWarn)(gtx)
			}),
		)
	})
}

func (u *ui) gameCard(gtx layout.Context) layout.Dimensions {
	sel := 0
	for i, g := range gameOptions {
		if g.mode == u.app.S.Game {
			sel = i
		}
	}
	vs := u.app.Voice.Status()
	return u.card(gtx, icCar, "Game", nil, func(gtx layout.Context) layout.Dimensions {
		var hint string
		switch u.app.S.Game {
		case gameNone:
			hint = "Uses whichever game you're driving in."
		case gameLMU:
			hint = "Reads your car position from Le Mans Ultimate. No setup needed."
		case gameBeamNG:
			hint = "Reads your car position from BeamNG / BeamMP."
		}
		var bng layout.Widget
		if u.app.S.Game != gameLMU {
			bng = func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 4}.Layout(gtx, u.caption(fmt.Sprintf("BeamNG setup: Options › Other › turn on MotionSim, IP 127.0.0.1, port %d.", BeamNGPort)))
			}
		}
		var live layout.Widget
		if vs.On {
			live = func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Top: 10}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					if vs.Game != "" {
						return u.pill("Driving in "+vs.Game, colGood)(gtx)
					}
					return u.pill("Not driving - everyone at normal volume", colFaint)(gtx)
				})
			}
		}
		return col(gtx,
			func(gtx layout.Context) layout.Dimensions { return u.segmented(gtx, sel) },
			vgap(10),
			u.caption(hint),
			bng,
			live,
		)
	})
}

func (u *ui) roomCard(gtx layout.Context) layout.Dimensions {
	r := u.app.Room()
	if r == nil {
		return u.card(gtx, icGroup, "Room", nil, u.lobby)
	}
	return u.card(gtx, icGroup, "Room", u.button(&u.leaveBtn, icExit, "Leave", btnDanger, false), func(gtx layout.Context) layout.Dimensions {
		return u.inRoom(gtx, r)
	})
}

func (u *ui) lobby(gtx layout.Context) layout.Dimensions {
	a := u.app
	return col(gtx,
		u.sectionLabel("Your name"),
		u.input(&u.nameEd, "Driver", false),
		divider,
		u.sectionLabel("Start a new room"),
		func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, u.label(14, "Max people", colText)),
				layout.Rigid(u.button(&u.minusBtn, icRemove, "", btnSecondary, false)),
				layout.Rigid(func(gtx layout.Context) layout.Dimensions {
					return layout.Inset{Left: 14, Right: 14}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						l := material.Label(u.th, 18, fmt.Sprint(a.S.MaxPeople))
						l.Font.Weight = font.SemiBold
						return l.Layout(gtx)
					})
				}),
				layout.Rigid(u.button(&u.plusBtn, icAdd, "", btnSecondary, false)),
			)
		},
		vgap(12),
		u.button(&u.createBtn, icAdd, "Create room", btnPrimary, true),
		divider,
		u.sectionLabel("Join a friend's room"),
		func(gtx layout.Context) layout.Dimensions {
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, u.input(&u.codeEd, "Room code, e.g. K7QM-29XD", true)),
				layout.Rigid(layout.Spacer{Width: 10}.Layout),
				layout.Rigid(u.button(&u.joinBtn, icLogin, "Join", btnSecondary, false)),
			)
		},
		func(gtx layout.Context) layout.Dimensions {
			if u.formErr == "" {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: 8}.Layout(gtx, u.label(13, u.formErr, colBad))
		},
	)
}

func (u *ui) inRoom(gtx layout.Context, r *Room) layout.Dimensions {
	state, max := r.State()
	members := r.Members()
	vs := u.app.Voice.Status()
	copied := time.Since(u.copiedAt) < 2*time.Second
	copyIc, copyLbl := icCopy, "Copy"
	if copied {
		copyIc, copyLbl = icCheck, "Copied"
	}
	ws := []layout.Widget{
		// big code block
		func(gtx layout.Context) layout.Dimensions {
			macro := op.Record(gtx.Ops)
			dims := layout.Inset{Top: 14, Bottom: 14, Left: 16, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				gtx.Constraints.Min.X = gtx.Constraints.Max.X
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return col(gtx,
							u.sectionLabel("Room code"),
							func(gtx layout.Context) layout.Dimensions {
								l := material.Label(u.th, 30, r.Code)
								l.Font.Weight = font.Bold
								l.Color = colAccent
								return l.Layout(gtx)
							},
						)
					}),
					layout.Rigid(u.button(&u.copyBtn, copyIc, copyLbl, btnPrimary, false)),
				)
			})
			call := macro.Stop()
			glass(gtx, dims.Size, gtx.Dp(16), color.NRGBA{0, 0, 0, 0x22}, color.NRGBA{255, 255, 255, 0x1c})
			call.Add(gtx.Ops)
			return dims
		},
		vgap(6),
		u.caption("Everyone pastes this same code into RingLink to join."),
		vgap(16),
		func(gtx layout.Context) layout.Dimensions {
			sc := colWarn
			txt := state
			switch {
			case strings.HasPrefix(state, "room is full"):
				sc = colBad
			case state == "in room":
				sc, txt = colGood, "Connected"
			}
			return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
				layout.Flexed(1, u.sectionLabel(fmt.Sprintf("People · %d of %d", len(members), max))),
				layout.Rigid(u.pill(capitalize(txt), sc)),
			)
		},
		vgap(4),
	}
	for _, m := range members {
		m := m
		ws = append(ws, func(gtx layout.Context) layout.Dimensions {
			return u.memberRow(gtx, m, vs)
		})
	}
	return col(gtx, ws...)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (u *ui) memberRow(gtx layout.Context, m MemberInfo, vs VoiceStatus) layout.Dimensions {
	pv, hasPV := vs.Peers[m.ID]
	talking := (m.Self && vs.Talking) || (hasPV && pv.Talking)
	name := m.Name
	if m.Self {
		name += " (you)"
	}
	var sub string
	var subC = colMuted
	switch {
	case m.Self && u.app.Voice.Muted.Load():
		sub, subC = "Mic muted", colWarn
	case m.Self:
		sub = "This PC"
	case !m.Connected:
		sub, subC = "Connecting...", colWarn
	default:
		sub = fmt.Sprintf("%d ms", m.RTT.Milliseconds())
		if hasPV {
			if pv.Game != "" && pv.Game != vs.Game {
				sub += " · in " + pv.Game
			}
			if pv.Dist >= 0 {
				sub += fmt.Sprintf(" · %.0f m away", pv.Dist)
				if pv.Muffled > 0 {
					sub += fmt.Sprintf(" · %d%% muffled", pv.Muffled)
				}
			}
		}
	}
	return layout.Inset{Top: 6, Bottom: 6}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
			layout.Rigid(u.avatar(m.Name, talking)),
			layout.Rigid(layout.Spacer{Width: 12}.Layout),
			layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
				return col(gtx,
					func(gtx layout.Context) layout.Dimensions {
						l := material.Label(u.th, 14.5, name)
						l.Font.Weight = font.Medium
						return l.Layout(gtx)
					},
					vgap(2),
					u.label(12.5, sub, subC),
				)
			}),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				if m.Self || !m.Connected || !hasPV {
					return layout.Dimensions{}
				}
				return row(gtx, 8,
					func(gtx layout.Context) layout.Dimensions {
						sz := gtx.Dp(16)
						gtx.Constraints = layout.Exact(image.Pt(sz, sz))
						return icVolume.Layout(gtx, colFaint)
					},
					u.barK(m.ID, float32(pv.Volume)/100, colAccent, 60),
					u.label(12, fmt.Sprintf("%d%%", pv.Volume), colMuted),
				)
			}),
		)
	})
}

func (u *ui) voiceCard(gtx layout.Context) layout.Dimensions {
	v := u.app.Voice
	vs := v.Status()
	inRoom := u.app.Room() != nil
	muted := v.Muted.Load()
	var trailing layout.Widget
	switch {
	case !inRoom:
		trailing = u.pill("Starts in a room", colFaint)
	case vs.On:
		trailing = u.pill("On", colGood)
	case vs.Err != "":
		trailing = u.pill("Problem", colBad)
	default:
		trailing = u.pill("Off", colFaint)
	}
	return u.card(gtx, icMic, "Voice", trailing, func(gtx layout.Context) layout.Dimensions {
		var ws []layout.Widget
		if vs.Err != "" && !vs.On && inRoom {
			ws = append(ws, u.label(13, vs.Err, colBad), vgap(10))
		}
		ws = append(ws,
			u.sectionLabel("Devices"),
			u.devicePicker(&u.micSel, icMic, "Microphone", u.app.S.MicDevice, u.micOpen, u.micList, u.micOpts),
			vgap(8),
			u.devicePicker(&u.spkSel, icHeadset, "Output", u.app.S.SpkDevice, u.spkOpen, u.spkList, u.spkOpts),
			divider,
		)
		if inRoom {
			muteIc, muteLbl, muteKind := icMic, "Mute mic", btnSecondary
			if muted {
				muteIc, muteLbl, muteKind = icMicOff, "Unmute mic", btnDanger
			}
			voiceLbl, voiceKind := "Turn voice off", btnGhost
			if !vs.On {
				voiceLbl, voiceKind = "Turn voice on", btnPrimary
			}
			ws = append(ws,
				func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Flexed(1, u.button(&u.muteBtn, muteIc, muteLbl, muteKind, true)),
						layout.Rigid(layout.Spacer{Width: 10}.Layout),
						layout.Rigid(u.button(&u.voiceBtn, icPower, voiceLbl, voiceKind, false)),
					)
				},
				vgap(14),
			)
		}
		if vs.On {
			lvlC := colFaint
			status := "Quiet"
			if muted {
				lvlC, status = colWarn, "Muted"
			} else if vs.Talking {
				lvlC, status = colGood, "Sending"
			}
			ws = append(ws,
				func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(u.label(13.5, "Mic level", colText)),
						layout.Rigid(layout.Spacer{Width: 12}.Layout),
						layout.Flexed(1, u.barK(&u.sens, float32(vs.Level)/100, lvlC, 0)),
						layout.Rigid(layout.Spacer{Width: 12}.Layout),
						layout.Rigid(u.label(12.5, status, lvlC)),
					)
				},
				vgap(10),
				u.sliderRow("Mic sensitivity", fmt.Sprintf("%d", v.Sens.Load()), &u.sens),
				u.caption("Raise it if your voice cuts out, lower it if background noise gets through."),
				divider,
			)
		}
		ws = append(ws,
			func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
					layout.Flexed(1, u.sectionLabel("Other people's voices")),
					layout.Rigid(u.button(&u.resetFX, icRestore, "Reset", btnGhost, false)),
				)
			},
			u.sliderRow("Volume", fmt.Sprintf("%d%%", v.VolPct.Load()), &u.vol),
			vgap(6),
			u.sliderRow("How far voices carry", fmt.Sprintf("%d m", v.RangeM.Load()), &u.rng),
			u.caption(fmt.Sprintf("Full volume within %d m, silent at %d m.", v.RangeM.Load()/10, v.RangeM.Load())),
			vgap(10),
			u.sliderRow("Muffle with distance", fmt.Sprintf("%d%%", v.Muffle.Load()), &u.muff),
			u.caption(capitalize(muffleDesc(v.Muffle.Load()))+"."),
			vgap(12),
			u.caption("Uses your default mic and speakers. Use headphones so nobody hears themselves echo."),
		)
		return col(gtx, ws...)
	})
}

func muffleDesc(m int32) string {
	switch {
	case m >= 75:
		return "like hearing them through a wall when far"
	case m >= 40:
		return "muffled when far"
	case m > 0:
		return "a little muffled when far"
	}
	return "always clear"
}

func (u *ui) logCard(gtx layout.Context) layout.Dimensions {
	lbl := "Show"
	if u.showLog {
		lbl = "Hide"
	}
	return u.card(gtx, icList, "Activity", u.button(&u.logBtn, nil, lbl, btnGhost, false), func(gtx layout.Context) layout.Dimensions {
		logs := u.app.Logs()
		n := 3
		if u.showLog {
			n = 20
		}
		if len(logs) > n {
			logs = logs[len(logs)-n:]
		}
		if len(logs) == 0 {
			return u.caption("Nothing yet.")(gtx)
		}
		var ws []layout.Widget
		for _, l := range logs {
			l := l
			ws = append(ws, func(gtx layout.Context) layout.Dimensions {
				ts, msg := "", l
				if len(l) > 9 && l[8] == ' ' {
					ts, msg = l[:8], l[9:]
				}
				return layout.Inset{Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					return layout.Flex{}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							lb := material.Label(u.th, 12, ts)
							lb.Color = colFaint
							return layout.Inset{Right: 10}.Layout(gtx, lb.Layout)
						}),
						layout.Flexed(1, u.label(12.5, msg, colMuted)),
					)
				})
			})
		}
		return col(gtx, ws...)
	})
}

func f32Scale(s float32) f32.Affine2D { return f32.Affine2D{}.Scale(f32.Point{}, f32.Pt(s, s)) }

func (u *ui) lookCard(gtx layout.Context) layout.Dimensions {
	return u.card(gtx, icTune, "Appearance", nil, func(gtx layout.Context) layout.Dimensions {
		if perPixel() {
			note := "Lower the background to see your desktop behind RingLink. Needs a compositing desktop (Linux Mint has one). Your desktop can't blur what's behind, so it shows through sharp."
			if blurHinted {
				note = "Lower the background to see your desktop behind RingLink. Blur works on desktops that support it (like KDE Plasma); Linux Mint's Cinnamon shows it sharp."
			}
			return col(gtx,
				u.sliderRow("Background", fmt.Sprintf("%d%%", u.opacity()), &u.opacityF),
				vgap(4),
				u.sliderRow("Panels & buttons", fmt.Sprintf("%d%%", u.panelOpacity()), &u.panelF),
				u.caption(note),
			)
		}
		note := "Lower it to see your desktop through the whole window."
		if u.view != nil && !u.opacityOK {
			note = "Your desktop doesn't support see-through windows (e.g. Wayland), so this has no effect."
		}
		return col(gtx,
			u.sliderRow("Window opacity", fmt.Sprintf("%d%%", u.opacity()), &u.opacityF),
			u.caption(note),
		)
	})
}

// devicePicker: a field showing the chosen device; click to open the list.
func (u *ui) devicePicker(sel *widget.Clickable, ic *widget.Icon, title, current string, open bool, list []AudioDevice, opts []widget.Clickable) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		label := "System default"
		if current != "" {
			label = current
			for _, d := range list {
				if d.ID == current {
					label = d.Name
				}
			}
			if label == current {
				// saved device isn't plugged in right now
				label = shortDevice(current) + " (not found)"
			}
		}
		field := func(gtx layout.Context) layout.Dimensions {
			return sel.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				pointer.CursorPointer.Add(gtx.Ops)
				macro := op.Record(gtx.Ops)
				d := layout.Inset{Top: 10, Bottom: 10, Left: 14, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							sz := gtx.Dp(17)
							gtx.Constraints = layout.Exact(image.Pt(sz, sz))
							return ic.Layout(gtx, colAccent)
						}),
						layout.Rigid(layout.Spacer{Width: 10}.Layout),
						layout.Rigid(u.label(13, title, colMuted)),
						layout.Rigid(layout.Spacer{Width: 10}.Layout),
						layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
							l := material.Label(u.th, 14, label)
							l.MaxLines = 1
							l.Truncator = "…"
							l.Alignment = text.End
							return l.Layout(gtx)
						}),
						layout.Rigid(layout.Spacer{Width: 6}.Layout),
						layout.Rigid(func(gtx layout.Context) layout.Dimensions {
							sz := gtx.Dp(20)
							gtx.Constraints = layout.Exact(image.Pt(sz, sz))
							if open {
								return icLess.Layout(gtx, colMuted)
							}
							return icExpand.Layout(gtx, colMuted)
						}),
					)
				})
				call := macro.Stop()
				bg := colField
				if sel.Hovered() {
					bg = colHover
				}
				glass(gtx, d.Size, gtx.Dp(12), bg, color.NRGBA{255, 255, 255, 0x1c})
				call.Add(gtx.Ops)
				return d
			})
		}
		if !open {
			return field(gtx)
		}
		ws := []layout.Widget{field, vgap(4)}
		fade := u.an.to(gtx, "open:"+title, 1, 14)
		if len(list) <= 1 {
			ws = append(ws, func(gtx layout.Context) layout.Dimensions {
				return layout.Inset{Left: 14, Top: 4}.Layout(gtx, u.caption("No other devices found."))
			})
		}
		for i := range list {
			i := i
			ws = append(ws, func(gtx layout.Context) layout.Dimensions {
				c := &opts[i]
				defer paint.PushOpacity(gtx.Ops, fade).Pop()
				return c.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					pointer.CursorPointer.Add(gtx.Ops)
					macro := op.Record(gtx.Ops)
					d := layout.Inset{Top: 8, Bottom: 8, Left: 14, Right: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
						gtx.Constraints.Min.X = gtx.Constraints.Max.X
						return layout.Flex{Alignment: layout.Middle}.Layout(gtx,
							layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
								l := material.Label(u.th, 13.5, list[i].Name)
								l.MaxLines = 1
								l.Truncator = "…"
								return l.Layout(gtx)
							}),
							layout.Rigid(func(gtx layout.Context) layout.Dimensions {
								if list[i].ID != current {
									return layout.Dimensions{}
								}
								sz := gtx.Dp(17)
								gtx.Constraints = layout.Exact(image.Pt(sz, sz))
								return icCheck.Layout(gtx, colAccent)
							}),
						)
					})
					call := macro.Stop()
					if c.Hovered() {
						rrect(gtx, d.Size, gtx.Dp(10), colHover)
					}
					call.Add(gtx.Ops)
					return d
				})
			})
		}
		return col(gtx, ws...)
	}
}

// shortDevice makes a raw sound-server name a bit friendlier.
func shortDevice(id string) string {
	id = strings.TrimPrefix(strings.TrimPrefix(id, "alsa_input."), "alsa_output.")
	if len(id) > 40 {
		id = id[:40] + "…"
	}
	return id
}
