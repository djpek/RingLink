//! Voice in LMU replays.
//!
//! While you drive, every voice packet (yours and everyone else's) is saved with the
//! session time it happened at (sessionInfo.currentEventTime, the session's elapsed
//! time, "ET"). When you watch a replay, LMU streams the replay position on
//! ws://127.0.0.1:6398/websocket/replaymetrics (replayStartET + currentReplayPos = the
//! session ET being shown), and the packets recorded at that moment are played again.
//! Only talking is stored (about 100 bytes per 20 ms of speech), one file per session.

use crate::audio::FRAME;
use crate::codec::VoiceDecoder;
use crate::games::LMU_REST;
use crate::motion::{Motion, V3, GAME_LMU};
use serde::Deserialize;
use std::collections::{BTreeSet, HashMap, HashSet};
use std::fs::File;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime};

const MAX_REPLAY_FILES: usize = 30;
const REC_HEADER: usize = 8 + 8 + 12 + 1 + 2;
const EXT: &str = "rlv3";
/// voice plays along with slow motion and fast forward (pitch follows, like a tape);
/// paused, rewinding or very fast skipping stays quiet
const MIN_SPEED: f64 = 0.2;
const MAX_SPEED: f64 = 4.0;

/// 20 ms full-quality packet
pub const REC_FULL: u8 = 0;
/// 60 ms low-quality packet
pub const REC_LOW: u8 = 1;

#[derive(Clone)]
struct RecFrame {
    et: f64,
    speaker: u64,
    /// where their car was (None: garage / unknown -> heard at full volume)
    pos: Option<V3>,
    kind: u8,
    data: Vec<u8>,
}

/// One recorded 20 ms frame to play now, with where it came from.
pub struct ReplayFrame {
    pub speaker: u64,
    pub pos: Option<V3>,
    pub pcm: Vec<i16>,
}

#[derive(Default)]
struct State {
    // live session clock from sessionInfo
    et: f64,
    et_at: Option<Instant>,
    realtime: bool,
    track: String,
    // recording
    rec: Option<File>,
    rec_last: f64,
    /// last timestamp written per speaker (keeps a burst evenly spaced)
    spk_last: HashMap<u64, f64>,
    // replay position from the websocket
    rp_et: f64,
    rp_at: Option<Instant>,
    rp_speed: f64,
    rp_seen: Option<Instant>,
    rp_hist: Vec<(Instant, f64)>,
    /// the replay's first session ET and length: tells which session it is
    rp_start: f64,
    rp_len: f64,
    // loaded recording for playback
    loaded: Option<PathBuf>,
    load_key: String,
    load_start: f64,
    frames: Vec<RecFrame>,
    last_t: f64,
    load_try: Option<Instant>,
    cursor: usize,
    decoders: HashMap<(u64, u8), VoiceDecoder>,
    /// who is in the session LMU is showing right now (live or replay)
    drivers: BTreeSet<String>,
    /// who has been in the session being recorded, and what's already written next to it
    rec_drivers: BTreeSet<String>,
    rec_path: Option<PathBuf>,
    rec_drivers_saved: usize,
    /// recordings the user said don't belong to the replay they're watching
    rejected: HashSet<PathBuf>,
    spans: HashMap<PathBuf, (u64, Option<SystemTime>, f64, f64)>,
    // the car the replay camera is following (the listener while watching)
    lis_pos: Option<V3>,
    lis_head: [f64; 2],
    lis_mot: Motion,
    lis_ok: bool,
}

pub struct ReplayVoice {
    off: AtomicBool,
    dir: PathBuf,
    st: Mutex<State>,
}

fn since(t: Option<Instant>, now: Instant) -> Duration {
    t.map(|t| now.saturating_duration_since(t)).unwrap_or(Duration::MAX)
}

impl ReplayVoice {
    pub fn new(dir: PathBuf) -> Arc<Self> {
        Arc::new(Self { off: AtomicBool::new(false), dir, st: Mutex::new(State { rp_speed: 1.0, ..Default::default() }) })
    }

    pub fn enabled(&self) -> bool {
        !self.off.load(Ordering::Relaxed)
    }

    pub fn set_enabled(&self, on: bool) {
        self.off.store(!on, Ordering::Relaxed);
    }

    /// LMU says you're not live (watching a replay).
    pub fn in_replay(&self) -> bool {
        let s = self.st.lock().unwrap();
        !s.realtime && since(s.et_at, Instant::now()) < Duration::from_secs(2)
    }

    /// Records where the replay camera's car is and which way it's heading.
    pub fn set_listener(&self, pos: Option<V3>, in_replay: bool) {
        let mut s = self.st.lock().unwrap();
        let (true, Some(pos)) = (in_replay, pos) else {
            s.lis_pos = None;
            s.lis_ok = false;
            return;
        };
        let now = Instant::now();
        s.lis_mot.update(Some(pos), GAME_LMU, now);
        if let Some(h) = s.lis_mot.heading() {
            s.lis_head = h;
            s.lis_ok = true;
        }
        s.lis_pos = s.lis_mot.now(now);
    }

    /// Who is in the session right now, from LMU's standings. While recording, the names
    /// are saved next to the recording; while watching a replay they're compared with
    /// those, so a different session at the same track is never played.
    pub fn note_drivers(&self, names: &[String]) {
        let mut g = self.st.lock().unwrap();
        let s = &mut *g;
        s.drivers = names.iter().map(|n| n.to_lowercase()).collect();
        if s.realtime && s.rec.is_some() {
            s.rec_drivers.extend(s.drivers.iter().cloned());
            if s.rec_drivers.len() != s.rec_drivers_saved {
                if let Some(p) = &s.rec_path {
                    let txt: Vec<&str> = s.rec_drivers.iter().map(|x| x.as_str()).collect();
                    if std::fs::write(p.with_extension("drivers"), txt.join("\n")).is_ok() {
                        s.rec_drivers_saved = s.rec_drivers.len();
                    }
                }
            }
        }
    }

    /// The recording being played with the replay you're watching, as "date time, track".
    pub fn playing(&self) -> Option<String> {
        let s = self.st.lock().unwrap();
        Self::replay_time(&s, Instant::now())?;
        let name = s.loaded.as_ref()?.file_stem()?.to_str()?.to_string();
        // 2026-11-11_20-10-05.123_Track_name
        let (date, rest) = name.split_once('_')?;
        let (time, track) = rest.split_once('_').unwrap_or((rest, ""));
        let hm: Vec<&str> = time.split('-').take(2).collect();
        Some(format!("{date} {}, {}", hm.join(":"), track.replace('_', " ")))
    }

    /// "That's not this session": stops the voice and never uses that recording for a
    /// replay again (until RingLink restarts). Another recording that fits is tried next.
    pub fn reject_playing(&self) {
        let mut s = self.st.lock().unwrap();
        if let Some(p) = s.loaded.take() {
            s.rejected.insert(p);
        }
        s.frames.clear();
        s.decoders.clear();
        s.cursor = 0;
        s.load_try = None;
        s.load_key.clear();
    }

    /// The replay camera car's position and heading (false if the heading is unknown).
    pub fn listener(&self) -> (Option<V3>, [f64; 2], bool) {
        let s = self.st.lock().unwrap();
        (s.lis_pos, s.lis_head, s.lis_ok)
    }

    /// Polls the session clock and listens for replay positions until `stop` is set.
    pub fn run(self: &Arc<Self>, stop: Arc<AtomicBool>) {
        let (a, st) = (self.clone(), stop.clone());
        std::thread::spawn(move || a.clock_loop(st));
        let a = self.clone();
        std::thread::spawn(move || a.replay_loop(stop));
    }

    fn clock_loop(&self, stop: Arc<AtomicBool>) {
        #[derive(Deserialize)]
        struct Si {
            #[serde(default, rename = "currentEventTime")]
            et: f64,
            #[serde(default, rename = "inRealtime")]
            realtime: bool,
            #[serde(default, rename = "trackName")]
            track: String,
        }
        let agent = ureq::AgentBuilder::new().timeout(Duration::from_millis(400)).build();
        while !stop.load(Ordering::Relaxed) {
            std::thread::sleep(Duration::from_millis(500));
            let Some(body) = agent.get(&format!("{LMU_REST}/rest/watch/sessionInfo")).call().ok().and_then(|r| r.into_string().ok()) else { continue };
            let Ok(si) = serde_json::from_str::<Si>(&body) else { continue };
            self.set_clock(si.et, si.realtime, &si.track, Instant::now());
        }
        self.st.lock().unwrap().rec = None;
    }

    pub(crate) fn set_clock(&self, et: f64, realtime: bool, track: &str, now: Instant) {
        let mut s = self.st.lock().unwrap();
        s.et = et;
        s.et_at = Some(now);
        s.realtime = realtime;
        s.track = track.to_string();
    }

    fn replay_loop(&self, stop: Arc<AtomicBool>) {
        #[derive(Deserialize, Default)]
        struct Body {
            #[serde(default, rename = "replayStartET")]
            start: f64,
            #[serde(default, rename = "currentReplayPos")]
            pos: f64,
            #[serde(default, rename = "replayLength")]
            len: f64,
        }
        #[derive(Deserialize)]
        struct Msg {
            #[serde(default)]
            body: Body,
        }
        while !stop.load(Ordering::Relaxed) {
            let conn = std::net::TcpStream::connect_timeout(&"127.0.0.1:6398".parse().unwrap(), Duration::from_millis(800)).ok().and_then(|tcp| {
                tcp.set_read_timeout(Some(Duration::from_secs(2))).ok();
                let req = tungstenite::http::Request::builder()
                    .uri("ws://127.0.0.1:6398/websocket/replaymetrics")
                    .header("Host", "127.0.0.1:6398")
                    .header("Origin", "http://127.0.0.1/")
                    .header("Connection", "Upgrade")
                    .header("Upgrade", "websocket")
                    .header("Sec-WebSocket-Version", "13")
                    .header("Sec-WebSocket-Key", tungstenite::handshake::client::generate_key())
                    .body(())
                    .ok()?;
                tungstenite::client(req, tcp).ok()
            });
            let Some((mut ws, _)) = conn else {
                for _ in 0..30 {
                    if stop.load(Ordering::Relaxed) {
                        return;
                    }
                    std::thread::sleep(Duration::from_millis(100));
                }
                continue;
            };
            while !stop.load(Ordering::Relaxed) {
                let msg = match ws.read() {
                    Ok(tungstenite::Message::Text(t)) => t.to_string(),
                    Ok(_) => continue,
                    Err(tungstenite::Error::Io(e)) if matches!(e.kind(), std::io::ErrorKind::WouldBlock | std::io::ErrorKind::TimedOut) => continue,
                    Err(_) => break,
                };
                if !msg.chars().take(40).collect::<String>().contains("replayMetrics") {
                    continue;
                }
                let Ok(m) = serde_json::from_str::<Msg>(&msg) else { continue };
                {
                    let mut s = self.st.lock().unwrap();
                    s.rp_start = m.body.start;
                    s.rp_len = m.body.len;
                }
                self.on_replay_pos(m.body.start + m.body.pos, Instant::now());
            }
        }
    }

    #[cfg(test)]
    pub(crate) fn set_replay_span(&self, start: f64, len: f64) {
        let mut s = self.st.lock().unwrap();
        s.rp_start = start;
        s.rp_len = len;
    }

    /// Turns LMU's replay position messages into a smooth clock. The messages can come
    /// unevenly and the position can move in steps, so the speed is the trend over the
    /// last 2 s and small errors are eased out instead of jumping (a jump back would
    /// stop the voice). Real seeks (more than about half a second off) jump straight there.
    pub(crate) fn on_replay_pos(&self, et: f64, now: Instant) {
        let mut s = self.st.lock().unwrap();
        s.rp_hist.push((now, et));
        while s.rp_hist.len() > 2 && now.saturating_duration_since(s.rp_hist[0].0) > Duration::from_secs(2) {
            s.rp_hist.remove(0);
        }
        let first = since(s.rp_seen, now) > Duration::from_secs(2);
        s.rp_seen = Some(now);
        if first {
            s.rp_et = et;
            s.rp_at = Some(now);
            s.rp_speed = 0.0;
            let l = *s.rp_hist.last().unwrap();
            s.rp_hist = vec![l];
            return;
        }
        let pred = s.rp_et + s.rp_speed * since(s.rp_at, now).as_secs_f64();
        let err = et - pred;
        if err.abs() > 0.6f64.max(0.3 * s.rp_speed.abs()) {
            // seek / jump
            s.rp_et = et;
            s.rp_at = Some(now);
            let l = *s.rp_hist.last().unwrap();
            s.rp_hist = vec![l];
            return;
        }
        s.rp_et = pred + 0.15 * err;
        s.rp_at = Some(now);
        // speed: least-squares slope of the recent positions
        let h = &s.rp_hist;
        let span = h[h.len() - 1].0.saturating_duration_since(h[0].0).as_secs_f64();
        if span >= 0.4 && h.len() >= 3 {
            let (mut sx, mut sy, mut sxx, mut sxy) = (0.0, 0.0, 0.0, 0.0);
            for (at, e) in h {
                let x = at.saturating_duration_since(h[0].0).as_secs_f64();
                sx += x;
                sy += e;
                sxx += x * x;
                sxy += x * e;
            }
            let n = h.len() as f64;
            let d = n * sxx - sx * sx;
            if d > 1e-9 {
                s.rp_speed = snap_speed((n * sxy - sx * sy) / d);
            }
        }
    }

    /// Stores one voice packet heard or spoken live.
    pub fn record(&self, speaker: u64, kind: u8, pkt: &[u8], pos: Option<V3>) {
        self.record_at(Instant::now(), speaker, kind, pkt, pos)
    }

    pub(crate) fn record_at(&self, now: Instant, speaker: u64, kind: u8, pkt: &[u8], pos: Option<V3>) {
        if !self.enabled() || pkt.is_empty() || pkt.len() > 1400 {
            return;
        }
        let mut s = self.st.lock().unwrap();
        // the current session time while driving (unknown / in a replay: nothing is saved).
        // The reported value lags the real clock by up to one 0.2 s step: aim for the middle
        if !s.realtime || since(s.et_at, now) > Duration::from_secs(2) {
            return;
        }
        let mut et = s.et + 0.1 + since(s.et_at, now).as_secs_f64();
        // new session: time went backwards or a long gap
        if s.rec.is_some() && (et < s.rec_last - 5.0 || et > s.rec_last + 900.0) {
            s.rec = None;
            s.spk_last.clear();
        }
        if s.rec.is_none() {
            let _ = std::fs::create_dir_all(&self.dir);
            let name = format!("{}_{}.{EXT}", chrono::Local::now().format("%Y-%m-%d_%H-%M-%S%.3f"), safe_name(&s.track));
            let path = self.dir.join(name);
            let Ok(f) = File::create(&path) else { return };
            s.rec = Some(f);
            s.rec_path = Some(path);
            s.rec_drivers = s.drivers.clone();
            s.rec_drivers_saved = 0;
            self.prune();
        }
        // LMU's session clock only moves in 0.2 s steps, so within a stretch of talking
        // stamp packets exactly one packet apart and only resync if we drift far off
        let step = if kind == REC_LOW { 0.06 } else { 0.02 };
        if let Some(last) = s.spk_last.get(&speaker) {
            if (et - last).abs() < 0.3 {
                let next = last + step;
                if (et - next).abs() < 0.25 {
                    et = next;
                }
            }
        }
        s.spk_last.insert(speaker, et);
        let mut b = Vec::with_capacity(REC_HEADER + pkt.len());
        b.extend_from_slice(&et.to_le_bytes());
        b.extend_from_slice(&speaker.to_le_bytes());
        let (px, py, pz) = pos.map(|p| (p.x as f32, p.y as f32, p.z as f32)).unwrap_or((f32::NAN, 0.0, 0.0));
        for v in [px, py, pz] {
            b.extend_from_slice(&v.to_le_bytes());
        }
        b.push(kind);
        b.extend_from_slice(&(pkt.len() as u16).to_le_bytes());
        b.extend_from_slice(pkt);
        if let Some(f) = s.rec.as_mut() {
            let _ = f.write_all(&b);
        }
        s.rec_last = et;
    }

    fn prune(&self) {
        let mut m = recordings(&self.dir);
        m.sort();
        while m.len() > MAX_REPLAY_FILES {
            let old = m.remove(0);
            let _ = std::fs::remove_file(old.with_extension("drivers"));
            let _ = std::fs::remove_file(old);
        }
    }

    /// The session time the replay is showing right now, and whether it's playing at a
    /// speed voice can follow.
    fn replay_time(s: &State, now: Instant) -> Option<(f64, bool)> {
        if s.realtime || since(s.rp_seen, now) > Duration::from_millis(800) {
            return None;
        }
        let sp = s.rp_speed;
        Some((s.rp_et + sp * since(s.rp_at, now).as_secs_f64(), (MIN_SPEED..=MAX_SPEED).contains(&sp)))
    }

    /// How fast the replay is playing (1 = normal), for the mixer's resampler.
    pub fn speed(&self) -> f64 {
        let sp = self.st.lock().unwrap().rp_speed;
        if !(MIN_SPEED..=MAX_SPEED).contains(&sp) || (sp - 1.0).abs() < 0.04 {
            1.0 // also snaps near-normal speed to exactly 1 so normal replays aren't resampled
        } else {
            sp
        }
    }

    /// Returns the recorded frames to play during the next 20 ms of the replay. Called
    /// from the mixer every frame.
    pub fn frames(&self, now: Instant) -> Vec<ReplayFrame> {
        if !self.enabled() {
            return vec![];
        }
        let mut g = self.st.lock().unwrap();
        let s = &mut *g;
        let Some((t, normal)) = Self::replay_time(s, now) else { return vec![] };
        if !self.ensure_loaded(s, t) {
            s.last_t = t;
            return vec![];
        }
        let t0 = s.last_t;
        if t < t0 && t > t0 - 0.15 {
            return vec![]; // the clock eased back a touch: just wait for it
        }
        s.last_t = t;
        let slack = 0.5 * s.rp_speed.max(1.0);
        if !normal || t < t0 || t - t0 > slack {
            // paused, rewind or a jump: stay quiet and re-aim
            s.cursor = s.frames.partition_point(|f| f.et < t);
            s.decoders.clear();
            return vec![];
        }
        s.cursor = s.cursor.min(s.frames.len());
        // hand out every packet whose time has come, in order; the mixer queues them per
        // speaker and plays them back to back, so timing wobble doesn't drop or bunch audio
        let mut out = Vec::new();
        while s.cursor < s.frames.len() && s.frames[s.cursor].et < t {
            let f = s.frames[s.cursor].clone();
            s.cursor += 1;
            if f.et < t - slack {
                continue; // stale (just loaded or caught up after a hiccup)
            }
            let dec = match s.decoders.entry((f.speaker, f.kind)) {
                std::collections::hash_map::Entry::Occupied(e) => e.into_mut(),
                std::collections::hash_map::Entry::Vacant(e) => match VoiceDecoder::new() {
                    Ok(d) => e.insert(d),
                    Err(_) => continue,
                },
            };
            for pcm in dec.decode(&f.data) {
                debug_assert_eq!(pcm.len(), FRAME);
                out.push(ReplayFrame { speaker: f.speaker, pos: f.pos, pcm });
            }
        }
        out
    }

    /// Picks the recording made during the session this replay shows. Every session's
    /// clock starts near zero, so time alone can't tell sessions apart: recordings must
    /// be from the same track and fit inside the replay's time span, and the one that
    /// fills the span best wins (newest on a tie). Works after restarting the game or
    /// RingLink, since recordings are files.
    fn ensure_loaded(&self, s: &mut State, t: f64) -> bool {
        let n = s.frames.len();
        let covers = n > 0 && t >= s.frames[0].et - 5.0 && t <= s.frames[n - 1].et + 5.0;
        let key = safe_name(&s.track);
        // keep the loaded recording while it covers this moment of the same replay
        // (LMU's numbers wobble a little, so only a clearly different replay re-picks)
        if covers && s.load_key == key && (s.load_start - s.rp_start).abs() < 5.0 {
            return true;
        }
        if s.load_try.map(|x| x.elapsed() < Duration::from_secs(3)).unwrap_or(false) {
            return covers;
        }
        s.load_try = Some(Instant::now());
        let files = recordings(&self.dir);
        let suffix = format!("_{key}.{EXT}");
        let is_track = |p: &Path| p.file_name().and_then(|x| x.to_str()).map(|x| x.ends_with(&suffix)).unwrap_or(false);
        let same_track = files.iter().filter(|p| is_track(p)).count();
        let mut best: Option<(PathBuf, f64)> = None;
        for p in &files {
            // only this track's recordings (unless LMU didn't give a name we have files for)
            if !s.track.is_empty() && same_track > 0 && !is_track(p) {
                continue;
            }
            if s.rejected.contains(p) {
                continue;
            }
            let Some((a, b)) = span_cached(&mut s.spans, p) else { continue };
            // the moment being shown must be inside the recording (a little slack for
            // LMU's clock steps), and the recording must fit inside the replay
            if t < a - 5.0 || t > b + 5.0 {
                continue;
            }
            let mut score = 1.0;
            // who was there: a recording from a session with other people isn't this one
            let rec_drivers = read_drivers(p);
            if !rec_drivers.is_empty() && !s.drivers.is_empty() {
                let both = rec_drivers.intersection(&s.drivers).count() as f64;
                let overlap = both / rec_drivers.len().min(s.drivers.len()) as f64;
                if overlap < 0.6 {
                    continue;
                }
                score += overlap;
            }
            if s.rp_len > 0.0 {
                // how much of the recording lies inside the replay
                let (lo, hi) = (a.max(s.rp_start - 5.0), b.min(s.rp_start + s.rp_len + 5.0));
                let inside = (hi - lo).max(0.0) / (b - a).max(1.0);
                if inside < 0.9 {
                    continue; // partly outside this replay: another session
                }
                // and how much of the replay it spans (talk only covers part, but the
                // right session's recording almost always spans the most)
                score = inside + ((b - a) / s.rp_len).min(1.0);
            }
            let better = match &best {
                None => true,
                Some((bp, bs)) => score > bs + 1e-9 || ((score - bs).abs() <= 1e-9 && p > bp),
            };
            if better {
                best = Some((p.clone(), score));
            }
        }
        let Some((best, _)) = best else {
            // nothing fits this replay: whatever was loaded belongs to another one
            if !(s.load_key == key && (s.load_start - s.rp_start).abs() < 5.0) {
                s.frames.clear();
                s.loaded = None;
                s.decoders.clear();
                return false;
            }
            return covers;
        };
        if s.loaded.as_ref() != Some(&best) || !covers {
            let fr = read_recording(&best);
            if fr.is_empty() {
                return covers;
            }
            s.cursor = fr.partition_point(|f| f.et < t);
            s.frames = fr;
            s.loaded = Some(best);
            s.decoders.clear();
        }
        s.load_key = key;
        s.load_start = s.rp_start;
        true
    }

    /// (recording right now, a replay is being watched)
    pub fn status(&self) -> (bool, bool) {
        let s = self.st.lock().unwrap();
        (s.rec.is_some() && s.realtime, Self::replay_time(&s, Instant::now()).is_some())
    }

    pub fn dir(&self) -> &Path {
        &self.dir
    }
}

/// LMU's replay speeds are fixed steps, and the measured trend wobbles a few percent, so
/// round to the nearest step (otherwise the voice pitch would waver).
pub fn snap_speed(sp: f64) -> f64 {
    if sp.abs() < 0.08 {
        return 0.0;
    }
    for st in [0.125, 0.25, 0.5, 1.0, 2.0, 4.0, 8.0] {
        if (sp - st).abs() <= 0.12 * st {
            return st;
        }
    }
    sp
}

pub fn safe_name(s: &str) -> String {
    let mut o: String = s.chars().map(|c| if c.is_ascii_alphanumeric() || c == '-' { c } else { '_' }).take(40).collect();
    if o.is_empty() {
        o = "session".into();
    }
    o
}

fn recordings(dir: &Path) -> Vec<PathBuf> {
    let mut v: Vec<PathBuf> = std::fs::read_dir(dir)
        .map(|rd| rd.filter_map(|e| e.ok()).map(|e| e.path()).filter(|p| p.extension().and_then(|x| x.to_str()) == Some(EXT)).collect())
        .unwrap_or_default();
    v.sort();
    v
}

fn read_drivers(p: &Path) -> BTreeSet<String> {
    std::fs::read_to_string(p.with_extension("drivers")).map(|t| t.lines().map(|l| l.trim().to_lowercase()).filter(|l| !l.is_empty()).collect()).unwrap_or_default()
}

fn read_recording(p: &Path) -> Vec<RecFrame> {
    let Ok(b) = std::fs::read(p) else { return vec![] };
    let mut out = Vec::new();
    let mut off = 0;
    while off + REC_HEADER <= b.len() {
        let et = f64::from_le_bytes(b[off..off + 8].try_into().unwrap());
        let speaker = u64::from_le_bytes(b[off + 8..off + 16].try_into().unwrap());
        let f = |o: usize| f32::from_le_bytes(b[off + o..off + o + 4].try_into().unwrap());
        let x = f(16);
        let pos = (!x.is_nan()).then(|| V3::new(x as f64, f(20) as f64, f(24) as f64));
        let kind = b[off + 28];
        let len = u16::from_le_bytes([b[off + 29], b[off + 30]]) as usize;
        off += REC_HEADER;
        if off + len > b.len() || !et.is_finite() {
            break; // cut off mid-write (crash / power loss): keep what's whole
        }
        out.push(RecFrame { et, speaker, pos, kind, data: b[off..off + len].to_vec() });
        off += len;
    }
    out.sort_by(|a, b| a.et.partial_cmp(&b.et).unwrap());
    out
}

/// First and last session time in a recording (remembered until the file changes).
fn span_cached(cache: &mut HashMap<PathBuf, (u64, Option<SystemTime>, f64, f64)>, p: &Path) -> Option<(f64, f64)> {
    let md = std::fs::metadata(p).ok()?;
    let (len, mt) = (md.len(), md.modified().ok());
    if let Some((l, m, a, b)) = cache.get(p) {
        if *l == len && *m == mt {
            return Some((*a, *b));
        }
    }
    let fr = read_recording(p);
    let (a, b) = (fr.first()?.et, fr.last()?.et);
    cache.insert(p.to_path_buf(), (len, mt, a, b));
    Some((a, b))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::codec::{VoiceEncoder, FULL_BITRATE};

    fn tmpdir(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("rl-replay-{tag}-{}-{}", std::process::id(), rand::random::<u32>()));
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    #[test]
    fn speeds_snap_to_lmu_steps() {
        assert_eq!(snap_speed(0.97), 1.0);
        assert_eq!(snap_speed(2.1), 2.0);
        assert_eq!(snap_speed(0.26), 0.25);
        assert_eq!(snap_speed(0.03), 0.0);
        assert_eq!(snap_speed(1.5), 1.5);
    }

    #[test]
    fn names() {
        assert_eq!(safe_name("Circuit de la Sarthe"), "Circuit_de_la_Sarthe");
        assert_eq!(safe_name(""), "session");
    }

    /// Record a stretch of talking live, then "watch the replay" and get it back in
    /// order at the right moments; a different track's recording is never picked.
    #[test]
    fn record_then_replay() {
        let dir = tmpdir("a");
        let rv = ReplayVoice::new(dir.clone());
        let mut enc = VoiceEncoder::new(FULL_BITRATE).unwrap();
        let t0 = Instant::now();
        // live at Monza, session time 100 s; 2 s of talking from speaker 7
        for i in 0..100u64 {
            let now = t0 + Duration::from_millis(20 * i);
            if i % 10 == 0 {
                rv.set_clock(100.0 + 0.02 * i as f64, true, "Monza", now);
            }
            let pcm: Vec<i16> = (0..FRAME).map(|k| (6000.0 * ((i as usize * FRAME + k) as f64 * 0.03).sin()) as i16).collect();
            rv.record_at(now, 7, REC_FULL, enc.encode(&pcm).unwrap(), Some(V3::new(1.0, 2.0, 3.0)));
        }
        assert_eq!(rv.status().0, true);
        // another session at another track
        let other = ReplayVoice::new(dir.clone());
        other.set_clock(100.0, true, "Spa", t0);
        other.record_at(t0, 9, REC_FULL, &[1, 2, 3], None);
        drop(other);
        assert_eq!(recordings(&dir).len(), 2);

        // later: watching the Monza replay from session time 99.5 s at normal speed
        let t1 = t0 + Duration::from_secs(10);
        rv.set_clock(0.0, false, "Monza", t1);
        rv.set_replay_span(0.0, 300.0);
        let mut got = 0;
        let mut speakers = std::collections::HashSet::new();
        for i in 0..200u64 {
            let now = t1 + Duration::from_millis(20 * i);
            if i % 5 == 0 {
                rv.set_clock(0.0, false, "Monza", now);
                rv.on_replay_pos(99.5 + 0.02 * i as f64, now);
            }
            for f in rv.frames(now) {
                assert_eq!(f.pcm.len(), FRAME);
                assert_eq!(f.pos, Some(V3::new(1.0, 2.0, 3.0)));
                speakers.insert(f.speaker);
                got += 1;
            }
        }
        assert!(got >= 90, "most of the 100 frames come back (got {got})");
        assert_eq!(speakers.into_iter().collect::<Vec<_>>(), vec![7]);
        let _ = std::fs::remove_dir_all(dir);
    }

    /// Two sessions at the same track with the same clock range: the one with the
    /// people who are in the replay is played, never the other (this used to pick the
    /// newest), and "wrong session" drops a recording for good.
    #[test]
    fn same_track_sessions_are_told_apart_by_who_was_there() {
        let dir = tmpdir("c");
        let names = |l: &[&str]| l.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        let t0 = Instant::now();
        let mut speakers = vec![];
        for (speaker, drivers) in [(1u64, names(&["Me", "Friend A"])), (2u64, names(&["Me", "Someone Else", "Another"]))] {
            let rv = ReplayVoice::new(dir.clone());
            rv.set_clock(60.0, true, "Road Atlanta", t0);
            rv.note_drivers(&drivers);
            for i in 0..50u64 {
                let now = t0 + Duration::from_millis(20 * i);
                rv.record_at(now, speaker, REC_FULL, &[0xf8, 0xff, 0xfe], None);
                rv.note_drivers(&drivers);
            }
            speakers.push(speaker);
            std::thread::sleep(Duration::from_millis(5)); // different file names
        }
        assert_eq!(recordings(&dir).len(), 2);
        let watch = |rv: &ReplayVoice, drivers: &[String]| -> HashSet<u64> {
            let t1 = Instant::now() + Duration::from_secs(4);
            rv.set_clock(0.0, false, "Road Atlanta", t1);
            rv.set_replay_span(0.0, 200.0);
            rv.note_drivers(drivers);
            let mut got = HashSet::new();
            for i in 0..80u64 {
                let now = t1 + Duration::from_millis(20 * i);
                if i % 5 == 0 {
                    rv.set_clock(0.0, false, "Road Atlanta", now);
                    rv.on_replay_pos(59.8 + 0.02 * i as f64, now);
                }
                got.extend(rv.frames(now).iter().map(|f| f.speaker));
            }
            got
        };
        // the older session (with Friend A) is the one being watched
        let rv = ReplayVoice::new(dir.clone());
        let _ = watch(&rv, &names(&["Me", "Friend A"]));
        assert!(rv.playing().unwrap().contains("Road Atlanta"));
        assert_eq!(rv.st.lock().unwrap().frames[0].speaker, 1);
        // a replay of a third session nobody recorded: silence
        let rv = ReplayVoice::new(dir.clone());
        let got = watch(&rv, &names(&["Me", "Stranger 1", "Stranger 2", "Stranger 3"]));
        assert!(got.is_empty() && rv.playing().is_none());
        // "wrong session": that recording is dropped and not picked again
        let rv = ReplayVoice::new(dir.clone());
        let _ = watch(&rv, &names(&["Me", "Friend A"]));
        rv.reject_playing();
        let _ = watch(&rv, &names(&["Me", "Friend A"]));
        assert!(rv.playing().is_none());
        let _ = std::fs::remove_dir_all(dir);
    }

    #[test]
    fn nothing_is_saved_while_watching_a_replay() {
        let dir = tmpdir("b");
        let rv = ReplayVoice::new(dir.clone());
        let now = Instant::now();
        rv.set_clock(50.0, false, "Monza", now);
        rv.record_at(now, 1, REC_FULL, &[1, 2, 3], None);
        rv.set_enabled(false);
        rv.set_clock(50.0, true, "Monza", now);
        rv.record_at(now, 1, REC_FULL, &[1, 2, 3], None);
        assert!(recordings(&dir).is_empty());
        let _ = std::fs::remove_dir_all(dir);
    }
}
