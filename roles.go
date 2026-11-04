package main

// Host and mods.
//
// Whoever made the room is the host. The host can make people mods; the host and mods
// can mute someone for everyone. There's no server, and everyone in the room has the
// room key, so these changes are signed: each member has its own Ed25519 key (sent in
// its announce), the host's key is the first one announced as host, and every client
// checks the signature before following a change. Changes are passed around by
// everyone every few seconds, so people who join later get them too.
//
// Muting is enforced twice: the muted person's RingLink stops sending, and everyone
// else's drops their voice anyway.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const pkCtrl = 9 // [ed25519 signature 64][JSON roleDoc]

type roleDoc struct {
	Kind  string   `json:"k"` // "roles" (who's a mod; host only), "mute" (who's muted) or "room" (room rules); the last two host or mods
	V     int64    `json:"v"` // version: newer replaces older
	By    string   `json:"by"`
	Mods  []string `json:"mods,omitempty"`
	Muted []string `json:"muted,omitempty"`
	// room rules
	PitRadio *bool    `json:"pitRadio,omitempty"` // in the garage everyone hears each other at full volume (default on)
	Isolate  string   `json:"isolate,omitempty"`  // everyone hears only this person (others faded)
	Subs     []string `json:"subs,omitempty"`     // sub-rooms people can pick for the pits
}

// RoomRules: set by the host or mods, the same for everyone in the room.
type RoomRules struct {
	PitRadio bool
	Isolate  uint64 // 0 = nobody
	Subs     []string
}

type roleState struct {
	priv              ed25519.PrivateKey
	hostID            uint64 // 0 = unknown yet
	hostKey           ed25519.PublicKey
	mods              map[uint64]bool
	muted             map[uint64]bool
	rolesV            int64
	muteV             int64
	rolesRaw, muteRaw []byte // last signed docs, passed on as they are
	rules             RoomRules
	rulesV            int64
	rulesRaw          []byte
	lastShare         time.Time
}

func (r *Room) initRoles() {
	_, priv, _ := ed25519.GenerateKey(nil)
	r.roles = roleState{priv: priv, mods: map[uint64]bool{}, muted: map[uint64]bool{}, rules: RoomRules{PitRadio: true}}
	if r.Creator {
		r.roles.hostID, r.roles.hostKey = r.ID, priv.Public().(ed25519.PublicKey)
	}
}

func (r *Room) pubKeyB64() string {
	return base64.RawStdEncoding.EncodeToString(r.roles.priv.Public().(ed25519.PublicKey))
}

// noteAnnouncedKey records a member's signing key; the first host announced is pinned.
func (r *Room) noteAnnouncedKeyLocked(p *peer, a announce) {
	if k, err := base64.RawStdEncoding.DecodeString(a.Key); err == nil && len(k) == ed25519.PublicKeySize && p.key == nil {
		p.key = ed25519.PublicKey(k)
	}
	if a.Host && p.key != nil && r.roles.hostID == 0 {
		r.roles.hostID, r.roles.hostKey = p.id, p.key
	}
}

func (r *Room) keyOfLocked(id uint64) ed25519.PublicKey {
	if id == r.ID {
		return r.roles.priv.Public().(ed25519.PublicKey)
	}
	if p := r.peers[id]; p != nil {
		return p.key
	}
	return nil
}

// ---------- reading roles ----------

func (r *Room) HostID() uint64 { r.mu.Lock(); defer r.mu.Unlock(); return r.roles.hostID }
func (r *Room) AmHost() bool   { return r.HostID() == r.ID }
func (r *Room) IsMod(id uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.roles.mods[id]
}

// CanModerate: you're the host or a mod.
func (r *Room) CanModerate() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.roles.hostID == r.ID || r.roles.mods[r.ID]
}

// MutedByHost: id was muted for everyone by the host or a mod.
func (r *Room) MutedByHost(id uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.roles.muted[id]
}

// Rules: the room's current pit radio / isolate / sub-room settings.
func (r *Room) Rules() RoomRules {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.roles.rules
	c.Subs = append([]string(nil), c.Subs...)
	return c
}

// SetRules changes the room rules (host and mods).
func (r *Room) SetRules(rr RoomRules) error {
	r.mu.Lock()
	if r.roles.hostID != r.ID && !r.roles.mods[r.ID] {
		r.mu.Unlock()
		return errors.New("only the host and mods can change this")
	}
	pr := rr.PitRadio
	d := roleDoc{Kind: "room", V: max(time.Now().UnixMilli(), r.roles.rulesV+1), By: idHex(r.ID), PitRadio: &pr, Subs: cleanSubs(rr.Subs)}
	if rr.Isolate != 0 {
		d.Isolate = idHex(rr.Isolate)
	}
	r.mu.Unlock()
	return r.publishDoc(d)
}

func cleanSubs(l []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range l {
		s = strings.TrimSpace(s)
		if len(s) > 24 {
			s = s[:24]
		}
		if s != "" && !seen[strings.ToLower(s)] && len(out) < 12 {
			seen[strings.ToLower(s)] = true
			out = append(out, s)
		}
	}
	return out
}

// ---------- changing roles ----------

func (r *Room) SetMod(id uint64, on bool) error {
	r.mu.Lock()
	if r.roles.hostID != r.ID {
		r.mu.Unlock()
		return errors.New("only the host can make mods")
	}
	mods := map[uint64]bool{}
	for k := range r.roles.mods {
		mods[k] = true
	}
	if on {
		mods[id] = true
	} else {
		delete(mods, id)
	}
	d := roleDoc{Kind: "roles", V: max(time.Now().UnixMilli(), r.roles.rolesV+1), By: idHex(r.ID), Mods: hexList(mods)}
	r.mu.Unlock()
	return r.publishDoc(d)
}

func (r *Room) SetMuted(id uint64, on bool) error {
	r.mu.Lock()
	if r.roles.hostID != r.ID && !r.roles.mods[r.ID] {
		r.mu.Unlock()
		return errors.New("only the host and mods can mute people")
	}
	if id == r.roles.hostID {
		r.mu.Unlock()
		return errors.New("the host can't be muted")
	}
	muted := map[uint64]bool{}
	for k := range r.roles.muted {
		muted[k] = true
	}
	if on {
		muted[id] = true
	} else {
		delete(muted, id)
	}
	d := roleDoc{Kind: "mute", V: max(time.Now().UnixMilli(), r.roles.muteV+1), By: idHex(r.ID), Muted: hexList(muted)}
	r.mu.Unlock()
	return r.publishDoc(d)
}

func (r *Room) publishDoc(d roleDoc) error {
	js, _ := json.Marshal(d)
	raw := append(ed25519.Sign(r.roles.priv, js), js...)
	if !r.applyDoc(raw) {
		return errors.New("change not accepted")
	}
	r.broadcast(pkCtrl, raw)
	r.changed()
	return nil
}

// applyDoc checks a signed change and takes it if it's valid and newer.
func (r *Room) applyDoc(raw []byte) bool {
	if len(raw) <= ed25519.SignatureSize {
		return false
	}
	sig, js := raw[:ed25519.SignatureSize], raw[ed25519.SignatureSize:]
	var d roleDoc
	if json.Unmarshal(js, &d) != nil {
		return false
	}
	var by uint64
	if _, err := fmt.Sscanf(d.By, "%x", &by); err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := r.keyOfLocked(by)
	if key == nil || !ed25519.Verify(key, js, sig) {
		return false
	}
	isHost := r.roles.hostID != 0 && by == r.roles.hostID && slices.Equal(key, r.roles.hostKey)
	switch d.Kind {
	case "roles":
		if !isHost || d.V <= r.roles.rolesV {
			return false
		}
		r.roles.mods, r.roles.rolesV, r.roles.rolesRaw = parseIDs(d.Mods), d.V, raw
	case "mute":
		if !isHost && !r.roles.mods[by] || d.V <= r.roles.muteV {
			return false
		}
		m := parseIDs(d.Muted)
		delete(m, r.roles.hostID) // nobody mutes the host
		r.roles.muted, r.roles.muteV, r.roles.muteRaw = m, d.V, raw
	case "room":
		if !isHost && !r.roles.mods[by] || d.V <= r.roles.rulesV {
			return false
		}
		rr := RoomRules{PitRadio: d.PitRadio == nil || *d.PitRadio, Subs: cleanSubs(d.Subs)}
		if d.Isolate != "" {
			fmt.Sscanf(d.Isolate, "%x", &rr.Isolate)
		}
		r.roles.rules, r.roles.rulesV, r.roles.rulesRaw = rr, d.V, raw
	default:
		return false
	}
	return true
}

// shareRolesLocked: the signed docs to pass on (every few seconds, for newcomers).
func (r *Room) shareRolesLocked() [][]byte {
	if time.Since(r.roles.lastShare) < 4*time.Second {
		return nil
	}
	r.roles.lastShare = time.Now()
	var out [][]byte
	for _, d := range [][]byte{r.roles.rolesRaw, r.roles.muteRaw, r.roles.rulesRaw} {
		if d != nil {
			out = append(out, d)
		}
	}
	return out
}

func hexList(m map[uint64]bool) []string {
	var out []string
	for id := range m {
		out = append(out, idHex(id))
	}
	slices.Sort(out)
	return out
}

func parseIDs(l []string) map[uint64]bool {
	m := map[uint64]bool{}
	for _, s := range l {
		var id uint64
		if _, err := fmt.Sscanf(s, "%x", &id); err == nil {
			m[id] = true
		}
	}
	return m
}
