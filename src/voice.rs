//! Proximity voice chat for any number of people.
//!
//! Audio: 48 kHz mono Opus in 20 ms frames (codec.rs), sent to everyone in the room as
//! unreliable UDP packets: a lost frame is rebuilt from the next packet or smoothed
//! over, because late audio is worse than slightly imperfect audio.
//!
//! Proximity: everyone reads their own car position (games.rs) and sends it to the room
//! ten times a second. Each person's voice is scaled, muffled, panned and pitch-shifted
//! by where their car is relative to yours. When either position is unknown (menus,
//! garage, different servers, game closed) they play at normal volume so you can
//! always talk.

use crate::audio::{self, MicSource, SpeakerSink, FRAME, RATE};
use crate::codec::{VoiceDecoder, VoiceEncoder, FULL_BITRATE, LOW_BITRATE, LOW_FRAMES};
use crate::games::{self, BeamNgListener};
use crate::motion::{game_name, horiz, Motion, V3, GAME_BEAMNG, GAME_LMU, GAME_NONE};
use crate::replay::{ReplayVoice, REC_FULL, REC_LOW};
use crate::signal::LogFn;
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, VecDeque};
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::time::{Duration, Instant};

/// [seq u16][opus, 20 ms]
pub const VK_AUDIO: u8 = 1;
/// [game u8][x y z f64 BE], or nothing = no position
pub const VK_POS: u8 = 2;
/// your "other people's voices" settings, so others can copy them (JSON)
pub const VK_SETTINGS: u8 = 4;
/// [seq u16][opus, 60 ms]: low quality, for people too far away to hear you
pub const VK_LOW: u8 = 5;
/// [flags: 1 in garage, 2 broadcasting, 4 keep me out of replays][sub-room name length][name]
pub const VK_PRESENCE: u8 = 6;

/// frames buffered before a talker starts playing (60 ms)
const JITTER_TARGET: usize = 3;
/// drop old frames beyond this (240 ms)
const JITTER_MAX: usize = 12;
const SOUND_SPEED: f64 = 343.0;
/// In big rooms your full-quality voice isn't sent to cars further than this (the
/// longest "how far voices carry" setting is 500 m, so nobody could hear it anyway).
pub const CULL_DIST: f64 = 600.0;
/// rooms up to this size always get everything
pub const CULL_ROOM_ABOVE: usize = 8;
/// everyone else's volume while someone is isolated
const ISOLATE_FADE: f64 = 0.15;
const POS_FRESH: Duration = Duration::from_secs(3);

fn since(t: Option<Instant>) -> Duration {
    t.map(|t| t.elapsed()).unwrap_or(Duration::MAX)
}

/// The "other people's voices" settings, shared so others can copy them.
#[derive(Serialize, Deserialize, Clone, Copy, Debug, PartialEq)]
pub struct VoiceSettings {
    pub vol: i32,
    pub range: i32,
    pub muffle: i32,
    #[serde(rename = "baseMuffle")]
    pub base_muffle: i32,
    pub directional: bool,
    pub doppler: i32,
}

impl Default for VoiceSettings {
    fn default() -> Self {
        Self { vol: 100, range: 150, muffle: 50, base_muffle: 0, directional: true, doppler: 50 }
    }
}

/// one remote talker
struct VPeer {
    queue: VecDeque<Vec<i16>>,
    priming: bool,
    pos: Option<V3>,
    game: u8,
    pos_seen: Option<Instant>,
    /// their car's movement, for guessing where it is now
    mt: Motion,
    /// your volume for this person (1 = 100 %)
    user_vol: Option<f64>,
    full_at: Option<Instant>,
    /// low-quality packets arrive 60 ms at a time: buffer a bit more
    low_at: Option<Instant>,
    in_garage: bool,
    bcasting: bool,
    /// they opted out of being in other people's replays
    no_record: bool,
    sub: String,
    pres_at: Option<Instant>,
    shared: Option<VoiceSettings>,
    talk_at: Option<Instant>,
    dec_full: Option<VoiceDecoder>,
    dec_low: Option<VoiceDecoder>,
    seq_full: Option<u16>,
    seq_low: Option<u16>,
    // playback thread only
    gain: f64,
    lp1: f64,
    lp2: f64,
    lpa: f64,
    pan: f64,
    // Doppler: resampler state and closing speed
    rs: Vec<f64>,
    ph: f64,
    rate: f64,
    last_d: f64,
    last_d_at: Option<Instant>,
    /// m/s, positive when the cars are getting closer
    closing: f64,
}

impl VPeer {
    fn new() -> Self {
        Self {
            queue: VecDeque::new(),
            priming: true,
            pos: None,
            game: GAME_NONE,
            pos_seen: None,
            mt: Motion::default(),
            user_vol: None,
            full_at: None,
            low_at: None,
            in_garage: false,
            bcasting: false,
            no_record: false,
            sub: String::new(),
            pres_at: None,
            shared: None,
            talk_at: None,
            dec_full: None,
            dec_low: None,
            seq_full: None,
            seq_low: None,
            gain: 1.0,
            lp1: 0.0,
            lp2: 0.0,
            lpa: 0.0,
            pan: 0.0,
            rs: Vec::new(),
            ph: 0.0,
            rate: 1.0,
            last_d: 0.0,
            last_d_at: None,
            closing: 0.0,
        }
    }

    fn present(&self) -> bool {
        since(self.pres_at) < POS_FRESH
    }

    fn pos_fresh(&self) -> bool {
        self.pos.is_some() && since(self.pos_seen) < POS_FRESH
    }

    fn here(&self, now: Instant) -> Option<V3> {
        self.mt.now(now).or(self.pos)
    }

    /// The next 20 ms of this peer's voice, played at rate `r` (r > 1 = higher pitch,
    /// consumes audio a little faster), or None when they're silent.
    fn next_frame(&mut self, r: f64) -> Option<Vec<i16>> {
        // at fast replay speeds each output frame eats several input frames, so buffer more
        let extra = if since(self.low_at) < Duration::from_secs(1) { LOW_FRAMES - 1 } else { 0 };
        if self.priming && self.queue.len() >= JITTER_TARGET + (r as usize).max(1) - 1 + extra {
            self.priming = false;
        }
        if self.priming {
            return None;
        }
        if r == 1.0 && self.ph == 0.0 && self.rs.is_empty() {
            // no Doppler: frames straight through
            let fr = self.queue.pop_front();
            if fr.is_none() {
                self.priming = true; // ran dry: rebuffer before their next sentence
            }
            return fr;
        }
        let need = (self.ph + r * (FRAME - 1) as f64) as usize + 2;
        while self.rs.len() < need {
            let Some(q) = self.queue.pop_front() else { break };
            self.rs.extend(q.iter().map(|x| *x as f64));
        }
        if self.rs.len() < need {
            self.priming = true; // keep what's buffered: it continues once more arrives
            return None;
        }
        let mut out = vec![0i16; FRAME];
        let mut ph = self.ph;
        for o in out.iter_mut() {
            let k = ph as usize;
            let t = ph - k as f64;
            *o = (self.rs[k] + (self.rs[k + 1] - self.rs[k]) * t) as i16;
            ph += r;
        }
        let drop = (ph as usize).min(self.rs.len());
        self.rs.drain(..drop);
        self.ph = ph - drop as f64;
        if (r - 1.0).abs() < 1e-9 && self.ph < 1e-6 && self.rs.is_empty() {
            self.ph = 0.0;
        }
        Some(out)
    }

    /// Tracks how fast this peer's car closes on yours and turns it into a playback
    /// rate. amount: 0 = off, 1 = physically realistic.
    fn doppler_rate(&mut self, d: f64, now: Instant, amount: f64) -> f64 {
        if d < 0.0 {
            self.closing *= 0.9;
            self.last_d_at = None;
        } else if d != self.last_d {
            if let Some(at) = self.last_d_at {
                let dt = now.saturating_duration_since(at).as_secs_f64();
                if dt > 0.02 && dt < 1.5 {
                    let v = ((self.last_d - d) / dt).clamp(-120.0, 120.0);
                    self.closing = 0.6 * self.closing + 0.4 * v;
                }
            }
            self.last_d = d;
            self.last_d_at = Some(now);
        } else if since(self.last_d_at) > Duration::from_millis(1500) {
            self.closing *= 0.9; // positions stopped updating
        }
        let target = if amount > 0.0 { (1.0 + amount * self.closing / SOUND_SPEED).clamp(0.75, 1.33) } else { 1.0 };
        self.rate += (target - self.rate) * 0.15; // glide so pitch never jumps
        if (self.rate - 1.0).abs() < 0.002 && (amount == 0.0 || self.closing.abs() < 0.3) {
            self.rate = 1.0;
        }
        self.rate
    }
}

#[derive(Clone, Debug, Default)]
pub struct PeerVoice {
    pub talking: bool,
    /// metres, -1 when unknown
    pub dist: f64,
    /// percent you hear them at
    pub volume: i32,
    /// 0..100
    pub muffled: i32,
    pub on_track: bool,
    pub game: &'static str,
    pub broadcasting: bool,
    /// where they are relative to your car: metres to your right and metres ahead
    pub rel: Option<[f32; 2]>,
}

#[derive(Clone, Debug, Default)]
pub struct VoiceStatus {
    pub on: bool,
    pub talking: bool,
    /// 0..100 for the meter
    pub level: i32,
    pub backend: String,
    pub err: String,
    pub have_pos: bool,
    /// game your position comes from
    pub game: &'static str,
    pub in_garage: bool,
    pub peers: HashMap<u64, PeerVoice>,
}

type Sender = Arc<dyn Fn(&[u8]) + Send + Sync>;
/// test / demo hook: returns your position instead of asking the games
pub type PosSource = Arc<dyn Fn() -> Option<(V3, u8)> + Send + Sync>;

struct State {
    running: bool,
    run_stop: Option<Arc<AtomicBool>>,
    send: Option<Sender>,
    send_low: Option<Sender>,
    peers: HashMap<u64, VPeer>,
    /// filter/pan state for replayed voices
    rp_peers: HashMap<u64, VPeer>,
    /// in the garage / watching a replay: you're heard at full volume, centred
    broadcast: bool,
    my_pos: Option<V3>,
    my_game: u8,
    me: Motion,
    my_head: [f64; 2],
    have_head: bool,
    mic_dev: String,
    spk_dev: String,
    backend: String,
    err_text: String,
    // room rules (from the host / mods) and where you are
    pit_radio: bool,
    isolate: u64,
    in_garage: bool,
    my_sub: String,
    /// short sounds (join / leave / radio click) waiting to be played
    sfx: VecDeque<i16>,
    /// your own voice, when testing the mic
    loopback: VecDeque<Vec<i16>>,
    pos_source: Option<PosSource>,
}

pub struct Voice {
    log: LogFn,
    notify: Arc<dyn Fn() + Send + Sync>,
    st: Mutex<State>,
    pub replay: Arc<ReplayVoice>,

    /// GAME_NONE = auto, GAME_LMU, GAME_BEAMNG
    pub game_mode: AtomicI32,
    pub muted: AtomicBool,
    /// stop hearing everyone (they can still hear you unless you mute too)
    pub deafened: AtomicBool,
    /// 0..100, higher = more sensitive
    pub sens: AtomicI32,
    /// everyone's volume, 0..200 %
    pub vol_pct: AtomicI32,
    /// distance (m) where voices fade to silence
    pub range_m: AtomicI32,
    /// 0..100: how muffled voices get with distance
    pub muffle: AtomicI32,
    /// 0..100: muffle applied to everyone all the time (helmet / radio feel)
    pub base_muff: AtomicI32,
    /// 0..100: pitch shift from closing speed (100 = realistic)
    pub doppler: AtomicI32,
    /// pan voices left/right by where their car is relative to yours
    pub direct: AtomicBool,
    /// "Broadcast" button switched on in the app
    pub bcast_btn: AtomicBool,
    /// push-to-broadcast key / wheel button held
    pub bcast_key: AtomicBool,
    /// push to talk instead of voice activated
    pub ptt_mode: AtomicBool,
    pub ptt_key: AtomicBool,
    pub ptm_key: AtomicBool,
    /// the host or a mod muted you for everyone
    pub host_mute: AtomicBool,
    /// you opted out of your voice being saved in replays (yours and others')
    pub no_record: AtomicBool,
    /// remove background noise (wheel motor, fans, keyboard) from your mic
    pub denoise: AtomicBool,
    /// join / leave / radio sounds
    pub sounds: AtomicBool,
    /// hear your own mic (to check how you sound)
    pub mic_test: AtomicBool,
    mic_level: AtomicI32,
    talking: AtomicBool,
    kick: (Mutex<bool>, Condvar),
}

impl Voice {
    pub fn new(log: LogFn, notify: Arc<dyn Fn() + Send + Sync>, replay: Arc<ReplayVoice>) -> Arc<Voice> {
        let d = VoiceSettings::default();
        Arc::new(Voice {
            log,
            notify,
            replay,
            st: Mutex::new(State {
                running: false,
                run_stop: None,
                send: None,
                send_low: None,
                peers: HashMap::new(),
                rp_peers: HashMap::new(),
                broadcast: false,
                my_pos: None,
                my_game: GAME_NONE,
                me: Motion::default(),
                my_head: [0.0, 0.0],
                have_head: false,
                mic_dev: String::new(),
                spk_dev: String::new(),
                backend: String::new(),
                err_text: String::new(),
                pit_radio: true,
                isolate: 0,
                in_garage: false,
                my_sub: String::new(),
                sfx: VecDeque::new(),
                loopback: VecDeque::new(),
                pos_source: None,
            }),
            game_mode: AtomicI32::new(0),
            muted: AtomicBool::new(false),
            deafened: AtomicBool::new(false),
            sens: AtomicI32::new(60),
            vol_pct: AtomicI32::new(d.vol),
            range_m: AtomicI32::new(d.range),
            muffle: AtomicI32::new(d.muffle),
            base_muff: AtomicI32::new(d.base_muffle),
            doppler: AtomicI32::new(d.doppler),
            direct: AtomicBool::new(d.directional),
            bcast_btn: AtomicBool::new(false),
            bcast_key: AtomicBool::new(false),
            ptt_mode: AtomicBool::new(false),
            ptt_key: AtomicBool::new(false),
            ptm_key: AtomicBool::new(false),
            host_mute: AtomicBool::new(false),
            no_record: AtomicBool::new(false),
            denoise: AtomicBool::new(true),
            sounds: AtomicBool::new(true),
            mic_test: AtomicBool::new(false),
            mic_level: AtomicI32::new(0),
            talking: AtomicBool::new(false),
            kick: (Mutex::new(false), Condvar::new()),
        })
    }

    fn logf(&self, s: String) {
        (self.log)(s)
    }

    /// Broadcasting: everyone hears you at full volume, centred, wherever their car is.
    pub fn broadcasting(&self) -> bool {
        self.bcast_btn.load(Ordering::Relaxed) || self.bcast_key.load(Ordering::Relaxed)
    }

    /// Sends your position (or "no position" while broadcasting) right away instead of
    /// waiting for the next tick, so the first words already count.
    pub fn kick_pos(&self) {
        *self.kick.0.lock().unwrap() = true;
        self.kick.1.notify_all();
    }

    pub fn set_sender(&self, f: Option<Sender>) {
        let mut s = self.st.lock().unwrap();
        if f.is_none() {
            s.peers.clear();
        }
        s.send = f;
    }

    /// Where low-quality packets go (the app sends them only to far-away people in big
    /// rooms; None = nowhere).
    pub fn set_low_sender(&self, f: Option<Sender>) {
        self.st.lock().unwrap().send_low = f;
    }

    pub fn set_pos_source(&self, f: Option<PosSource>) {
        self.st.lock().unwrap().pos_source = f;
    }

    fn sender(&self) -> Option<Sender> {
        self.st.lock().unwrap().send.clone()
    }

    fn low_sender(&self) -> Option<Sender> {
        self.st.lock().unwrap().send_low.clone()
    }

    /// Whether member `id` is too far from you to hear you right now. Unknown positions
    /// (menus, garage, replays, other game) are never too far, and neither is anyone
    /// while you're broadcasting.
    pub fn too_far(&self, id: u64) -> bool {
        if self.broadcasting() {
            return false;
        }
        let s = self.st.lock().unwrap();
        if s.broadcast || s.my_pos.is_none() {
            return false;
        }
        match s.peers.get(&id) {
            Some(p) => {
                let d = self.dist(&s, p);
                d > CULL_DIST
            }
            None => false,
        }
    }

    /// Applies the room's rules from the host / mods.
    pub fn set_rules(&self, pit_radio: bool, isolate: u64) {
        let changed = {
            let mut s = self.st.lock().unwrap();
            let c = s.pit_radio != pit_radio;
            s.pit_radio = pit_radio;
            s.isolate = isolate;
            c
        };
        if changed {
            self.kick_pos();
        }
    }

    /// Picks your sub-room for the pits ("" = none).
    pub fn set_sub(&self, name: &str) {
        self.st.lock().unwrap().my_sub = name.chars().take(24).collect();
        self.kick_pos();
    }

    pub fn sub(&self) -> String {
        self.st.lock().unwrap().my_sub.clone()
    }

    /// Sub-rooms people in the room are in right now (anyone can make one by joining
    /// it), so they show up for everyone to pick.
    pub fn people_subs(&self) -> Vec<String> {
        let s = self.st.lock().unwrap();
        let mut out = Vec::new();
        if !s.my_sub.is_empty() {
            out.push(s.my_sub.clone());
        }
        for p in s.peers.values() {
            if !p.sub.is_empty() && p.present() {
                out.push(p.sub.clone());
            }
        }
        out
    }

    /// Whether someone is in the garage and which sub-room they picked.
    pub fn peer_presence(&self, id: u64) -> Option<(bool, String)> {
        let s = self.st.lock().unwrap();
        s.peers.get(&id).filter(|p| p.present()).map(|p| (p.in_garage, p.sub.clone()))
    }

    pub fn in_garage(&self) -> bool {
        self.st.lock().unwrap().in_garage
    }

    /// Picks the mic and output ("" = system default). If voice is on, it restarts on
    /// the new devices.
    pub fn set_devices(self: &Arc<Self>, mic: &str, spk: &str) {
        let (changed, running) = {
            let mut s = self.st.lock().unwrap();
            let c = s.mic_dev != mic || s.spk_dev != spk;
            s.mic_dev = mic.to_string();
            s.spk_dev = spk.to_string();
            (c, s.running)
        };
        if changed && running {
            self.stop();
            std::thread::sleep(Duration::from_millis(150));
            self.start();
        }
    }

    pub fn running(&self) -> bool {
        self.st.lock().unwrap().running
    }

    pub fn remove_peer(&self, id: u64) {
        self.st.lock().unwrap().peers.remove(&id);
    }

    pub fn peer_ids(&self) -> Vec<u64> {
        let mut v: Vec<u64> = self.st.lock().unwrap().peers.keys().copied().collect();
        v.sort();
        v
    }

    pub fn my_settings(&self) -> VoiceSettings {
        VoiceSettings {
            vol: self.vol_pct.load(Ordering::Relaxed),
            range: self.range_m.load(Ordering::Relaxed),
            muffle: self.muffle.load(Ordering::Relaxed),
            base_muffle: self.base_muff.load(Ordering::Relaxed),
            directional: self.direct.load(Ordering::Relaxed),
            doppler: self.doppler.load(Ordering::Relaxed),
        }
    }

    pub fn apply_settings(&self, v: &VoiceSettings) {
        self.vol_pct.store(v.vol.clamp(0, 200), Ordering::Relaxed);
        self.range_m.store(v.range.clamp(20, 500), Ordering::Relaxed);
        self.muffle.store(v.muffle.clamp(0, 100), Ordering::Relaxed);
        self.base_muff.store(v.base_muffle.clamp(0, 100), Ordering::Relaxed);
        self.direct.store(v.directional, Ordering::Relaxed);
        self.doppler.store(v.doppler.clamp(0, 100), Ordering::Relaxed);
    }

    /// Your own volume for one person, in percent (100 = normal).
    pub fn set_peer_volume(&self, id: u64, pct: i32) {
        self.st.lock().unwrap().peers.entry(id).or_insert_with(VPeer::new).user_vol = Some(pct.clamp(0, 300) as f64 / 100.0);
    }

    /// The voice settings someone shared, if any.
    pub fn peer_settings(&self, id: u64) -> Option<VoiceSettings> {
        self.st.lock().unwrap().peers.get(&id).and_then(|p| p.shared)
    }

    /// Queues a short sound (see `sfx`), if sounds are switched on.
    pub fn play_sfx(&self, samples: Vec<i16>) {
        if self.sounds.load(Ordering::Relaxed) {
            let mut s = self.st.lock().unwrap();
            if s.running && s.sfx.len() < RATE * 2 {
                s.sfx.extend(samples);
            }
        }
    }

    // ---------- where people are ----------

    fn me_pos(&self, s: &State, now: Instant) -> Option<V3> {
        s.me.now(now).or(s.my_pos)
    }

    /// Metres between your car and theirs, -1 when unknown.
    fn dist(&self, s: &State, p: &VPeer) -> f64 {
        if s.my_pos.is_none() || !p.pos_fresh() || p.game != s.my_game {
            return -1.0;
        }
        let now = Instant::now();
        match (self.me_pos(s, now), p.here(now)) {
            (Some(a), Some(b)) => a.dist(&b),
            _ => -1.0,
        }
    }

    fn gain_for(&self, d: f64) -> f64 {
        proximity_gain(d, self.range_m.load(Ordering::Relaxed) as f64) * self.vol_pct.load(Ordering::Relaxed) as f64 / 100.0
    }

    /// 0 = clear, 1 = as muffled as it gets.
    fn muffle_for(&self, d: f64) -> f64 {
        let m = far_frac(d, self.range_m.load(Ordering::Relaxed) as f64) * self.muffle.load(Ordering::Relaxed) as f64 / 100.0;
        let b = self.base_muff.load(Ordering::Relaxed) as f64 / 100.0;
        1.0 - (1.0 - m) * (1.0 - b) // the constant muffle stacks with the distance one
    }

    /// How loud you hear member `id`, by the room's rules:
    /// isolate > broadcasting > sub-rooms in the pits > distance.
    fn peer_gain(&self, s: &State, id: u64, p: &VPeer, d: f64) -> f64 {
        let full = self.vol_pct.load(Ordering::Relaxed) as f64 / 100.0;
        let mut g = self.gain_for(d);
        let pres = p.present();
        let same_sub = p.sub.eq_ignore_ascii_case(&s.my_sub);
        if s.isolate != 0 && id == s.isolate {
            g = full;
        } else if s.isolate != 0 {
            g *= ISOLATE_FADE;
        } else if pres && p.bcasting && (p.sub.is_empty() || same_sub) {
            // broadcasting reaches everyone, or only their sub-room if they're in one
            // (team radio); people outside it hear them by distance as usual
            g = full;
        } else if pres && p.bcasting && !s.in_garage {
            // you're on track, they're broadcasting to another sub-room: just distance
        } else if s.pit_radio && ((s.in_garage && !s.my_sub.is_empty()) || (pres && p.in_garage && !p.sub.is_empty())) {
            // someone in the pits is in a sub-room: only people in the pits in the same
            // sub-room hear each other (on track it's back to everyone)
            g = if s.in_garage && pres && p.in_garage && same_sub { full } else { 0.0 };
        }
        g * p.user_vol.unwrap_or(1.0)
    }

    /// Where `src` is relative to a listener at `pos` heading `head`: pan is -1 (left)
    /// .. +1 (right), behind is 0 (in front or beside) .. 1 (straight behind).
    fn dir_from(&self, pos: Option<V3>, head: [f64; 2], game: u8, src: Option<V3>) -> (f64, f64) {
        if !self.direct.load(Ordering::Relaxed) {
            return (0.0, 0.0);
        }
        match rel_from(pos, head, game, src) {
            Some((right, fwd)) => {
                let l = right.hypot(fwd);
                if l < 1.0 {
                    (0.0, 0.0)
                } else {
                    (right / l, (-fwd / l).max(0.0))
                }
            }
            None => (0.0, 0.0),
        }
    }

    fn dir(&self, s: &State, p: &VPeer) -> (f64, f64) {
        if !s.have_head || s.my_pos.is_none() || !p.pos_fresh() || p.game != s.my_game {
            return (0.0, 0.0);
        }
        let now = Instant::now();
        self.dir_from(self.me_pos(s, now), s.my_head, s.my_game, p.here(now))
    }

    pub fn status(&self) -> VoiceStatus {
        let s = self.st.lock().unwrap();
        let now = Instant::now();
        let mut st = VoiceStatus {
            on: s.running,
            talking: self.talking.load(Ordering::Relaxed),
            level: self.mic_level.load(Ordering::Relaxed),
            backend: s.backend.clone(),
            err: s.err_text.clone(),
            have_pos: s.my_pos.is_some(),
            game: game_name(s.my_game),
            in_garage: s.in_garage,
            peers: HashMap::new(),
        };
        for (id, p) in &s.peers {
            let d = self.dist(&s, p);
            let rel = if d >= 0.0 && s.have_head { rel_from(self.me_pos(&s, now), s.my_head, s.my_game, p.here(now)).map(|(r, f)| [r as f32, f as f32]) } else { None };
            st.peers.insert(
                *id,
                PeerVoice {
                    talking: since(p.talk_at) < Duration::from_millis(400),
                    dist: d,
                    volume: (self.peer_gain(&s, *id, p, d) * 100.0).round() as i32,
                    muffled: (self.muffle_for(d) * 100.0).round() as i32,
                    on_track: p.pos_fresh(),
                    game: game_name(p.game),
                    broadcasting: p.present() && p.bcasting,
                    rel,
                },
            );
        }
        st
    }

    // ---------- start / stop ----------

    pub fn start(self: &Arc<Self>) {
        let (stop, mic_dev, spk_dev) = {
            let mut s = self.st.lock().unwrap();
            if s.running {
                return;
            }
            s.running = true;
            s.err_text.clear();
            let stop = Arc::new(AtomicBool::new(false));
            s.run_stop = Some(stop.clone());
            (stop, s.mic_dev.clone(), s.spk_dev.clone())
        };
        let fake = std::env::var("RINGLINK_FAKE_AUDIO").map(|v| v == "1").unwrap_or(false);
        let mic = if fake { audio::fake::open_mic(&mic_dev) } else { audio::open_mic(&mic_dev) };
        let (mic, mname) = match mic {
            Ok(m) => m,
            Err(e) => return self.fail(&stop, format!("couldn't open the microphone: {e}")),
        };
        let spk = if fake { audio::fake::open_speaker(&spk_dev) } else { audio::open_speaker(&spk_dev) };
        let (spk, sname) = match spk {
            Ok(m) => m,
            Err(e) => return self.fail(&stop, format!("couldn't open the speakers: {e}")),
        };
        if stop.load(Ordering::Relaxed) {
            return; // switched off again while the devices were opening
        }
        self.st.lock().unwrap().backend = format!("{mname} / {sname}");
        self.logf(format!("Voice chat on (mic: {mname}, output: {sname})."));
        let (a, st) = (self.clone(), stop.clone());
        std::thread::spawn(move || a.capture_loop(mic, st));
        let (a, st) = (self.clone(), stop.clone());
        std::thread::spawn(move || a.play_loop(spk, st));
        let a = self.clone();
        std::thread::spawn(move || a.pos_loop(stop));
        (self.notify)();
    }

    fn fail(&self, stop: &Arc<AtomicBool>, msg: String) {
        {
            let mut s = self.st.lock().unwrap();
            // only the current run may report a failure (an old run's devices closing isn't one)
            if !s.run_stop.as_ref().map(|r| Arc::ptr_eq(r, stop)).unwrap_or(false) {
                return;
            }
            s.running = false;
            s.err_text = msg.clone();
        }
        stop.store(true, Ordering::SeqCst);
        self.mic_level.store(0, Ordering::Relaxed);
        self.talking.store(false, Ordering::Relaxed);
        self.logf(format!("Voice chat: {msg}"));
        (self.notify)();
    }

    pub fn stop(&self) {
        {
            let mut s = self.st.lock().unwrap();
            if !s.running {
                return;
            }
            s.running = false;
            if let Some(st) = s.run_stop.take() {
                st.store(true, Ordering::SeqCst);
            }
            for p in s.peers.values_mut() {
                p.queue.clear();
            }
            s.sfx.clear();
            s.loopback.clear();
        }
        self.kick_pos();
        self.mic_level.store(0, Ordering::Relaxed);
        self.talking.store(false, Ordering::Relaxed);
        self.logf("Voice chat off.".into());
        (self.notify)();
    }

    // ---------- capture ----------

    fn capture_loop(self: Arc<Self>, mut mic: Box<dyn MicSource>, stop: Arc<AtomicBool>) {
        let (mut enc, mut low_enc) = match (VoiceEncoder::new(FULL_BITRATE), VoiceEncoder::new(LOW_BITRATE)) {
            (Ok(a), Ok(b)) => (a, b),
            (Err(e), _) | (_, Err(e)) => return self.fail(&stop, format!("couldn't start the voice codec: {e}")),
        };
        let mut dn = nnnoiseless::DenoiseState::new();
        let mut dn_was = false;
        let mut frame = vec![0i16; FRAME];
        let mut noise = 200.0f64; // running noise-floor estimate (RMS)
        // the last couple of quiet frames, sent when the gate opens so word starts aren't clipped
        let mut pre: VecDeque<Vec<i16>> = VecDeque::new();
        let mut low_buf: Vec<i16> = Vec::with_capacity(FRAME * LOW_FRAMES);
        let (mut seq, mut low_seq) = (0u16, 0u16);
        let mut was_talking = false;
        let mut last_loud: Option<Instant> = None;
        let mut ptt_at: Option<Instant> = None;
        while !stop.load(Ordering::Relaxed) {
            if let Err(e) = mic.read(&mut frame) {
                if !stop.load(Ordering::Relaxed) {
                    self.fail(&stop, format!("microphone stopped: {e}"));
                }
                return;
            }
            // noise suppression: takes the wheel motor, fans and keyboard out of your mic
            let dn_on = self.denoise.load(Ordering::Relaxed);
            if dn_on {
                if !dn_was {
                    dn = nnnoiseless::DenoiseState::new();
                }
                let mut out = [0f32; nnnoiseless::DenoiseState::FRAME_SIZE];
                let mut inp = [0f32; nnnoiseless::DenoiseState::FRAME_SIZE];
                for half in frame.chunks_mut(nnnoiseless::DenoiseState::FRAME_SIZE) {
                    for (i, s) in half.iter().enumerate() {
                        inp[i] = *s as f32;
                    }
                    dn.process_frame(&mut out, &inp);
                    for (i, s) in half.iter_mut().enumerate() {
                        *s = out[i].clamp(-32768.0, 32767.0) as i16;
                    }
                }
            }
            dn_was = dn_on;
            let rms = (frame.iter().map(|s| (*s as f64) * (*s as f64)).sum::<f64>() / FRAME as f64).sqrt();
            let lvl = ((20.0 * (rms + 1.0).log10() - 20.0) * 100.0 / 70.0).clamp(0.0, 100.0);
            self.mic_level.store(lvl as i32, Ordering::Relaxed);
            // adaptive gate: open when clearly above the noise floor
            noise = if rms < noise * 1.5 { noise * 0.98 + rms * 0.02 } else { noise * 0.999 + rms * 0.001 };
            noise = noise.max(30.0);
            let sens = self.sens.load(Ordering::Relaxed) as f64; // 0..100
            let thr = (noise * 2.5).max(1500.0 - sens * 14.0); // 100 -> ~100, 0 -> 1500
            if rms > thr {
                last_loud = Some(Instant::now());
            }
            let mut talking = if self.ptt_mode.load(Ordering::Relaxed) {
                // push to talk: while the key is held (holding broadcast talks too), plus
                // a short tail so the last word isn't clipped
                if self.ptt_key.load(Ordering::Relaxed) || self.bcast_key.load(Ordering::Relaxed) {
                    ptt_at = Some(Instant::now());
                }
                since(ptt_at) < Duration::from_millis(150)
            } else {
                since(last_loud) < Duration::from_millis(350)
            };
            talking = talking && !self.muted.load(Ordering::Relaxed) && !self.ptm_key.load(Ordering::Relaxed) && !self.host_mute.load(Ordering::Relaxed);
            self.talking.store(talking, Ordering::Relaxed);
            if !talking {
                if was_talking && !low_buf.is_empty() {
                    // stopped talking: send the rest of the low-quality packet
                    low_buf.resize(FRAME * LOW_FRAMES, 0);
                    self.send_low_packet(&mut low_enc, &low_buf, &mut low_seq);
                    low_buf.clear();
                }
                was_talking = false;
                pre.push_back(frame.clone());
                if pre.len() > 2 {
                    pre.pop_front();
                }
                continue;
            }
            let mut out: Vec<Vec<i16>> = if was_talking { vec![] } else { pre.drain(..).collect() };
            out.push(frame.clone());
            was_talking = true;
            if self.mic_test.load(Ordering::Relaxed) {
                let mut s = self.st.lock().unwrap();
                s.loopback.extend(out.iter().cloned());
                while s.loopback.len() > 25 {
                    s.loopback.pop_front();
                }
            }
            let Some(send) = self.sender() else { continue };
            for fr in &out {
                let Some(op) = enc.encode(fr) else { continue };
                let mut pkt = Vec::with_capacity(3 + op.len());
                pkt.push(VK_AUDIO);
                pkt.extend_from_slice(&seq.to_be_bytes());
                pkt.extend_from_slice(op);
                seq = seq.wrapping_add(1);
                send(&pkt);
                low_buf.extend_from_slice(fr);
                if low_buf.len() >= FRAME * LOW_FRAMES {
                    self.send_low_packet(&mut low_enc, &low_buf, &mut low_seq);
                    low_buf.clear();
                }
                if self.replay.enabled() && !self.no_record.load(Ordering::Relaxed) {
                    let mp = {
                        let s = self.st.lock().unwrap();
                        if s.my_game == GAME_LMU {
                            s.my_pos
                        } else {
                            None
                        }
                    };
                    self.replay.record(0, REC_FULL, &pkt[3..], mp);
                }
            }
        }
    }

    fn send_low_packet(&self, enc: &mut VoiceEncoder, pcm: &[i16], seq: &mut u16) {
        let Some(send) = self.low_sender() else { return };
        let Some(op) = enc.encode(pcm) else { return };
        let mut pkt = Vec::with_capacity(3 + op.len());
        pkt.push(VK_LOW);
        pkt.extend_from_slice(&seq.to_be_bytes());
        pkt.extend_from_slice(op);
        *seq = seq.wrapping_add(1);
        send(&pkt);
    }

    // ---------- receive ----------

    /// Handles a voice payload from someone in the room.
    pub fn on_packet(&self, from: u64, b: &[u8]) {
        let Some(kind) = b.first().copied() else { return };
        match kind {
            VK_AUDIO | VK_LOW => {
                if b.len() < 4 || b.len() > 1400 {
                    return;
                }
                let seq = u16::from_be_bytes([b[1], b[2]]);
                let op = &b[3..];
                let low = kind == VK_LOW;
                let mut rec: Option<Option<V3>> = None;
                {
                    let mut s = self.st.lock().unwrap();
                    let running = s.running;
                    let p = s.peers.entry(from).or_insert_with(VPeer::new);
                    if low && since(p.full_at) < Duration::from_millis(100) {
                        return; // switching from full quality: no repeats
                    }
                    let (last, dec) = if low { (&mut p.seq_low, &mut p.dec_low) } else { (&mut p.seq_full, &mut p.dec_full) };
                    let gap = last.map(|l| seq.wrapping_sub(l) as i16);
                    if matches!(gap, Some(g) if g <= 0 && g > -200) {
                        return; // late or repeated packet
                    }
                    *last = Some(seq);
                    if dec.is_none() {
                        *dec = VoiceDecoder::new().ok();
                    }
                    let Some(dec) = dec.as_mut() else { return };
                    let mut frames: Vec<Vec<i16>> = Vec::new();
                    // packets went missing: rebuild the last one from this packet's
                    // error-correction data and smooth over the ones before it
                    if let (false, Some(g @ 2..=4)) = (low, gap) {
                        for _ in 0..(g - 2) {
                            frames.extend(dec.conceal());
                        }
                        frames.extend(dec.recover(op).or_else(|| dec.conceal()));
                    }
                    frames.extend(dec.decode(op));
                    if frames.is_empty() {
                        return;
                    }
                    if low {
                        p.low_at = Some(Instant::now());
                    } else {
                        p.full_at = Some(Instant::now());
                    }
                    if frames.iter().any(|fr| fr.iter().any(|x| *x > 2000 || *x < -2000)) {
                        p.talk_at = Some(Instant::now());
                    }
                    // saved for replays whether or not you can hear it right now (voice
                    // off, too far away), unless they've opted out of being recorded
                    if !p.no_record {
                        rec = Some(if p.game == GAME_LMU && p.pos_fresh() { p.here(Instant::now()) } else { None });
                    }
                    if running {
                        p.queue.extend(frames);
                        if p.queue.len() > JITTER_MAX {
                            let drop = p.queue.len() - JITTER_TARGET;
                            p.queue.drain(..drop);
                        }
                    }
                }
                if let Some(pos) = rec {
                    if self.replay.enabled() {
                        self.replay.record(from, if low { REC_LOW } else { REC_FULL }, op, pos);
                    }
                }
            }
            VK_PRESENCE => {
                if b.len() >= 3 && b.len() >= 3 + b[2] as usize {
                    let sub: String = String::from_utf8_lossy(&b[3..3 + b[2] as usize]).chars().filter(|c| !c.is_control()).take(24).collect();
                    let mut click = false;
                    {
                        let mut s = self.st.lock().unwrap();
                        let my_sub = s.my_sub.clone();
                        let p = s.peers.entry(from).or_insert_with(VPeer::new);
                        let bc = b[1] & 2 != 0;
                        if bc && !(p.bcasting && p.present()) && (sub.is_empty() || sub.eq_ignore_ascii_case(&my_sub)) {
                            click = true;
                        }
                        p.in_garage = b[1] & 1 != 0;
                        p.bcasting = bc;
                        p.no_record = b[1] & 4 != 0;
                        p.sub = sub;
                        p.pres_at = Some(Instant::now());
                    }
                    if click {
                        self.play_sfx(sfx::radio_on());
                    }
                }
            }
            VK_SETTINGS => {
                if let Ok(vs) = serde_json::from_slice::<VoiceSettings>(&b[1..]) {
                    self.st.lock().unwrap().peers.entry(from).or_insert_with(VPeer::new).shared = Some(vs);
                }
            }
            VK_POS => {
                let mut s = self.st.lock().unwrap();
                let p = s.peers.entry(from).or_insert_with(VPeer::new);
                if b.len() == 26 {
                    let f = |o: usize| f64::from_be_bytes(b[o..o + 8].try_into().unwrap());
                    let v = V3::new(f(2), f(10), f(18));
                    if v.x.is_finite() && v.y.is_finite() && v.z.is_finite() {
                        p.game = b[1];
                        p.pos = Some(v);
                    } else {
                        p.pos = None;
                        p.game = GAME_NONE;
                    }
                } else {
                    p.pos = None;
                    p.game = GAME_NONE;
                }
                let now = Instant::now();
                p.pos_seen = Some(now);
                p.mt.update(p.pos, p.game, now);
            }
            _ => {}
        }
    }

    // ---------- playback ----------

    fn play_loop(self: Arc<Self>, mut spk: Box<dyn SpeakerSink>, stop: Arc<AtomicBool>) {
        struct Job {
            key: (bool, u64),
            fr: Option<Vec<i16>>,
            gain: f64,
            muff: f64,
            pan: f64,
        }
        let mut out = vec![0i16; FRAME * 2];
        let mut mix_l = vec![0f64; FRAME];
        let mut mix_r = vec![0f64; FRAME];
        let a_clear = 1.0 - (-std::f64::consts::TAU * muffle_cutoff(0.0) / RATE as f64).exp();
        while !stop.load(Ordering::Relaxed) {
            let mut jobs: Vec<Job> = Vec::new();
            let mut extra: Vec<Vec<i16>> = Vec::new(); // centred, as they are: mic test and sounds
            {
                let mut g = self.st.lock().unwrap();
                let now = Instant::now();
                let dop = self.doppler.load(Ordering::Relaxed) as f64 / 100.0;
                let ids: Vec<u64> = g.peers.keys().copied().collect();
                for id in ids {
                    let (d, (pan, behind), gain) = {
                        let s = &*g;
                        let p = &s.peers[&id];
                        let d = self.dist(s, p);
                        (d, self.dir(s, p), self.peer_gain(s, id, p, d))
                    };
                    let p = g.peers.get_mut(&id).unwrap();
                    let rate = p.doppler_rate(d, now, dop);
                    let fr = p.next_frame(rate);
                    let m = 1.0 - (1.0 - self.muffle_for(d)) * (1.0 - 0.3 * behind); // voices from behind you are a bit duller
                    jobs.push(Job { key: (false, id), fr, gain, muff: m, pan });
                }
                // voices recorded at this moment of the replay you're watching: queued per
                // speaker (so they play back smoothly) and heard from where their car was,
                // relative to the car the replay camera follows
                let rp_speed = self.replay.speed();
                let rp_max = ((JITTER_MAX as f64) * rp_speed.max(1.0)) as usize;
                for f in self.replay.frames(now) {
                    let q = g.rp_peers.entry(f.speaker).or_insert_with(VPeer::new);
                    q.queue.push_back(f.pcm);
                    if q.queue.len() > rp_max {
                        let drop = q.queue.len() - rp_max / 2;
                        q.queue.drain(..drop);
                    }
                    q.pos = f.pos;
                }
                if !g.rp_peers.is_empty() {
                    let (lp, lh, lok) = self.replay.listener();
                    let ids: Vec<u64> = g.rp_peers.keys().copied().collect();
                    for id in ids {
                        let q = g.rp_peers.get_mut(&id).unwrap();
                        // slow motion stretches the voice (lower), fast forward squeezes it (higher)
                        let Some(fr) = q.next_frame(rp_speed) else { continue };
                        let d = match (lp, q.pos) {
                            (Some(a), Some(b)) => a.dist(&b),
                            _ => -1.0,
                        };
                        let (pan, behind) = if lok { self.dir_from(lp, lh, GAME_LMU, q.pos) } else { (0.0, 0.0) };
                        let m = 1.0 - (1.0 - self.muffle_for(d)) * (1.0 - 0.3 * behind);
                        // replays: distance still fades and muffles, but never below 40 %
                        // so teammates far from the camera car stay audible
                        let gain = self.gain_for(d).max(0.4 * self.vol_pct.load(Ordering::Relaxed) as f64 / 100.0);
                        jobs.push(Job { key: (true, id), fr: Some(fr), gain, muff: m, pan });
                    }
                }
                if self.mic_test.load(Ordering::Relaxed) {
                    if let Some(fr) = g.loopback.pop_front() {
                        extra.push(fr);
                    }
                } else {
                    g.loopback.clear();
                }
                if !g.sfx.is_empty() {
                    let n = g.sfx.len().min(FRAME);
                    let mut fr: Vec<i16> = g.sfx.drain(..n).collect();
                    fr.resize(FRAME, 0);
                    extra.push(fr);
                }
                mix_l.iter_mut().for_each(|x| *x = 0.0);
                mix_r.iter_mut().for_each(|x| *x = 0.0);
                for j in jobs {
                    let p = if j.key.0 { g.rp_peers.get_mut(&j.key.1) } else { g.peers.get_mut(&j.key.1) };
                    let Some(p) = p else { continue };
                    // two one-pole low-passes (12 dB/octave); coefficient glides toward its target
                    let a = 1.0 - (-std::f64::consts::TAU * muffle_cutoff(j.muff) / RATE as f64).exp();
                    if p.lpa == 0.0 {
                        p.lpa = a;
                    }
                    let Some(fr) = j.fr else {
                        // silent this frame: keep filter/gain state moving
                        p.gain = j.gain;
                        p.lpa = a;
                        p.pan = j.pan;
                        p.lp1 = 0.0;
                        p.lp2 = 0.0;
                        continue;
                    };
                    let n = FRAME as f64;
                    let dp = (j.pan - p.pan) / n;
                    let da = (a - p.lpa) / n;
                    let step = (j.gain - p.gain) / n; // ramp so volume changes don't click
                    let (mut gg, mut la, mut pn) = (p.gain, p.lpa, p.pan);
                    let filtered = j.muff > 0.01 || (p.lpa - a_clear).abs() > 0.005;
                    for (i, s) in fr.iter().enumerate() {
                        gg += step;
                        la += da;
                        let mut x = *s as f64;
                        p.lp1 += la * (x - p.lp1);
                        p.lp2 += la * (p.lp1 - p.lp2);
                        if filtered {
                            x = p.lp2;
                        }
                        x *= gg * (1.0 + 0.5 * j.muff); // muffled voices sound quieter; make up some loudness
                        // equal-power pan, 1:1 in the middle
                        pn += dp;
                        let th = (pn + 1.0) * std::f64::consts::FRAC_PI_4;
                        mix_l[i] += x * (th.cos() * std::f64::consts::SQRT_2).min(1.25);
                        mix_r[i] += x * (th.sin() * std::f64::consts::SQRT_2).min(1.25);
                    }
                    p.gain = j.gain;
                    p.lpa = a;
                    p.pan = j.pan;
                }
            }
            for fr in &extra {
                for (i, s) in fr.iter().enumerate() {
                    mix_l[i] += *s as f64;
                    mix_r[i] += *s as f64;
                }
            }
            let deaf = self.deafened.load(Ordering::Relaxed);
            for i in 0..FRAME {
                for (c, x) in [mix_l[i], mix_r[i]].into_iter().enumerate() {
                    // gentle limiter so several people talking at once don't distort harshly
                    let ax = x.abs();
                    let y = if ax > 24000.0 { (24000.0 + (ax - 24000.0) * 0.25).copysign(x) } else { x };
                    out[2 * i + c] = if deaf { 0 } else { y.clamp(-32768.0, 32767.0) as i16 };
                }
            }
            if let Err(e) = spk.write(&out) {
                if !stop.load(Ordering::Relaxed) {
                    self.fail(&stop, format!("speaker output stopped: {e}"));
                }
                return;
            }
        }
    }

    // ---------- positions ----------

    fn pos_loop(self: Arc<Self>, stop: Arc<AtomicBool>) {
        let agent = games::lmu_agent();
        let bng = match BeamNgListener::start(stop.clone()) {
            Ok(b) => Some(b),
            Err(e) => {
                if self.st.lock().unwrap().pos_source.is_none() {
                    self.logf(format!("BeamNG positions unavailable: port {} is in use ({e}).", games::BEAMNG_PORT));
                }
                None
            }
        };
        let mut tick = 0u64;
        let mut last_pres: Vec<u8> = Vec::new();
        loop {
            {
                // LMU updates positions every 200 ms: catch each one quickly
                let mut k = self.kick.0.lock().unwrap();
                if !*k {
                    k = self.kick.1.wait_timeout(k, Duration::from_millis(100)).unwrap().0;
                }
                *k = false;
            }
            if stop.load(Ordering::Relaxed) {
                return;
            }
            let (sub, garage, source) = {
                let s = self.st.lock().unwrap();
                (s.my_sub.clone(), s.in_garage, s.pos_source.clone())
            };
            let mut flags = 0u8;
            if garage {
                flags |= 1;
            }
            if self.broadcasting() {
                flags |= 2;
            }
            if self.no_record.load(Ordering::Relaxed) {
                flags |= 4; // please don't save my voice in your replays
            }
            let mut pres = vec![VK_PRESENCE, flags, sub.len() as u8];
            pres.extend_from_slice(sub.as_bytes());
            if let Some(send) = self.sender() {
                if tick % 10 == 0 || pres != last_pres {
                    send(&pres);
                    last_pres = pres;
                }
                if tick % 30 == 1 {
                    // every 3 s: share your voice settings so others can copy them
                    let mut b = vec![VK_SETTINGS];
                    b.extend(serde_json::to_vec(&self.my_settings()).unwrap());
                    send(&b);
                }
            }
            tick += 1;
            // BeamNG only sends while you're driving, so a recent packet wins; otherwise ask LMU.
            let mut game = GAME_NONE;
            let mut pos: Option<V3> = None;
            let mut broadcast = false; // in the garage or watching a replay: talk to everyone at full volume
            let mut in_garage = false;
            let mode = self.game_mode.load(Ordering::Relaxed) as u8;
            if let Some(src) = source {
                if let Some((p, g)) = src() {
                    pos = Some(p);
                    game = g;
                }
            } else {
                if mode != GAME_LMU {
                    if let Some(p) = bng.as_ref().and_then(|b| b.recent(Duration::from_secs(1))) {
                        pos = Some(p);
                        game = GAME_BEAMNG;
                    }
                }
                if pos.is_none() && mode != GAME_BEAMNG {
                    let st = games::read_lmu(&agent);
                    if st.ok {
                        let in_replay = self.replay.in_replay();
                        self.replay.set_listener(st.focus, in_replay);
                        self.replay.note_drivers(&st.drivers);
                        let pit_radio = self.st.lock().unwrap().pit_radio;
                        in_garage = st.in_garage && !in_replay;
                        if in_replay || (st.in_garage && pit_radio) {
                            broadcast = true;
                        } else if st.pos.is_some() {
                            // pit radio off: the garage is just a place on the map
                            pos = st.pos;
                            game = GAME_LMU;
                        } else if st.in_garage {
                            broadcast = true;
                        }
                    }
                }
            }
            let now = Instant::now();
            let send_pos = {
                let mut s = self.st.lock().unwrap();
                s.in_garage = in_garage;
                s.me.update(pos, game, now);
                if let Some(mut h) = s.me.heading() {
                    if s.have_head && game == s.my_game {
                        // smooth a little so wiggles don't swing voices around
                        h = [0.7 * h[0] + 0.3 * s.my_head[0], 0.7 * h[1] + 0.3 * s.my_head[1]];
                        let n = h[0].hypot(h[1]);
                        if n > 1e-6 {
                            h = [h[0] / n, h[1] / n];
                        }
                    }
                    s.my_head = h;
                    s.have_head = true;
                } else if pos.is_none() || game != s.my_game {
                    s.have_head = false;
                }
                s.my_pos = pos;
                s.my_game = game;
                s.broadcast = broadcast;
                // send where the car is now, not where the game last said it was
                pos.and_then(|_| s.me.now(now))
            };
            if let Some(send) = self.sender() {
                match send_pos {
                    Some(p) if !self.broadcasting() => {
                        let mut b = Vec::with_capacity(26);
                        b.push(VK_POS);
                        b.push(game);
                        for v in [p.x, p.y, p.z] {
                            b.extend_from_slice(&v.to_be_bytes());
                        }
                        send(&b);
                    }
                    _ => send(&[VK_POS]), // no position = everyone hears you at full volume
                }
            }
        }
    }
}

/// Where `src` is relative to a listener at `pos` heading `head`, in metres:
/// (to the right, ahead).
fn rel_from(pos: Option<V3>, head: [f64; 2], game: u8, src: Option<V3>) -> Option<(f64, f64)> {
    let (a, b) = (horiz(&pos?, game), horiz(&src?, game));
    let (dx, dz) = (b[0] - a[0], b[1] - a[1]);
    // both games: facing +forward, right-hand side is (f.z, -f.x)
    Some((dx * head[1] - dz * head[0], dx * head[0] + dz * head[1]))
}

/// 0 when close (or position unknown), 1 at the edge of the range.
pub fn far_frac(d: f64, rng: f64) -> f64 {
    let near = rng * 0.1;
    if d < 0.0 || d <= near {
        0.0
    } else if d >= rng {
        1.0
    } else {
        (d - near) / (rng - near)
    }
}

/// Maps car distance to playback volume (before the volume setting).
pub fn proximity_gain(d: f64, rng: f64) -> f64 {
    let t = far_frac(d, rng);
    (1.0 - t) * (1.0 - t) // falls off a bit faster than linear, like real sound
}

/// Cutoff for the low-pass filter: ~11 kHz (clear) down to ~250 Hz (behind a wall).
pub fn muffle_cutoff(m: f64) -> f64 {
    11000.0 * (250.0f64 / 11000.0).powf(m.clamp(0.0, 1.0))
}

/// Short generated sounds.
pub mod sfx {
    use crate::audio::RATE;

    fn tone(parts: &[(f64, f64)], vol: f64) -> Vec<i16> {
        let mut out = Vec::new();
        let mut ph = 0.0f64;
        for (freq, ms) in parts {
            let n = (RATE as f64 * ms / 1000.0) as usize;
            for i in 0..n {
                // 8 ms fade in and out so it doesn't click
                let edge = (i.min(n - 1 - i) as f64 / (RATE as f64 * 0.008)).min(1.0);
                ph += std::f64::consts::TAU * freq / RATE as f64;
                out.push((ph.sin() * vol * edge) as i16);
            }
        }
        out
    }

    /// someone joined
    pub fn join() -> Vec<i16> {
        tone(&[(660.0, 70.0), (880.0, 110.0)], 3500.0)
    }

    /// someone left
    pub fn leave() -> Vec<i16> {
        tone(&[(660.0, 70.0), (440.0, 110.0)], 3500.0)
    }

    /// someone started broadcasting to you
    pub fn radio_on() -> Vec<i16> {
        tone(&[(1320.0, 45.0)], 3000.0)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn voice() -> Arc<Voice> {
        let dir = std::env::temp_dir().join(format!("rl-voice-test-{}", rand::random::<u32>()));
        let rv = ReplayVoice::new(dir);
        rv.set_enabled(false);
        Voice::new(Arc::new(|_| {}), Arc::new(|| {}), rv)
    }

    fn pos_pkt(game: u8, p: V3) -> Vec<u8> {
        let mut b = vec![VK_POS, game];
        for v in [p.x, p.y, p.z] {
            b.extend_from_slice(&v.to_be_bytes());
        }
        b
    }

    fn pres(flags: u8, sub: &str) -> Vec<u8> {
        let mut b = vec![VK_PRESENCE, flags, sub.len() as u8];
        b.extend_from_slice(sub.as_bytes());
        b
    }

    fn put_me(v: &Voice, p: V3, head: [f64; 2]) {
        let mut s = v.st.lock().unwrap();
        s.my_pos = Some(p);
        s.my_game = GAME_LMU;
        s.my_head = head;
        s.have_head = true;
    }

    #[test]
    fn volume_follows_distance() {
        assert_eq!(proximity_gain(-1.0, 150.0), 1.0);
        assert_eq!(proximity_gain(10.0, 150.0), 1.0);
        assert_eq!(proximity_gain(150.0, 150.0), 0.0);
        let mid = proximity_gain(82.5, 150.0);
        assert!((mid - 0.25).abs() < 1e-9);
        assert!(muffle_cutoff(0.0) > 10000.0 && muffle_cutoff(1.0) < 300.0);

        let v = voice();
        put_me(&v, V3::new(0.0, 0.0, 0.0), [0.0, 1.0]);
        v.on_packet(5, &pos_pkt(GAME_LMU, V3::new(0.0, 0.0, 10.0)));
        v.on_packet(6, &pos_pkt(GAME_LMU, V3::new(0.0, 0.0, 149.0)));
        v.on_packet(7, &pos_pkt(GAME_BEAMNG, V3::new(0.0, 0.0, 149.0)));
        v.on_packet(8, &[VK_POS]);
        let st = v.status();
        assert_eq!(st.peers[&5].volume, 100);
        assert!(st.peers[&6].volume <= 1 && st.peers[&6].muffled >= 49);
        assert_eq!(st.peers[&7].volume, 100, "another game: distance doesn't apply");
        assert_eq!(st.peers[&8].volume, 100, "no position: normal volume");
        assert_eq!(st.peers[&8].dist, -1.0);
        // your own volume for one person stacks on top
        v.set_peer_volume(5, 50);
        assert_eq!(v.status().peers[&5].volume, 50);
        assert!(!v.too_far(6));
        v.on_packet(6, &pos_pkt(GAME_LMU, V3::new(0.0, 0.0, 700.0)));
        assert!(v.too_far(6));
        v.bcast_btn.store(true, Ordering::Relaxed);
        assert!(!v.too_far(6), "broadcasting reaches everyone");
    }

    #[test]
    fn directions() {
        let v = voice();
        // LMU: heading +z; right-hand side is +x... as (f.z, -f.x) = (1, 0)
        put_me(&v, V3::new(0.0, 0.0, 0.0), [0.0, 1.0]);
        let (pan, behind) = v.dir_from(Some(V3::default()), [0.0, 1.0], GAME_LMU, Some(V3::new(20.0, 0.0, 0.0)));
        assert!((pan - 1.0).abs() < 1e-9 && behind == 0.0);
        let (pan, behind) = v.dir_from(Some(V3::default()), [0.0, 1.0], GAME_LMU, Some(V3::new(-20.0, 0.0, 0.0)));
        assert!((pan + 1.0).abs() < 1e-9 && behind == 0.0);
        let (pan, behind) = v.dir_from(Some(V3::default()), [0.0, 1.0], GAME_LMU, Some(V3::new(0.0, 0.0, -30.0)));
        assert!(pan.abs() < 1e-9 && (behind - 1.0).abs() < 1e-9);
        // BeamNG is z-up: the horizontal plane is x/y
        let (pan, _) = v.dir_from(Some(V3::default()), [0.0, 1.0], GAME_BEAMNG, Some(V3::new(20.0, 0.0, 5.0)));
        assert!((pan - 1.0).abs() < 1e-9);
        v.direct.store(false, Ordering::Relaxed);
        assert_eq!(v.dir_from(Some(V3::default()), [0.0, 1.0], GAME_LMU, Some(V3::new(20.0, 0.0, 0.0))), (0.0, 0.0));
        // the radar gets metres right / ahead
        v.on_packet(5, &pos_pkt(GAME_LMU, V3::new(3.0, 0.0, 40.0)));
        let rel = v.status().peers[&5].rel.unwrap();
        assert!((rel[0] - 3.0).abs() < 1e-3 && (rel[1] - 40.0).abs() < 1e-3);
    }

    #[test]
    fn room_rules() {
        let v = voice();
        put_me(&v, V3::new(0.0, 0.0, 0.0), [0.0, 1.0]);
        let far = V3::new(0.0, 0.0, 149.0);
        for id in [5, 6] {
            v.on_packet(id, &pos_pkt(GAME_LMU, far));
            v.on_packet(id, &pres(0, ""));
        }
        assert!(v.status().peers[&5].volume <= 1);
        // broadcasting: full volume wherever they are
        v.on_packet(5, &pres(2, ""));
        assert_eq!(v.status().peers[&5].volume, 100);
        // ... but a team broadcast only reaches that team
        v.on_packet(5, &pres(2, "Blue"));
        assert!(v.status().peers[&5].volume <= 1);
        v.set_sub("blue");
        assert_eq!(v.status().peers[&5].volume, 100);
        v.set_sub("");
        v.on_packet(5, &pres(0, ""));
        // isolate: only that person, everyone else faded
        v.on_packet(6, &pos_pkt(GAME_LMU, V3::new(0.0, 0.0, 5.0)));
        v.set_rules(true, 5);
        let st = v.status();
        assert_eq!(st.peers[&5].volume, 100);
        assert_eq!(st.peers[&6].volume, 15);
        v.set_rules(true, 0);
        // pits with sub-rooms: in the garage you only hear your own sub-room
        v.st.lock().unwrap().in_garage = true;
        v.set_sub("Red");
        v.on_packet(5, &pres(1, "red"));
        v.on_packet(6, &pres(1, "Blue"));
        let st = v.status();
        assert_eq!(st.peers[&5].volume, 100);
        assert_eq!(st.peers[&6].volume, 0);
        // pit radio off: sub-rooms don't apply, it's distance again
        v.set_rules(false, 0);
        assert_eq!(v.status().peers[&6].volume, 100);
        assert_eq!(v.people_subs().len(), 3);
        assert_eq!(v.peer_presence(5), Some((true, "red".to_string())));
    }

    #[test]
    fn audio_arrives_in_order_with_loss() {
        let v = voice();
        v.st.lock().unwrap().running = true;
        let mut enc = VoiceEncoder::new(FULL_BITRATE).unwrap();
        let mk = |enc: &mut VoiceEncoder, seq: u16| {
            let pcm: Vec<i16> = (0..FRAME).map(|i| (9000.0 * ((seq as usize * FRAME + i) as f64 * 0.05).sin()) as i16).collect();
            let mut p = vec![VK_AUDIO];
            p.extend_from_slice(&seq.to_be_bytes());
            p.extend_from_slice(enc.encode(&pcm).unwrap());
            p
        };
        let pk: Vec<Vec<u8>> = (0..8).map(|i| mk(&mut enc, i)).collect();
        v.on_packet(9, &pk[0]);
        v.on_packet(9, &pk[1]);
        v.on_packet(9, &pk[1]); // repeat: ignored
        v.on_packet(9, &pk[0]); // late: ignored
        assert_eq!(v.st.lock().unwrap().peers[&9].queue.len(), 2);
        v.on_packet(9, &pk[4]); // 2 and 3 lost: one concealed, one rebuilt
        assert_eq!(v.st.lock().unwrap().peers[&9].queue.len(), 5);
        assert!(v.status().peers[&9].talking);
        // garbage never panics
        v.on_packet(9, &[VK_AUDIO, 0, 9, 1, 2, 3]);
        v.on_packet(9, &[VK_PRESENCE, 0, 200, 1]);
        v.on_packet(9, &[VK_SETTINGS, b'{']);
        v.on_packet(9, &[]);
        // settings are shared so others can copy them
        let mut b = vec![VK_SETTINGS];
        b.extend(serde_json::to_vec(&VoiceSettings { vol: 150, ..Default::default() }).unwrap());
        v.on_packet(9, &b);
        assert_eq!(v.peer_settings(9).unwrap().vol, 150);
    }

    #[test]
    fn doppler_and_resampler() {
        let mut p = VPeer::new();
        let t0 = Instant::now();
        // closing at 50 m/s for a second
        let mut rate = 1.0;
        for i in 0..50 {
            // positions arrive ten times a second; the mixer asks every 20 ms
            let d = 500.0 - 5.0 * (i / 5) as f64;
            rate = p.doppler_rate(d, t0 + Duration::from_millis(20 * i), 1.0);
        }
        assert!(rate > 1.08 && rate < 1.2, "closing = higher pitch (rate {rate})");
        for i in 0..200 {
            rate = p.doppler_rate(450.0, t0 + Duration::from_millis(1000 + 20 * i), 0.0);
        }
        assert_eq!(rate, 1.0);
        // the resampler hands out whole frames and keeps the leftover
        let mut p = VPeer::new();
        for _ in 0..6 {
            p.queue.push_back(vec![1000i16; FRAME]);
        }
        let fr = p.next_frame(1.1).unwrap();
        assert_eq!(fr.len(), FRAME);
        assert!(fr.iter().all(|x| *x == 1000));
        assert!(p.next_frame(1.1).is_some());
        // straight through at rate 1
        let mut p = VPeer::new();
        for i in 0..3 {
            p.queue.push_back(vec![i as i16; FRAME]);
        }
        assert_eq!(p.next_frame(1.0).unwrap()[0], 0);
        assert_eq!(p.next_frame(1.0).unwrap()[0], 1);
        assert_eq!(p.next_frame(1.0).unwrap()[0], 2);
        assert!(p.next_frame(1.0).is_none() && p.priming);
    }
}
