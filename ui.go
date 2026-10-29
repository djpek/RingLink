package main

import (
	"fmt"
	"image"
	"image/color"
	"io"
	"strings"
	"time"

	"gioui.org/app"
	"gioui.org/font/gofont"
	"gioui.org/io/clipboard"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/text"
	"gioui.org/unit"
	"gioui.org/widget"
	"gioui.org/widget/material"
)

var (
	colBg     = color.NRGBA{0x14, 0x16, 0x1b, 0xff}
	colCard   = color.NRGBA{0x1e, 0x22, 0x2a, 0xff}
	colField  = color.NRGBA{0x2a, 0x2f, 0x39, 0xff}
	colText   = color.NRGBA{0xe8, 0xea, 0xee, 0xff}
	colMuted  = color.NRGBA{0x9a, 0xa1, 0xad, 0xff}
	colAccent = color.NRGBA{0x6c, 0xc4, 0xf5, 0xff}
	colGood   = color.NRGBA{0x3c, 0xc4, 0x6a, 0xff}
	colWarn   = color.NRGBA{0xf0, 0xb4, 0x29, 0xff}
	colBad    = color.NRGBA{0xff, 0x5c, 0x5c, 0xff}
	colBtn2   = color.NRGBA{0x37, 0x3d, 0x49, 0xff}
)

type ui struct {
	w   *app.Window
	th  *material.Theme
	app *App

	page                        layout.List
	nameEd, codeEd              widget.Editor
	createBtn, joinBtn, copyBtn widget.Clickable
	leaveBtn, voiceBtn, muteBtn widget.Clickable
	fxResetBtn                  widget.Clickable
	maxF, sens, vol, rng, muff  widget.Float
	formErr                     string
	copiedAt                    time.Time
}

func runUI() {
	u := &ui{w: new(app.Window)}
	u.w.Option(app.Title("RingLink - LMU proximity chat"), app.Size(unit.Dp(620), unit.Dp(860)), app.MinSize(unit.Dp(480), unit.Dp(500)))
	u.th = material.NewTheme()
	u.th.Shaper = text.NewShaper(text.WithCollection(gofont.Collection()))
	u.th.Palette = material.Palette{Fg: colText, Bg: colBg, ContrastBg: colAccent, ContrastFg: color.NRGBA{0x0b, 0x1a, 0x26, 0xff}}
	u.th.TextSize = 15
	u.app = NewApp(u.w.Invalidate)
	u.page.Axis = layout.Vertical
	u.nameEd.SingleLine = true
	u.codeEd.SingleLine = true
	u.nameEd.SetText(u.app.S.DisplayName)
	u.maxF.Value = float32(u.app.S.MaxPeople-2) / float32(MaxRoomSize-2)
	u.sens.Value = float32(u.app.Voice.Sens.Load()) / 100
	u.syncFX()
	go func() {
		for range time.Tick(300 * time.Millisecond) {
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
		case app.FrameEvent:
			gtx := app.NewContext(&ops, e)
			paint.Fill(gtx.Ops, colBg)
			u.update(gtx)
			u.layout(gtx)
			e.Frame(gtx.Ops)
		}
	}
}

// ---------- input handling ----------

func (u *ui) update(gtx layout.Context) {
	a := u.app
	if n := strings.TrimSpace(u.nameEd.Text()); n != "" && n != a.S.DisplayName {
		a.S.DisplayName = n
	}
	if u.maxF.Update(gtx) {
		a.S.MaxPeople = 2 + int(u.maxF.Value*float32(MaxRoomSize-2)+0.5)
		if !u.maxF.Dragging() {
			a.Save()
		}
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
	saveFX := false
	if u.vol.Update(gtx) {
		x := int(u.vol.Value*200 + 0.5)
		a.Voice.VolPct.Store(int32(x))
		a.S.VoiceVol = &x
		saveFX = !u.vol.Dragging()
	}
	if u.rng.Update(gtx) {
		x := int(20+u.rng.Value*480) / 5 * 5
		a.Voice.RangeM.Store(int32(x))
		a.S.VoiceRange = &x
		saveFX = !u.rng.Dragging()
	}
	if u.muff.Update(gtx) {
		x := int(u.muff.Value*100 + 0.5)
		a.Voice.Muffle.Store(int32(x))
		a.S.VoiceMuff = &x
		saveFX = !u.muff.Dragging()
	}
	if u.fxResetBtn.Clicked(gtx) {
		a.S.VoiceVol, a.S.VoiceRange, a.S.VoiceMuff = nil, nil, nil
		a.Voice.VolPct.Store(100)
		a.Voice.RangeM.Store(150)
		a.Voice.Muffle.Store(50)
		u.syncFX()
		saveFX = true
	}
	if saveFX {
		a.Save()
	}
}

// ---------- layout helpers ----------

func (u *ui) card(gtx layout.Context, w layout.Widget) layout.Dimensions {
	return layout.Inset{Left: 14, Right: 14, Bottom: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		macro := op.Record(gtx.Ops)
		dims := layout.UniformInset(14).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Min.X = gtx.Constraints.Max.X
			return w(gtx)
		})
		call := macro.Stop()
		rr := clip.UniformRRect(image.Rectangle{Max: dims.Size}, gtx.Dp(10)).Push(gtx.Ops)
		paint.Fill(gtx.Ops, colCard)
		rr.Pop()
		call.Add(gtx.Ops)
		return dims
	})
}

func (u *ui) title(t string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Subtitle1(u.th, t)
		l.Font.Weight = 700
		return layout.Inset{Bottom: 8}.Layout(gtx, l.Layout)
	}
}

func (u *ui) text(t string, c color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Body2(u.th, t)
		l.Color = c
		return l.Layout(gtx)
	}
}

func (u *ui) small(t string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		l := material.Caption(u.th, t)
		l.Color = colMuted
		return l.Layout(gtx)
	}
}

func (u *ui) field(label string, ed *widget.Editor, hint string) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return layout.Flex{Axis: layout.Vertical}.Layout(gtx,
			layout.Rigid(u.small(label)),
			layout.Rigid(layout.Spacer{Height: 3}.Layout),
			layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				macro := op.Record(gtx.Ops)
				dims := layout.UniformInset(8).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
					gtx.Constraints.Min.X = gtx.Constraints.Max.X
					e := material.Editor(u.th, ed, hint)
					e.HintColor = colMuted
					e.TextSize = 14
					return e.Layout(gtx)
				})
				call := macro.Stop()
				rr := clip.UniformRRect(image.Rectangle{Max: dims.Size}, gtx.Dp(6)).Push(gtx.Ops)
				paint.Fill(gtx.Ops, colField)
				rr.Pop()
				call.Add(gtx.Ops)
				return dims
			}),
			layout.Rigid(layout.Spacer{Height: 8}.Layout),
		)
	}
}

func (u *ui) btn(c *widget.Clickable, t string, primary bool) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		b := material.Button(u.th, c, t)
		b.TextSize = 14
		b.CornerRadius = 6
		if !primary {
			b.Background = colBtn2
			b.Color = colText
		}
		return b.Layout(gtx)
	}
}

func row(gtx layout.Context, ws ...layout.Widget) layout.Dimensions {
	var ch []layout.FlexChild
	for i, w := range ws {
		if i > 0 {
			ch = append(ch, layout.Rigid(layout.Spacer{Width: 8}.Layout))
		}
		ch = append(ch, layout.Rigid(w))
	}
	return layout.Flex{Alignment: layout.Middle}.Layout(gtx, ch...)
}

func col(gtx layout.Context, ws ...layout.Widget) layout.Dimensions {
	var ch []layout.FlexChild
	for _, w := range ws {
		if w != nil {
			ch = append(ch, layout.Rigid(w))
		}
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, ch...)
}

func gap(h unit.Dp) layout.Widget { return layout.Spacer{Height: h}.Layout }

func (u *ui) slider(label string, f *widget.Float) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		return col(gtx, u.small(label), func(gtx layout.Context) layout.Dimensions {
			gtx.Constraints.Max.X = gtx.Dp(320)
			return material.Slider(u.th, f).Layout(gtx)
		})
	}
}

func dot(c color.NRGBA) layout.Widget {
	return func(gtx layout.Context) layout.Dimensions {
		sz := gtx.Dp(10)
		defer clip.Ellipse(image.Rectangle{Max: image.Pt(sz, sz)}).Push(gtx.Ops).Pop()
		paint.Fill(gtx.Ops, c)
		return layout.Dimensions{Size: image.Pt(sz, sz)}
	}
}

func (u *ui) syncFX() {
	v := u.app.Voice
	u.vol.Value = float32(v.VolPct.Load()) / 200
	u.rng.Value = float32(v.RangeM.Load()-20) / 480
	u.muff.Value = float32(v.Muffle.Load()) / 100
}

// ---------- sections ----------

func (u *ui) layout(gtx layout.Context) {
	sections := []layout.Widget{u.header, u.roomCard, u.voiceCard, u.logCard}
	u.page.Layout(gtx, len(sections), func(gtx layout.Context, i int) layout.Dimensions {
		return sections[i](gtx)
	})
}

func (u *ui) header(gtx layout.Context) layout.Dimensions {
	return layout.Inset{Left: 18, Right: 18, Top: 16, Bottom: 12}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
		return col(gtx,
			func(gtx layout.Context) layout.Dimensions {
				l := material.H5(u.th, "RingLink")
				l.Font.Weight = 800
				return l.Layout(gtx)
			},
			u.small("Proximity voice chat for Le Mans Ultimate and BeamNG - the closer someone's car is, the louder they are."),
		)
	})
}

func (u *ui) roomCard(gtx layout.Context) layout.Dimensions {
	r := u.app.Room()
	return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
		if r == nil {
			return u.lobby(gtx)
		}
		return u.inRoom(gtx, r)
	})
}

func (u *ui) lobby(gtx layout.Context) layout.Dimensions {
	return col(gtx,
		u.title("1. Get in a room"),
		u.field("Your name (what others see)", &u.nameEd, "Driver"),
		gap(4),
		u.text("Make a new room", colText),
		u.slider(fmt.Sprintf("Max people: %d", u.app.S.MaxPeople), &u.maxF),
		u.btn(&u.createBtn, "Create room", true),
		gap(16),
		u.text("...or join a friend's room", colText),
		gap(4),
		u.field("Room code", &u.codeEd, "e.g. K7QM-29XD"),
		u.btn(&u.joinBtn, "Join room", false),
		func(gtx layout.Context) layout.Dimensions {
			if u.formErr == "" {
				return layout.Dimensions{}
			}
			return layout.Inset{Top: 6}.Layout(gtx, u.text(u.formErr, colBad))
		},
	)
}

func (u *ui) inRoom(gtx layout.Context, r *Room) layout.Dimensions {
	state, max := r.State()
	members := r.Members()
	vs := u.app.Voice.Status()
	copied := "Copy code"
	if time.Since(u.copiedAt) < 2*time.Second {
		copied = "Copied!"
	}
	ws := []layout.Widget{
		u.title("1. Room"),
		func(gtx layout.Context) layout.Dimensions {
			return row(gtx, func(gtx layout.Context) layout.Dimensions {
				l := material.H4(u.th, r.Code)
				l.Font.Weight = 700
				l.Font.Typeface = "Go Mono"
				return l.Layout(gtx)
			}, u.btn(&u.copyBtn, copied, true))
		},
		u.small("Everyone pastes this same code into RingLink to join."),
		gap(10),
	}
	stc := colMuted
	switch {
	case strings.HasPrefix(state, "room is full"):
		stc = colBad
	case state == "in room":
		stc = colGood
	}
	ws = append(ws, u.text(fmt.Sprintf("%s  -  %d of %d people", state, len(members), max), stc), gap(8))
	for _, m := range members {
		m := m
		ws = append(ws, func(gtx layout.Context) layout.Dimensions {
			c := colWarn
			detail := "connecting..."
			if m.Self {
				c = colGood
				detail = "you"
				if vs.Talking {
					detail = "you - talking"
				}
			} else if m.Connected {
				c = colGood
				detail = fmt.Sprintf("ping %d ms", m.RTT.Milliseconds())
				if pv, ok := vs.Peers[m.ID]; ok {
					if pv.Game != "" && pv.Game != vs.Game {
						detail += "  -  in " + pv.Game
					}
					if pv.Dist >= 0 {
						detail += fmt.Sprintf("  -  %.0f m away, %d%% volume", pv.Dist, pv.Volume)
						if pv.Muffled > 0 {
							detail += fmt.Sprintf(", %d%% muffled", pv.Muffled)
						}
					}
					if pv.Talking {
						detail += "  -  talking"
					}
				}
			}
			return layout.Inset{Bottom: 4}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return row(gtx, dot(c), u.text(m.Name, colText), u.small(detail))
			})
		})
	}
	ws = append(ws, gap(8), u.btn(&u.leaveBtn, "Leave room", false))
	return col(gtx, ws...)
}

func (u *ui) voiceCard(gtx layout.Context) layout.Dimensions {
	v := u.app.Voice
	vs := v.Status()
	inRoom := u.app.Room() != nil
	return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
		state, c := "Off", colMuted
		if vs.On {
			state, c = "On", colGood
		}
		if vs.Err != "" && !vs.On {
			state, c = "Problem: "+vs.Err, colBad
		}
		if !inRoom {
			state, c = "Starts when you're in a room", colMuted
		}
		prox := "Volume follows car distance when you're on the same LMU server or BeamMP map. Otherwise everyone is at normal volume."
		if vs.On {
			if vs.Game != "" {
				prox = "Your car position: " + vs.Game + ". " + prox
			} else {
				prox = "Your car position: not driving. " + prox
			}
		}
		voiceLabel, muteLabel := "Turn voice on", "Mute mic"
		if vs.On {
			voiceLabel = "Turn voice off"
		}
		if v.Muted.Load() {
			muteLabel = "Unmute mic"
		}
		talk, tc := "silent", colMuted
		if v.Muted.Load() {
			talk, tc = "muted", colWarn
		} else if vs.Talking {
			talk, tc = "sending", colGood
		}
		return col(gtx,
			u.title("2. Voice"),
			u.text(state, c),
			gap(4),
			u.small(prox),
			gap(8),
			func(gtx layout.Context) layout.Dimensions {
				if !vs.On {
					return layout.Dimensions{}
				}
				return col(gtx,
					func(gtx layout.Context) layout.Dimensions {
						return row(gtx, u.small("Your mic"), func(gtx layout.Context) layout.Dimensions {
							w, h := gtx.Dp(160), gtx.Dp(8)
							rr := clip.UniformRRect(image.Rectangle{Max: image.Pt(w, h)}, h/2).Push(gtx.Ops)
							paint.Fill(gtx.Ops, colField)
							rr.Pop()
							r2 := clip.UniformRRect(image.Rectangle{Max: image.Pt(w*vs.Level/100, h)}, h/2).Push(gtx.Ops)
							paint.Fill(gtx.Ops, tc)
							r2.Pop()
							return layout.Dimensions{Size: image.Pt(w, h)}
						}, u.text(talk, tc))
					},
					gap(6),
					u.slider("Mic sensitivity (raise it if your voice cuts out, lower it if background noise gets through)", &u.sens),
					gap(6),
				)
			},
			u.text("Everyone else's voices", colText),
			gap(4),
			u.slider(fmt.Sprintf("Volume: %d%%", v.VolPct.Load()), &u.vol),
			u.slider(fmt.Sprintf("How far voices carry: %d m (full volume within %d m)", v.RangeM.Load(), v.RangeM.Load()/10), &u.rng),
			u.slider(fmt.Sprintf("Muffle with distance: %d%% (%s)", v.Muffle.Load(), muffleDesc(v.Muffle.Load())), &u.muff),
			gap(4),
			func(gtx layout.Context) layout.Dimensions {
				if !inRoom {
					return u.btn(&u.fxResetBtn, "Reset voice settings", false)(gtx)
				}
				return row(gtx, u.btn(&u.voiceBtn, voiceLabel, !vs.On), u.btn(&u.muteBtn, muteLabel, false), u.btn(&u.fxResetBtn, "Reset voice settings", false))
			},
			gap(4),
			u.small("Uses your default mic and speakers/headset. Use headphones so nobody hears themselves echo."),
			gap(4),
			u.small(fmt.Sprintf("BeamNG: in Options > Other, turn on MotionSim with IP 127.0.0.1 and port %d.", BeamNGPort)),
		)
	})
}

func muffleDesc(m int32) string {
	switch {
	case m >= 75:
		return "like through a wall when far"
	case m >= 40:
		return "muffled when far"
	case m > 0:
		return "a little muffled when far"
	}
	return "clear"
}

func (u *ui) logCard(gtx layout.Context) layout.Dimensions {
	return u.card(gtx, func(gtx layout.Context) layout.Dimensions {
		logs := u.app.Logs()
		if len(logs) > 12 {
			logs = logs[len(logs)-12:]
		}
		ws := []layout.Widget{u.title("Log")}
		for _, l := range logs {
			l := l
			ws = append(ws, func(gtx layout.Context) layout.Dimensions {
				lb := material.Caption(u.th, l)
				lb.Color = colMuted
				return lb.Layout(gtx)
			})
		}
		return col(gtx, ws...)
	})
}
