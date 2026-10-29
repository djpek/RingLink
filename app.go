package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const appVersion = "3.0"

type Settings struct {
	DisplayName string `json:"displayName"`
	MaxPeople   int    `json:"maxPeople"`
	Voice       *bool  `json:"voice,omitempty"`
	VoiceSens   int    `json:"voiceSens,omitempty"`
	VoiceVol    *int   `json:"voiceVol,omitempty"`
	VoiceRange  *int   `json:"voiceRange,omitempty"`
	VoiceMuff   *int   `json:"voiceMuffle,omitempty"`
}

type App struct {
	mu     sync.Mutex
	S      Settings
	room   *Room
	logs   []string
	notify func()
	Voice  *Voice
	sig    func(topic string) Signaler // tests swap the relay
}

func NewApp(notify func()) *App {
	a := &App{notify: notify}
	a.load()
	a.Voice = NewVoice(func(f string, v ...any) { a.Log(f, v...) }, func() { a.poke() })
	if a.S.VoiceSens > 0 {
		a.Voice.Sens.Store(int32(a.S.VoiceSens))
	}
	if a.S.VoiceVol != nil {
		a.Voice.VolPct.Store(int32(*a.S.VoiceVol))
	}
	if a.S.VoiceRange != nil {
		a.Voice.RangeM.Store(int32(*a.S.VoiceRange))
	}
	if a.S.VoiceMuff != nil {
		a.Voice.Muffle.Store(int32(*a.S.VoiceMuff))
	}
	if a.S.MaxPeople < 2 || a.S.MaxPeople > MaxRoomSize {
		a.S.MaxPeople = 4
	}
	if strings.TrimSpace(a.S.DisplayName) == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "Driver"
		}
		a.S.DisplayName = h
	}
	return a
}

func settingsPath() string {
	d, _ := os.UserConfigDir()
	return filepath.Join(d, "RingLink", "settings.json")
}

func (a *App) load() {
	if b, err := os.ReadFile(settingsPath()); err == nil {
		json.Unmarshal(b, &a.S)
	}
}

func (a *App) Save() {
	a.mu.Lock()
	b, _ := json.MarshalIndent(a.S, "", "  ")
	a.mu.Unlock()
	os.MkdirAll(filepath.Dir(settingsPath()), 0o755)
	os.WriteFile(settingsPath(), b, 0o644)
}

func (a *App) Log(f string, v ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(f, v...)
	a.mu.Lock()
	a.logs = append(a.logs, line)
	if len(a.logs) > 300 {
		a.logs = a.logs[len(a.logs)-300:]
	}
	a.mu.Unlock()
	a.poke()
}

func (a *App) poke() {
	if a.notify != nil {
		a.notify()
	}
}

func (a *App) Logs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.logs...)
}

func (a *App) Room() *Room {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.room
}

// VoiceWanted: voice chat is on unless the user switched it off.
func (a *App) VoiceWanted() bool { return a.S.Voice == nil || *a.S.Voice }

func (a *App) SetVoice(on bool) {
	a.S.Voice = &on
	a.Save()
	if on {
		a.Voice.Start()
	} else {
		a.Voice.Stop()
	}
}

// CreateRoom makes a new room code and opens it.
func (a *App) CreateRoom() error {
	return a.open(NewRoomCode(), true)
}

// JoinRoom joins the room with this code.
func (a *App) JoinRoom(code string) error {
	return a.open(code, false)
}

func (a *App) open(code string, creator bool) error {
	if _, err := NormalizeCode(code); err != nil {
		return err
	}
	a.LeaveRoom()
	name := strings.TrimSpace(a.S.DisplayName)
	var sig Signaler
	if a.sig != nil {
		c, _ := NormalizeCode(code)
		_, topic := roomSecrets(c)
		sig = a.sig(topic)
	}
	r, err := OpenRoom(code, name, a.S.MaxPeople, creator, sig, func(f string, v ...any) { a.Log(f, v...) })
	if err != nil {
		return err
	}
	r.OnVoice = a.Voice.OnPacket
	r.OnChange = func() {
		// forget voice state for people who left
		live := map[uint64]bool{}
		for _, m := range r.Members() {
			live[m.ID] = true
		}
		for _, id := range a.Voice.PeerIDs() {
			if !live[id] {
				a.Voice.RemovePeer(id)
			}
		}
		a.poke()
	}
	a.mu.Lock()
	a.room = r
	a.mu.Unlock()
	a.Voice.SetSender(r.SendVoice)
	if creator {
		a.Log("Room %s created (up to %d people). Send the code to your friends.", r.Code, a.S.MaxPeople)
	} else {
		a.Log("Joining room %s...", r.Code)
	}
	if a.VoiceWanted() {
		go a.Voice.Start()
	}
	a.poke()
	return nil
}

func (a *App) LeaveRoom() {
	a.mu.Lock()
	r := a.room
	a.room = nil
	a.mu.Unlock()
	if r == nil {
		return
	}
	a.Voice.SetSender(nil)
	a.Voice.Stop()
	r.Leave()
	a.Log("Left room %s.", r.Code)
	a.poke()
}
