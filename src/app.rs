//! Settings and the glue between the room, voice chat and key bindings.

use crate::bind::{Binder, ACT_BROADCAST, ACT_MUTE, ACT_PTM, ACT_PTT};
use crate::codes::{new_room_code, normalize_code, room_secrets};
use crate::replay::ReplayVoice;
use crate::room::{Room, RoomOptions, MAX_ROOM_SIZE};
use crate::signal::{LogFn, MemHub, MemSignal, Signaler};
use crate::voice::{sfx, Voice, VoiceSettings, CULL_ROOM_ABOVE, VK_POS, VK_PRESENCE, VK_SETTINGS};
use serde::{Deserialize, Serialize};
use std::collections::{HashMap, HashSet};
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};

pub const APP_VERSION: &str = env!("CARGO_PKG_VERSION");

fn yes() -> bool {
    true
}

/// Saved in the same place and with the same names as RingLink 4, so your name, devices,
/// bindings and per-person volumes carry over.
#[derive(Serialize, Deserialize, Clone, Debug)]
#[serde(default)]
pub struct Settings {
    #[serde(rename = "displayName")]
    pub display_name: String,
    #[serde(rename = "maxPeople")]
    pub max_people: usize,
    /// 0 auto, 1 LMU, 2 BeamNG
    pub game: i32,
    /// background (or window) opacity %
    pub opacity: i32,
    /// panels & buttons opacity % when see-through
    #[serde(rename = "panelOpacity")]
    pub panel_opacity: i32,
    #[serde(default = "yes")]
    pub voice: bool,
    #[serde(rename = "micDevice")]
    pub mic_device: String,
    #[serde(rename = "speakerDevice")]
    pub speaker_device: String,
    #[serde(rename = "voiceSens")]
    pub voice_sens: i32,
    #[serde(rename = "voiceVol")]
    pub voice_vol: i32,
    #[serde(rename = "voiceRange")]
    pub voice_range: i32,
    #[serde(rename = "voiceMuffle")]
    pub voice_muffle: i32,
    #[serde(rename = "voiceBaseMuffle")]
    pub voice_base_muffle: i32,
    #[serde(rename = "voiceDirectional")]
    pub voice_directional: bool,
    #[serde(rename = "voiceDoppler")]
    pub voice_doppler: i32,
    #[serde(rename = "replayVoice")]
    pub replay_voice: bool,
    #[serde(rename = "broadcastBind")]
    pub broadcast_bind: String,
    #[serde(rename = "pushToTalkBind")]
    pub ptt_bind: String,
    #[serde(rename = "pushToMuteBind")]
    pub ptm_bind: String,
    #[serde(rename = "muteToggleBind")]
    pub mute_bind: String,
    /// keep my voice out of replays (mine and others')
    #[serde(rename = "noReplayRecord")]
    pub no_record: bool,
    /// false = voice activated
    #[serde(rename = "pushToTalk")]
    pub push_to_talk: bool,
    #[serde(rename = "othersVoicesOpen")]
    pub fx_open: bool,
    /// per-person volume % by display name
    #[serde(rename = "peerVolume")]
    pub peer_volume: HashMap<String, i32>,
    // new in 5.0
    #[serde(rename = "noiseSuppression")]
    pub denoise: bool,
    pub sounds: bool,
    /// the room you were last in, for one-click rejoin
    #[serde(rename = "lastRoom")]
    pub last_room: String,
    #[serde(rename = "alwaysOnTop")]
    pub always_on_top: bool,
    #[serde(rename = "showRadar")]
    pub show_radar: bool,
}

impl Default for Settings {
    fn default() -> Self {
        let v = VoiceSettings::default();
        Self {
            display_name: String::new(),
            max_people: 4,
            game: 0,
            opacity: 100,
            panel_opacity: 90,
            voice: true,
            mic_device: String::new(),
            speaker_device: String::new(),
            voice_sens: 60,
            voice_vol: v.vol,
            voice_range: v.range,
            voice_muffle: v.muffle,
            voice_base_muffle: v.base_muffle,
            voice_directional: v.directional,
            voice_doppler: v.doppler,
            replay_voice: true,
            broadcast_bind: String::new(),
            ptt_bind: String::new(),
            ptm_bind: String::new(),
            mute_bind: String::new(),
            no_record: false,
            push_to_talk: false,
            fx_open: false,
            peer_volume: HashMap::new(),
            denoise: true,
            sounds: true,
            last_room: String::new(),
            always_on_top: false,
            show_radar: true,
        }
    }
}

pub fn config_dir() -> PathBuf {
    if let Ok(d) = std::env::var("RINGLINK_CONFIG_DIR") {
        return PathBuf::from(d);
    }
    dirs::config_dir().unwrap_or_else(|| PathBuf::from(".")).join("RingLink")
}

fn host_name() -> String {
    for k in ["HOSTNAME", "COMPUTERNAME"] {
        if let Ok(h) = std::env::var(k) {
            if !h.trim().is_empty() {
                return h.trim().to_string();
            }
        }
    }
    std::fs::read_to_string("/etc/hostname").ok().map(|s| s.trim().to_string()).filter(|s| !s.is_empty()).unwrap_or_else(|| "Driver".into())
}

pub struct AppOptions {
    /// in-process relay instead of the public one (tests, demo)
    pub hub: Option<Arc<MemHub>>,
    pub no_stun: bool,
    pub config_dir: Option<PathBuf>,
}

impl Default for AppOptions {
    fn default() -> Self {
        Self { hub: None, no_stun: false, config_dir: None }
    }
}

pub struct App {
    pub settings: Mutex<Settings>,
    room: Mutex<Option<Arc<Room>>>,
    logs: Mutex<Vec<String>>,
    notify: Arc<dyn Fn() + Send + Sync>,
    pub voice: Arc<Voice>,
    pub bind: Arc<Binder>,
    /// the room's permanent sub-rooms last time we looked
    last_subs: Mutex<Vec<String>>,
    /// who was in the room last time we looked (for join / leave sounds)
    last_members: Mutex<Option<HashSet<u64>>>,
    opts: AppOptions,
    dir: PathBuf,
    stop: Arc<AtomicBool>,
    pos_n: AtomicU64,
}

impl App {
    pub fn new(notify: Arc<dyn Fn() + Send + Sync>, opts: AppOptions) -> Arc<App> {
        let dir = opts.config_dir.clone().unwrap_or_else(config_dir);
        let mut s: Settings = std::fs::read(dir.join("settings.json")).ok().and_then(|b| serde_json::from_slice(&b).ok()).unwrap_or_default();
        if !(2..=MAX_ROOM_SIZE).contains(&s.max_people) {
            s.max_people = 4;
        }
        if s.display_name.trim().is_empty() {
            s.display_name = host_name();
        }
        if s.opacity <= 0 {
            s.opacity = 100;
        }
        if s.panel_opacity <= 0 {
            s.panel_opacity = 90;
        }
        if s.voice_sens <= 0 {
            s.voice_sens = 60;
        }
        if s.voice_range <= 0 {
            s.voice_range = VoiceSettings::default().range;
        }
        let logs: Mutex<Vec<String>> = Mutex::new(Vec::new());
        let stop = Arc::new(AtomicBool::new(false));
        let replay = ReplayVoice::new(dir.join("voice-replays"));
        replay.set_enabled(s.replay_voice);
        // session clock + replay position, for as long as the app runs
        replay.run(stop.clone());
        let bind = Binder::new();
        for (a, id) in [(ACT_BROADCAST, &s.broadcast_bind), (ACT_PTT, &s.ptt_bind), (ACT_PTM, &s.ptm_bind), (ACT_MUTE, &s.mute_bind)] {
            bind.set_bind(a, id);
        }
        let app = Arc::new_cyclic(|weak: &std::sync::Weak<App>| {
            let w = weak.clone();
            let log: LogFn = Arc::new(move |m| {
                if let Some(a) = w.upgrade() {
                    a.log(m);
                }
            });
            let w = weak.clone();
            let poke: Arc<dyn Fn() + Send + Sync> = Arc::new(move || {
                if let Some(a) = w.upgrade() {
                    a.poke();
                }
            });
            let voice = Voice::new(log, poke, replay);
            App {
                settings: Mutex::new(s.clone()),
                room: Mutex::new(None),
                logs,
                notify,
                voice,
                bind: bind.clone(),
                last_subs: Mutex::new(vec![]),
                last_members: Mutex::new(None),
                opts,
                dir,
                stop: stop.clone(),
                pos_n: AtomicU64::new(0),
            }
        });
        let v = &app.voice;
        v.sens.store(s.voice_sens, Ordering::Relaxed);
        v.apply_settings(&VoiceSettings {
            vol: s.voice_vol,
            range: s.voice_range,
            muffle: s.voice_muffle,
            base_muffle: s.voice_base_muffle,
            directional: s.voice_directional,
            doppler: s.voice_doppler,
        });
        v.ptt_mode.store(s.push_to_talk, Ordering::Relaxed);
        v.no_record.store(s.no_record, Ordering::Relaxed);
        v.denoise.store(s.denoise, Ordering::Relaxed);
        v.sounds.store(s.sounds, Ordering::Relaxed);
        v.game_mode.store(s.game, Ordering::Relaxed);
        v.set_devices(&s.mic_device, &s.speaker_device);

        let w = Arc::downgrade(&app);
        bind.set_on_change(move |act, held| {
            let Some(a) = w.upgrade() else { return };
            match act {
                ACT_BROADCAST => {
                    a.voice.bcast_key.store(held, Ordering::Relaxed);
                    a.voice.kick_pos();
                }
                ACT_PTT => a.voice.ptt_key.store(held, Ordering::Relaxed),
                ACT_PTM => a.voice.ptm_key.store(held, Ordering::Relaxed),
                ACT_MUTE => {
                    if held {
                        a.voice.muted.fetch_xor(true, Ordering::Relaxed);
                    }
                }
                _ => {}
            }
            a.poke();
        });
        let w = Arc::downgrade(&app);
        bind.set_on_capture(move |act, id| {
            let Some(a) = w.upgrade() else { return };
            {
                let mut s = a.settings.lock().unwrap();
                match act {
                    ACT_BROADCAST => s.broadcast_bind = id.to_string(),
                    ACT_PTT => s.ptt_bind = id.to_string(),
                    ACT_PTM => s.ptm_bind = id.to_string(),
                    ACT_MUTE => s.mute_bind = id.to_string(),
                    _ => {}
                }
            }
            a.save();
            a.poke();
        });
        bind.run(stop);
        app
    }

    pub fn clear_bind(&self, act: &str) {
        self.bind.set_bind(act, "");
        {
            let mut s = self.settings.lock().unwrap();
            match act {
                ACT_BROADCAST => s.broadcast_bind.clear(),
                ACT_PTT => s.ptt_bind.clear(),
                ACT_PTM => s.ptm_bind.clear(),
                ACT_MUTE => s.mute_bind.clear(),
                _ => {}
            }
        }
        self.save();
    }

    /// Copies what the voice engine is using back into the settings and saves them.
    pub fn save(&self) {
        let js = {
            let mut s = self.settings.lock().unwrap();
            let v = self.voice.my_settings();
            s.voice_vol = v.vol;
            s.voice_range = v.range;
            s.voice_muffle = v.muffle;
            s.voice_base_muffle = v.base_muffle;
            s.voice_directional = v.directional;
            s.voice_doppler = v.doppler;
            s.voice_sens = self.voice.sens.load(Ordering::Relaxed);
            s.push_to_talk = self.voice.ptt_mode.load(Ordering::Relaxed);
            s.no_record = self.voice.no_record.load(Ordering::Relaxed);
            s.denoise = self.voice.denoise.load(Ordering::Relaxed);
            s.sounds = self.voice.sounds.load(Ordering::Relaxed);
            s.replay_voice = self.voice.replay.enabled();
            serde_json::to_vec_pretty(&*s).unwrap()
        };
        // write-then-rename, so a crash can't leave half a file
        let _ = std::fs::create_dir_all(&self.dir);
        let tmp = self.dir.join("settings.json.tmp");
        if std::fs::write(&tmp, js).is_ok() {
            let _ = std::fs::rename(&tmp, self.dir.join("settings.json"));
        }
    }

    pub fn log(&self, msg: String) {
        let line = format!("{} {msg}", chrono::Local::now().format("%H:%M:%S"));
        {
            let mut l = self.logs.lock().unwrap();
            l.push(line);
            if l.len() > 300 {
                let n = l.len() - 300;
                l.drain(..n);
            }
        }
        self.poke();
    }

    pub fn poke(&self) {
        (self.notify)()
    }

    pub fn logs(&self) -> Vec<String> {
        self.logs.lock().unwrap().clone()
    }

    pub fn room(&self) -> Option<Arc<Room>> {
        self.room.lock().unwrap().clone()
    }

    /// Voice chat is on unless the user switched it off.
    pub fn voice_wanted(&self) -> bool {
        self.settings.lock().unwrap().voice
    }

    pub fn set_voice(self: &Arc<Self>, on: bool) {
        self.settings.lock().unwrap().voice = on;
        self.save();
        if on {
            let v = self.voice.clone();
            std::thread::spawn(move || v.start());
        } else {
            self.voice.stop();
        }
    }

    /// Makes a new room code and opens it.
    pub fn create_room(self: &Arc<Self>) -> Result<(), String> {
        self.open(&new_room_code(), true)
    }

    /// Joins the room with this code.
    pub fn join_room(self: &Arc<Self>, code: &str) -> Result<(), String> {
        self.open(code, false)
    }

    fn open(self: &Arc<Self>, code: &str, creator: bool) -> Result<(), String> {
        let c = normalize_code(code)?;
        self.leave_room();
        let (name, max) = {
            let s = self.settings.lock().unwrap();
            (s.display_name.trim().to_string(), s.max_people)
        };
        let sig: Option<Arc<dyn Signaler>> = self.opts.hub.as_ref().map(|hub| {
            let (_, topic) = room_secrets(&c);
            Arc::new(MemSignal { hub: hub.clone(), topic }) as Arc<dyn Signaler>
        });
        let w = Arc::downgrade(self);
        let log: LogFn = Arc::new(move |m| {
            if let Some(a) = w.upgrade() {
                a.log(m);
            }
        });
        let r = Room::open(&c, &name, max, creator, RoomOptions { sig, no_stun: self.opts.no_stun }, log)?;
        let v = self.voice.clone();
        r.set_on_voice(move |from, p| v.on_packet(from, p));
        let (w, wr) = (Arc::downgrade(self), Arc::downgrade(&r));
        r.set_on_change(move || {
            if let (Some(a), Some(r)) = (w.upgrade(), wr.upgrade()) {
                a.on_room_change(&r);
            }
        });
        *self.room.lock().unwrap() = Some(r.clone());
        *self.last_members.lock().unwrap() = None;
        let (wa, wr) = (Arc::downgrade(self), Arc::downgrade(&r));
        self.voice.set_sender(Some(Arc::new(move |p: &[u8]| {
            let (Some(a), Some(r)) = (wa.upgrade(), wr.upgrade()) else { return };
            // big rooms: full-quality voice only goes to people close enough to hear it;
            // positions go to far-away people 2x a second instead of 10x (enough to know
            // when they're near)
            if p.is_empty() || r.size() <= CULL_ROOM_ABOVE || p[0] == VK_SETTINGS || p[0] == VK_PRESENCE {
                r.send_voice(p);
                return;
            }
            if p[0] == VK_POS && a.pos_n.fetch_add(1, Ordering::Relaxed) % 5 == 0 {
                r.send_voice(p);
                return;
            }
            let v = a.voice.clone();
            r.send_voice_skip(p, Some(&move |id| v.too_far(id)));
        })));
        // far-away people in big rooms still get your voice, in low quality (about a
        // third of the upload), so they can hear it in their replays
        let (wa, wr) = (Arc::downgrade(self), Arc::downgrade(&r));
        self.voice.set_low_sender(Some(Arc::new(move |p: &[u8]| {
            let (Some(a), Some(r)) = (wa.upgrade(), wr.upgrade()) else { return };
            if r.size() <= CULL_ROOM_ABOVE {
                return; // small rooms: everyone already gets full quality
            }
            let v = a.voice.clone();
            r.send_voice_skip(p, Some(&move |id| !v.too_far(id)));
        })));
        if creator {
            self.log(format!("Room {} created (up to {max} people). Send the code to your friends.", r.code));
        } else {
            self.log(format!("Joining room {}...", r.code));
        }
        self.settings.lock().unwrap().last_room = r.code.clone();
        self.save();
        if self.voice_wanted() {
            let v = self.voice.clone();
            std::thread::spawn(move || v.start());
        }
        self.poke();
        Ok(())
    }

    fn on_room_change(&self, r: &Arc<Room>) {
        // forget voice state for people who left
        let members = r.members();
        let live: HashSet<u64> = members.iter().map(|m| m.id).collect();
        for m in members.iter().filter(|m| !m.is_self) {
            // your volume for them, remembered by name
            let pv = self.settings.lock().unwrap().peer_volume.get(&m.name).copied();
            if let Some(pv) = pv {
                self.voice.set_peer_volume(m.id, pv);
            }
        }
        for id in self.voice.peer_ids() {
            if !live.contains(&id) {
                self.voice.remove_peer(id);
            }
        }
        {
            let mut lm = self.last_members.lock().unwrap();
            if let Some(old) = lm.as_ref() {
                if live.iter().any(|id| !old.contains(id)) {
                    self.voice.play_sfx(sfx::join());
                } else if old.iter().any(|id| !live.contains(id)) {
                    self.voice.play_sfx(sfx::leave());
                }
            }
            *lm = Some(live);
        }
        let rules = r.rules();
        self.voice.set_rules(rules.pit_radio, rules.isolate);
        // a host / mod removed the (permanent) sub-room you're in: leave it
        {
            let mut ls = self.last_subs.lock().unwrap();
            let sub = self.voice.sub();
            let has = |l: &[String]| l.iter().any(|s| s.eq_ignore_ascii_case(&sub));
            if !sub.is_empty() && has(&ls) && !has(&rules.subs) {
                self.voice.set_sub("");
            }
            *ls = rules.subs.clone();
        }
        let m = r.muted_by_host(r.id);
        if m != self.voice.host_mute.swap(m, Ordering::Relaxed) {
            self.log(if m { "You were muted by the host or a mod.".into() } else { "You were unmuted.".into() });
        }
        self.poke();
    }

    pub fn leave_room(&self) {
        let Some(r) = self.room.lock().unwrap().take() else { return };
        self.voice.set_sender(None);
        self.voice.set_low_sender(None);
        self.voice.host_mute.store(false, Ordering::Relaxed);
        self.voice.bcast_btn.store(false, Ordering::Relaxed);
        self.voice.stop();
        r.leave();
        self.log(format!("Left room {}.", r.code));
        self.poke();
    }

    pub fn set_game(&self, g: i32) {
        self.settings.lock().unwrap().game = g;
        self.voice.game_mode.store(g, Ordering::Relaxed);
        self.save();
    }

    pub fn set_devices(&self, mic: &str, spk: &str) {
        {
            let mut s = self.settings.lock().unwrap();
            s.mic_device = mic.to_string();
            s.speaker_device = spk.to_string();
        }
        self.save();
        let (v, mic, spk) = (self.voice.clone(), mic.to_string(), spk.to_string());
        std::thread::spawn(move || v.set_devices(&mic, &spk));
    }

    /// Your own volume for one person, remembered by their name.
    pub fn set_peer_volume(&self, id: u64, name: &str, pct: i32) {
        self.voice.set_peer_volume(id, pct);
        self.settings.lock().unwrap().peer_volume.insert(name.to_string(), pct);
    }

    pub fn shutdown(&self) {
        self.leave_room();
        self.stop.store(true, Ordering::SeqCst);
    }
}
