//! A room: everyone who typed the same short code. Every member connects directly to
//! every other member (a small mesh) over one UDP socket, using UDP hole punching.
//! Addresses are swapped through the signaling relay (signal.rs); everything is
//! encrypted with a key derived from the room code.

use crate::codes::{format_code, normalize_code, room_secrets};
use crate::roles::RoleState;
use crate::signal::{LogFn, NtfySignal, Signaler};
use crate::stun;
use base64::Engine;
use chacha20poly1305::aead::{Aead, KeyInit, Payload};
use chacha20poly1305::{XChaCha20Poly1305, XNonce};
use ed25519_dalek::VerifyingKey;
use rand::RngCore;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::net::{IpAddr, Ipv4Addr, SocketAddr, ToSocketAddrs, UdpSocket};
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::mpsc::{sync_channel, Receiver, SyncSender};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

pub const MAX_ROOM_SIZE: usize = 40;

pub const PK_PUNCH: u8 = 1;
pub const PK_PING: u8 = 2;
pub const PK_PONG: u8 = 3;
pub const PK_VOICE: u8 = 4;
pub const PK_BYE: u8 = 5;
/// who I can reach directly and how fast: [id u64, rtt ms u16]...
pub const PK_LINKS: u8 = 6;
/// please pass this on: [target id u64][sealed packet from me]
pub const PK_RELAY: u8 = 7;
/// passed on for someone: [sealed packet from them]
pub const PK_RELAYED: u8 = 8;
/// signed host / mod change: [ed25519 signature 64][JSON]
pub const PK_CTRL: u8 = 9;

const ANNOUNCE_EVERY: Duration = Duration::from_secs(240);
const ANNOUNCE_FRESH_MS: i64 = 6 * 60 * 1000;
const PEER_TIMEOUT: Duration = Duration::from_secs(30);
/// a relay must beat the direct path by this much
const RELAY_GAIN: Duration = Duration::from_millis(40);
const LINKS_FRESH: Duration = Duration::from_secs(5);
const NONCE: usize = 24;
const TAG: usize = 16;

pub fn now_ms() -> i64 {
    SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_millis() as i64).unwrap_or(0)
}

pub fn id_hex(id: u64) -> String {
    format!("{id:016x}")
}

pub fn parse_id(s: &str) -> Option<u64> {
    u64::from_str_radix(s.trim(), 16).ok()
}

fn since(t: Option<Instant>) -> Duration {
    t.map(|t| t.elapsed()).unwrap_or(Duration::MAX)
}

#[derive(Serialize, Deserialize, Default, Debug)]
pub(crate) struct Announce {
    /// "a" announce, "b" bye
    pub k: String,
    pub id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub n: String,
    /// public UDP address (from STUN)
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub p: String,
    /// LAN UDP address
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub l: String,
    /// room size limit (set by whoever made the room)
    #[serde(default)]
    pub m: usize,
    /// unix ms when this member joined
    #[serde(default)]
    pub j: i64,
    /// Ed25519 public key, for signed host / mod changes
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub pk: String,
    /// the one who made the room
    #[serde(default)]
    pub h: bool,
    /// unix ms when sent
    pub t: i64,
}

pub(crate) struct Peer {
    pub id: u64,
    pub name: String,
    pub cands: Vec<SocketAddr>,
    /// confirmed address (we got a packet from it)
    pub addr: Option<SocketAddr>,
    pub last_recv: Option<Instant>,
    pub last_ann_ms: i64,
    pub last_ann: Option<Instant>,
    pub joined: i64,
    pub rtt: Duration,
    pub gone: bool,
    /// who they reach directly (from their PK_LINKS)
    pub links: HashMap<u64, Duration>,
    pub links_at: Option<Instant>,
    /// last packet from them that came through someone else
    pub last_relay: Option<Instant>,
    /// relay we last used to reach them (0 = direct)
    pub via: u64,
    pub key: Option<VerifyingKey>,
}

#[derive(Clone, Debug, Default)]
pub struct MemberInfo {
    pub id: u64,
    pub name: String,
    pub is_self: bool,
    pub connected: bool,
    pub rtt: Duration,
    /// name of the person relaying for them ("" = direct)
    pub via: String,
    pub host: bool,
    pub moderator: bool,
    /// muted for everyone by the host or a mod
    pub muted: bool,
}

pub(crate) struct Inner {
    pub max: usize,
    pub joined: i64,
    pub pub_addr: Option<SocketAddr>,
    pub lan: Option<SocketAddr>,
    pub peers: HashMap<u64, Peer>,
    pub state: String,
    pub last_ann: Option<Instant>,
    pub roles: RoleState,
}

type VoiceCb = Arc<dyn Fn(u64, &[u8]) + Send + Sync>;
type ChangeCb = Arc<dyn Fn() + Send + Sync>;

pub struct Room {
    /// formatted, e.g. K7QM-29XD
    pub code: String,
    pub id: u64,
    pub name: String,
    pub creator: bool,

    aead: XChaCha20Poly1305,
    sig: Arc<dyn Signaler>,
    sock: UdpSocket,
    stun_tx: SyncSender<Vec<u8>>,
    stun_rx: Mutex<Receiver<Vec<u8>>>,
    log: LogFn,
    on_voice: RwLock<Option<VoiceCb>>,
    on_change: RwLock<Option<ChangeCb>>,
    pub(crate) inner: Mutex<Inner>,
    stop: Arc<AtomicBool>,
    closed: AtomicBool,
    bytes_in: AtomicI64,
    bytes_out: AtomicI64,
    epoch: Instant,
    /// tests: pretend we can't reach these UDP ports directly
    pub(crate) blocked_ports: Mutex<Vec<u16>>,
    /// tests: skip STUN (no internet in CI)
    no_stun: bool,
}

pub struct RoomOptions {
    pub sig: Option<Arc<dyn Signaler>>,
    pub no_stun: bool,
}

impl Default for RoomOptions {
    fn default() -> Self {
        Self { sig: None, no_stun: false }
    }
}

impl Room {
    /// Creates (creator=true) or joins a room. Returns quickly; connecting happens in
    /// the background.
    pub fn open(code: &str, name: &str, max: usize, creator: bool, opts: RoomOptions, log: LogFn) -> Result<Arc<Room>, String> {
        let c = normalize_code(code)?;
        let (key, topic) = room_secrets(&c);
        let aead = XChaCha20Poly1305::new((&key).into());
        let sock = UdpSocket::bind((Ipv4Addr::UNSPECIFIED, 0)).map_err(|e| e.to_string())?;
        sock.set_read_timeout(Some(Duration::from_millis(250))).ok();
        let sig: Arc<dyn Signaler> = opts.sig.unwrap_or_else(|| Arc::new(NtfySignal::new(topic, log.clone())));
        let id = loop {
            let v = rand::thread_rng().next_u64();
            if v != 0 {
                break v;
            }
        };
        let max = if (2..=MAX_ROOM_SIZE).contains(&max) { max } else { MAX_ROOM_SIZE };
        let (stun_tx, stun_rx) = sync_channel(8);
        let r = Arc::new(Room {
            code: format_code(&c),
            id,
            name: name.to_string(),
            creator,
            aead,
            sig,
            sock,
            stun_tx,
            stun_rx: Mutex::new(stun_rx),
            log,
            on_voice: RwLock::new(None),
            on_change: RwLock::new(None),
            inner: Mutex::new(Inner {
                max,
                joined: now_ms(),
                pub_addr: None,
                lan: None,
                peers: HashMap::new(),
                state: "starting".into(),
                last_ann: None,
                roles: RoleState::new(id, creator),
            }),
            stop: Arc::new(AtomicBool::new(false)),
            closed: AtomicBool::new(false),
            bytes_in: AtomicI64::new(0),
            bytes_out: AtomicI64::new(0),
            epoch: Instant::now(),
            blocked_ports: Mutex::new(Vec::new()),
            no_stun: opts.no_stun,
        });
        let a = r.clone();
        std::thread::spawn(move || a.read_loop());
        let a = r.clone();
        std::thread::spawn(move || a.run());
        Ok(r)
    }

    pub fn set_on_voice(&self, f: impl Fn(u64, &[u8]) + Send + Sync + 'static) {
        *self.on_voice.write().unwrap() = Some(Arc::new(f));
    }

    pub fn set_on_change(&self, f: impl Fn() + Send + Sync + 'static) {
        *self.on_change.write().unwrap() = Some(Arc::new(f));
    }

    pub(crate) fn logf(&self, s: String) {
        (self.log)(s)
    }

    fn set_state(&self, s: &str) {
        self.inner.lock().unwrap().state = s.to_string();
        self.changed();
    }

    pub(crate) fn changed(&self) {
        let cb = self.on_change.read().unwrap().clone();
        if let Some(cb) = cb {
            cb();
        }
    }

    pub fn state(&self) -> (String, usize) {
        let g = self.inner.lock().unwrap();
        (g.state.clone(), g.max)
    }

    pub fn local_port(&self) -> u16 {
        self.sock.local_addr().map(|a| a.port()).unwrap_or(0)
    }

    pub fn members(&self) -> Vec<MemberInfo> {
        let g = self.inner.lock().unwrap();
        let mut out = vec![MemberInfo {
            id: self.id,
            name: self.name.clone(),
            is_self: true,
            connected: true,
            host: g.roles.host_id == self.id,
            moderator: g.roles.mods.contains(&self.id),
            muted: g.roles.muted.contains(&self.id),
            ..Default::default()
        }];
        let mut ps: Vec<&Peer> = g.peers.values().filter(|p| !p.gone).collect();
        ps.sort_by_key(|p| (p.joined, p.id));
        for p in ps {
            let mut mi = MemberInfo {
                id: p.id,
                name: p.name.clone(),
                connected: direct(p),
                rtt: p.rtt,
                host: g.roles.host_id == p.id,
                moderator: g.roles.mods.contains(&p.id),
                muted: g.roles.muted.contains(&p.id),
                ..Default::default()
            };
            if let Some((rel, cost)) = best_relay(&g.peers, p.id) {
                if (!mi.connected || p.via == rel) && (!mi.connected || cost + RELAY_GAIN < p.rtt) {
                    mi.connected = true;
                    mi.rtt = cost;
                    mi.via = g.peers.get(&rel).map(|q| q.name.clone()).unwrap_or_default();
                }
            }
            out.push(mi);
        }
        out
    }

    pub fn traffic(&self) -> (i64, i64) {
        (self.bytes_in.load(Ordering::Relaxed), self.bytes_out.load(Ordering::Relaxed))
    }

    /// How many people are in the room, you included.
    pub fn size(&self) -> usize {
        1 + self.inner.lock().unwrap().peers.values().filter(|p| !p.gone).count()
    }

    pub fn is_closed(&self) -> bool {
        self.closed.load(Ordering::Relaxed)
    }

    // ---------- lifecycle ----------

    fn stun_discover(&self) -> Result<(std::net::SocketAddrV4, bool), String> {
        if self.no_stun {
            return Err("skipped".into());
        }
        let rx = self.stun_rx.lock().unwrap();
        while rx.try_recv().is_ok() {}
        stun::discover(
            &|b, a| {
                let _ = self.sock.send_to(b, a);
            },
            &rx,
        )
    }

    fn wait(&self, d: Duration) -> bool {
        let end = Instant::now() + d;
        while Instant::now() < end {
            if self.stop.load(Ordering::Relaxed) {
                return false;
            }
            std::thread::sleep(Duration::from_millis(50).min(end.saturating_duration_since(Instant::now())));
        }
        !self.stop.load(Ordering::Relaxed)
    }

    fn run(self: Arc<Self>) {
        // 1. find our addresses
        self.set_state("finding your internet address");
        let res = self.stun_discover();
        // tests / demo: everyone is on this PC
        let lan_ip = if self.no_stun { Some(Ipv4Addr::LOCALHOST) } else { local_ipv4() };
        {
            let mut g = self.inner.lock().unwrap();
            if let Ok((p, _)) = &res {
                g.pub_addr = Some(SocketAddr::V4(*p));
            }
            if let Some(ip) = lan_ip {
                g.lan = Some(SocketAddr::new(IpAddr::V4(ip), self.local_port()));
            }
        }
        match &res {
            Err(e) if !self.no_stun => {
                self.logf(format!("Couldn't find your internet address ({e}) - only people on your home network can connect."))
            }
            Ok((_, true)) => self.logf(
                "Your router changes ports for every connection (symmetric NAT); connecting to people outside your network may need someone else in the room to pass your voice along.".into(),
            ),
            _ => {}
        }

        // 2. listen on the relay (also replays recent announces from people already in the room)
        let a = self.clone();
        std::thread::spawn(move || {
            let me = a.clone();
            a.sig.subscribe(a.stop.clone(), &mut |m| me.on_signal(m));
        });
        let a = self.clone();
        std::thread::spawn(move || a.tick_loop());

        if !self.creator {
            self.set_state("looking for the room");
            if !self.wait(Duration::from_secs(if self.no_stun { 1 } else { 4 })) {
                return;
            }
            let (n, max) = {
                let g = self.inner.lock().unwrap();
                (g.peers.values().filter(|p| !p.gone).count(), g.max)
            };
            if n >= max {
                self.logf(format!("This room is full ({max} people max)."));
                self.set_state(&format!("room is full ({max} max)"));
                self.stop.store(true, Ordering::Relaxed);
                return;
            }
            if n == 0 {
                self.logf("Nobody is in this room right now - waiting. Check the code if this doesn't change.".into());
            }
        }
        self.announce();
        self.set_state("in room");
        self.keep_address();
    }

    /// Re-checks our public address every 25 s. The STUN traffic also keeps the router's
    /// port mapping open while nobody else is in the room, so the address we announced
    /// stays valid for people who join later.
    fn keep_address(&self) {
        if self.no_stun {
            return;
        }
        while self.wait(Duration::from_secs(25)) {
            let Ok((p, _)) = self.stun_discover() else { continue };
            let p = SocketAddr::V4(p);
            let changed = {
                let mut g = self.inner.lock().unwrap();
                let c = g.pub_addr != Some(p);
                g.pub_addr = Some(p);
                c
            };
            if changed {
                self.logf("Your internet address changed - telling the room.".into());
                self.announce();
            }
        }
    }

    pub fn leave(&self) {
        if self.closed.swap(true, Ordering::SeqCst) {
            return;
        }
        let b = self.seal_announce(&Announce { k: "b".into(), id: id_hex(self.id), t: now_ms(), ..Default::default() });
        self.broadcast(PK_BYE, &[]);
        let sig = self.sig.clone();
        let (tx, rx) = sync_channel(1);
        std::thread::spawn(move || {
            let _ = sig.publish(&b);
            let _ = tx.send(());
        });
        let _ = rx.recv_timeout(Duration::from_secs(2));
        self.stop.store(true, Ordering::SeqCst);
    }

    // ---------- signaling ----------

    fn seal_announce(&self, a: &Announce) -> Vec<u8> {
        let js = serde_json::to_vec(a).unwrap();
        let mut nonce = [0u8; NONCE];
        rand::thread_rng().fill_bytes(&mut nonce);
        let ct = self.aead.encrypt(XNonce::from_slice(&nonce), Payload { msg: &js, aad: b"announce" }).expect("seal");
        let mut raw = nonce.to_vec();
        raw.extend(ct);
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(raw).into_bytes()
    }

    fn open_announce(&self, msg: &[u8]) -> Option<Announce> {
        let s = std::str::from_utf8(msg).ok()?.trim();
        let raw = base64::engine::general_purpose::URL_SAFE_NO_PAD.decode(s).ok()?;
        if raw.len() < NONCE + TAG {
            return None;
        }
        let pt = self.aead.decrypt(XNonce::from_slice(&raw[..NONCE]), Payload { msg: &raw[NONCE..], aad: b"announce" }).ok()?;
        serde_json::from_slice(&pt).ok()
    }

    pub(crate) fn announce(&self) {
        if self.is_closed() {
            return;
        }
        let a = {
            let mut g = self.inner.lock().unwrap();
            g.last_ann = Some(Instant::now());
            Announce {
                k: "a".into(),
                id: id_hex(self.id),
                n: self.name.clone(),
                m: g.max,
                j: g.joined,
                t: now_ms(),
                pk: g.roles.pub_key_b64(),
                h: self.creator,
                p: g.pub_addr.map(|a| a.to_string()).unwrap_or_default(),
                l: g.lan.map(|a| a.to_string()).unwrap_or_default(),
            }
        };
        let msg = self.seal_announce(&a);
        let sig = self.sig.clone();
        let log = self.log.clone();
        std::thread::spawn(move || {
            if let Err(e) = sig.publish(&msg) {
                log(format!("Couldn't reach the room relay ({e})."));
            }
        });
    }

    fn on_signal(&self, msg: &[u8]) {
        let Some(a) = self.open_announce(msg) else { return };
        let Some(id) = parse_id(&a.id) else { return };
        if id == self.id || id == 0 || now_ms() - a.t > ANNOUNCE_FRESH_MS {
            return;
        }
        let mut g = self.inner.lock().unwrap();
        if a.k == "b" {
            if let Some(p) = g.peers.get_mut(&id) {
                if !p.gone && p.last_ann_ms <= a.t {
                    p.gone = true;
                    let name = p.name.clone();
                    drop(g);
                    self.logf(format!("{name} left the room."));
                    self.changed();
                }
            }
            return;
        }
        if a.k != "a" {
            return;
        }
        let is_new = g.peers.get(&id).map(|p| p.gone).unwrap_or(true);
        let my_joined = g.joined;
        let p = g.peers.entry(id).or_insert_with(|| Peer {
            id,
            name: String::new(),
            cands: vec![],
            addr: None,
            last_recv: None,
            last_ann_ms: 0,
            last_ann: None,
            joined: 0,
            rtt: Duration::ZERO,
            gone: false,
            links: HashMap::new(),
            links_at: None,
            last_relay: None,
            via: 0,
            key: None,
        });
        if p.last_ann_ms > a.t {
            return; // older than what we know
        }
        p.gone = false;
        p.name = clean_name(&a.n);
        p.joined = a.j;
        p.last_ann_ms = a.t;
        p.last_ann = Some(Instant::now());
        p.cands.clear();
        for s in [&a.p, &a.l] {
            if s.is_empty() {
                continue;
            }
            if let Some(u) = s.to_socket_addrs().ok().and_then(|mut i| i.find(|x| x.is_ipv4())) {
                if !p.cands.contains(&u) {
                    p.cands.push(u);
                }
            }
        }
        let name = p.name.clone();
        let joined = p.joined;
        g.roles.note_announced_key(id, &a);
        if let Some(k) = g.roles.keys.get(&id).cloned() {
            if let Some(p) = g.peers.get_mut(&id) {
                p.key = Some(k);
            }
        }
        // the room limit is whatever the earliest member says
        if a.m >= 2 && joined < my_joined && !self.creator {
            g.max = a.m.min(MAX_ROOM_SIZE);
        }
        let reannounce = is_new && since(g.last_ann) > Duration::from_secs(20) && g.state == "in room";
        drop(g);
        if is_new {
            self.logf(format!("{name} is in the room - connecting..."));
            self.changed();
        }
        if reannounce {
            self.announce(); // so the newcomer gets our current address
        }
    }

    // ---------- UDP ----------

    pub(crate) fn seal(&self, t: u8, payload: &[u8]) -> Vec<u8> {
        let mut pt = Vec::with_capacity(9 + payload.len());
        pt.push(t);
        pt.extend_from_slice(&self.id.to_be_bytes());
        pt.extend_from_slice(payload);
        let mut nonce = [0u8; NONCE];
        rand::thread_rng().fill_bytes(&mut nonce);
        let ct = self.aead.encrypt(XNonce::from_slice(&nonce), pt.as_slice()).expect("seal");
        let mut out = Vec::with_capacity(NONCE + ct.len());
        out.extend_from_slice(&nonce);
        out.extend(ct);
        out
    }

    fn send(&self, a: SocketAddr, pkt: &[u8]) {
        if !self.blocked_ports.lock().unwrap().is_empty() && self.blocked_ports.lock().unwrap().contains(&a.port()) {
            return;
        }
        if let Ok(n) = self.sock.send_to(pkt, a) {
            self.bytes_out.fetch_add(n as i64, Ordering::Relaxed);
        }
    }

    /// How to get a sealed packet to peer `id` (direct, through a relay, or not at all).
    fn route(&self, g: &mut Inner, id: u64, pkt: &[u8]) -> Option<(SocketAddr, Vec<u8>)> {
        let rel = best_relay(&g.peers, id);
        let p = g.peers.get(&id)?;
        let is_direct = direct(p);
        if let Some((rid, cost)) = rel {
            if !is_direct || (p.rtt > Duration::ZERO && cost + RELAY_GAIN < p.rtt) {
                let raddr = g.peers.get(&rid).and_then(|q| q.addr)?;
                let mut w = Vec::with_capacity(8 + pkt.len());
                w.extend_from_slice(&id.to_be_bytes());
                w.extend_from_slice(pkt);
                g.peers.get_mut(&id).unwrap().via = rid;
                return Some((raddr, self.seal(PK_RELAY, &w)));
            }
        }
        let p = g.peers.get_mut(&id)?;
        p.via = 0;
        if is_direct {
            return Some((p.addr?, pkt.to_vec()));
        }
        None
    }

    /// Sends one packet to every connected member.
    pub(crate) fn broadcast(&self, t: u8, payload: &[u8]) {
        self.broadcast_skip(t, payload, None)
    }

    fn broadcast_skip(&self, t: u8, payload: &[u8], skip: Option<&dyn Fn(u64) -> bool>) {
        let pkt = self.seal(t, payload);
        let ids: Vec<u64> = {
            let g = self.inner.lock().unwrap();
            g.peers.values().filter(|p| !p.gone).map(|p| p.id).collect()
        };
        // `skip` may take other locks (voice state): call it without ours held
        let ids: Vec<u64> = ids.into_iter().filter(|id| skip.map(|f| !f(*id)).unwrap_or(true)).collect();
        let jobs: Vec<(SocketAddr, Vec<u8>)> = {
            let mut g = self.inner.lock().unwrap();
            ids.iter().filter_map(|id| self.route(&mut g, *id, &pkt)).collect()
        };
        for (a, p) in jobs {
            self.send(a, &p);
        }
    }

    /// Sends a voice payload to everyone in the room.
    pub fn send_voice(&self, p: &[u8]) {
        self.send_voice_skip(p, None)
    }

    /// Sends to everyone except those `skip` says no to (too far away to hear).
    pub fn send_voice_skip(&self, p: &[u8], skip: Option<&dyn Fn(u64) -> bool>) {
        if !self.is_closed() {
            self.broadcast_skip(PK_VOICE, p, skip);
        }
    }

    fn read_loop(self: Arc<Self>) {
        let mut buf = vec![0u8; 65536];
        loop {
            if self.stop.load(Ordering::Relaxed) {
                return;
            }
            let Ok((n, from)) = self.sock.recv_from(&mut buf) else { continue };
            let b = &buf[..n];
            if stun::is_stun(b) {
                let _ = self.stun_tx.try_send(b.to_vec());
                continue;
            }
            if n < NONCE + 9 + TAG {
                continue;
            }
            self.bytes_in.fetch_add(n as i64, Ordering::Relaxed);
            self.handle(b, from, 0);
        }
    }

    /// Handles one sealed packet. `via` is the member who relayed it (0 = it came directly).
    fn handle(&self, b: &[u8], from: SocketAddr, via: u64) {
        if b.len() < NONCE + 9 + TAG {
            return;
        }
        let Ok(pt) = self.aead.decrypt(XNonce::from_slice(&b[..NONCE]), &b[NONCE..]) else { return };
        if pt.len() < 9 {
            return;
        }
        let t = pt[0];
        let id = u64::from_be_bytes(pt[1..9].try_into().unwrap());
        let payload = &pt[9..];
        if id == self.id {
            return;
        }
        if via != 0 && t != PK_VOICE && t != PK_BYE && t != PK_CTRL {
            return; // only voice (and goodbyes) travel through relays
        }
        let (first, was_gone, name, muted) = {
            let mut g = self.inner.lock().unwrap();
            let muted = g.roles.muted.contains(&id);
            let Some(p) = g.peers.get_mut(&id) else { return }; // not announced in this room (yet)
            if p.gone && t != PK_BYE && since(p.last_ann) > PEER_TIMEOUT {
                return;
            }
            let was_gone = p.gone;
            let mut first = false;
            if t == PK_BYE {
                p.gone = true;
            } else if via != 0 {
                first = was_gone || (since(p.last_relay) > Duration::from_secs(15) && !direct(p));
                p.gone = false;
                p.last_relay = Some(Instant::now());
            } else {
                first = p.addr.is_none() || since(p.last_recv) > Duration::from_secs(15);
                p.gone = false;
                p.addr = Some(from);
                p.last_recv = Some(Instant::now());
            }
            (first, was_gone, p.name.clone(), muted)
        };
        match t {
            PK_BYE => {
                if !was_gone {
                    self.logf(format!("{name} left the room."));
                    self.changed();
                }
                return;
            }
            PK_PING => self.send(from, &self.seal(PK_PONG, payload)),
            PK_PONG => {
                if payload.len() == 8 {
                    let sent = u64::from_be_bytes(payload.try_into().unwrap());
                    let now = self.epoch.elapsed().as_nanos() as u64;
                    if now >= sent {
                        if let Some(p) = self.inner.lock().unwrap().peers.get_mut(&id) {
                            p.rtt = Duration::from_nanos(now - sent).max(Duration::from_micros(1));
                        }
                    }
                }
            }
            PK_VOICE => {
                // muted for everyone: dropped here too
                if !muted {
                    let cb = self.on_voice.read().unwrap().clone();
                    if let Some(cb) = cb {
                        cb(id, payload);
                    }
                }
            }
            PK_CTRL => {
                if self.apply_doc(payload) {
                    self.changed();
                }
            }
            PK_LINKS => {
                let mut m = HashMap::new();
                for c in payload.chunks_exact(10) {
                    m.insert(u64::from_be_bytes(c[..8].try_into().unwrap()), Duration::from_millis(u16::from_be_bytes([c[8], c[9]]) as u64));
                }
                if let Some(p) = self.inner.lock().unwrap().peers.get_mut(&id) {
                    p.links = m;
                    p.links_at = Some(Instant::now());
                }
            }
            PK_RELAY => {
                // pass a sealed packet on to someone we reach directly
                if payload.len() > 8 {
                    let target = u64::from_be_bytes(payload[..8].try_into().unwrap());
                    let to = {
                        let g = self.inner.lock().unwrap();
                        g.peers.get(&target).filter(|q| !q.gone && q.id != id && direct(q)).and_then(|q| q.addr)
                    };
                    if let Some(to) = to {
                        self.send(to, &self.seal(PK_RELAYED, &payload[8..]));
                    }
                }
            }
            PK_RELAYED => {
                if via == 0 {
                    self.handle(payload, from, id);
                }
            }
            _ => {}
        }
        if first {
            if via != 0 {
                self.logf(format!("Connected to {name} (through {}).", self.name_of(via)));
            } else {
                self.logf(format!("Connected to {name}."));
            }
            self.changed();
        }
    }

    pub fn name_of(&self, id: u64) -> String {
        if id == self.id {
            return self.name.clone();
        }
        self.inner.lock().unwrap().peers.get(&id).map(|p| p.name.clone()).unwrap_or_else(|| "someone".into())
    }

    fn tick_loop(self: Arc<Self>) {
        let mut n = 0u64;
        while self.wait(Duration::from_millis(200)) {
            n += 1;
            let punch = self.seal(PK_PUNCH, &[]);
            let ping = (n % 5 == 0).then(|| self.seal(PK_PING, &(self.epoch.elapsed().as_nanos() as u64).to_be_bytes()));
            let mut jobs: Vec<(SocketAddr, Vec<u8>)> = Vec::new();
            let mut changed = false;
            let mut lost = Vec::new();
            let (docs, reann) = {
                let mut g = self.inner.lock().unwrap();
                let ids: Vec<u64> = g.peers.keys().copied().collect();
                for id in ids {
                    let relay = best_relay(&g.peers, id).is_some();
                    let p = g.peers.get_mut(&id).unwrap();
                    if p.gone {
                        if now_ms() - p.last_ann_ms > ANNOUNCE_FRESH_MS && since(p.last_recv) > Duration::from_secs(360) {
                            g.peers.remove(&id);
                        }
                        continue;
                    }
                    let alive = p.addr.is_some() && since(p.last_recv) < Duration::from_secs(8);
                    if !alive {
                        for c in &p.cands {
                            jobs.push((*c, punch.clone()));
                        }
                        if let Some(a) = p.addr {
                            if !p.cands.contains(&a) {
                                jobs.push((a, punch.clone()));
                            }
                        }
                        // nobody answering, not reachable through anyone and no recent announce: they've gone
                        if !relay && since(p.last_recv) > PEER_TIMEOUT && since(p.last_relay) > PEER_TIMEOUT && since(p.last_ann) > PEER_TIMEOUT {
                            p.gone = true;
                            changed = true;
                            lost.push(p.name.clone());
                        }
                    } else if let Some(ping) = &ping {
                        jobs.push((p.addr.unwrap(), ping.clone()));
                    }
                }
                if n % 5 == 0 {
                    // once a second: tell everyone who we reach directly, so they can route through us
                    let mut lb = Vec::new();
                    let mut to = Vec::new();
                    for p in g.peers.values().filter(|p| !p.gone && direct(p)) {
                        lb.extend_from_slice(&p.id.to_be_bytes());
                        let ms = p.rtt.as_millis();
                        let ms: u16 = if ms == 0 || ms > 65535 { 500 } else { ms as u16 };
                        lb.extend_from_slice(&ms.to_be_bytes());
                        to.push(p.addr.unwrap());
                    }
                    if !to.is_empty() {
                        let pkt = self.seal(PK_LINKS, &lb);
                        for a in to {
                            jobs.push((a, pkt.clone()));
                        }
                    }
                }
                let docs = g.roles.share();
                let reann = g.state == "in room" && since(g.last_ann) > ANNOUNCE_EVERY;
                (docs, reann)
            };
            for d in docs {
                self.broadcast(PK_CTRL, &d);
            }
            for (a, p) in jobs {
                self.send(a, &p);
            }
            for name in lost {
                self.logf(format!("Lost connection to {name}."));
            }
            if changed {
                self.changed();
            }
            if reann {
                self.announce();
            }
        }
    }
}

fn direct(p: &Peer) -> bool {
    p.addr.is_some() && since(p.last_recv) < Duration::from_secs(10)
}

/// The directly connected member with the quickest path to `id`.
fn best_relay(peers: &HashMap<u64, Peer>, id: u64) -> Option<(u64, Duration)> {
    let mut best: Option<(u64, Duration)> = None;
    for q in peers.values() {
        if q.id == id || q.gone || !direct(q) || since(q.links_at) > LINKS_FRESH {
            continue;
        }
        let Some(d) = q.links.get(&id) else { continue };
        let rtt = if q.rtt.is_zero() { Duration::from_millis(100) } else { q.rtt };
        let c = rtt + *d;
        if best.map(|(_, b)| c < b).unwrap_or(true) {
            best = Some((q.id, c));
        }
    }
    best
}

fn clean_name(s: &str) -> String {
    let t: String = s.chars().filter(|c| !c.is_control()).take(32).collect();
    let t = t.trim();
    if t.is_empty() {
        "Driver".into()
    } else {
        t.to_string()
    }
}

fn local_ipv4() -> Option<Ipv4Addr> {
    let s = UdpSocket::bind((Ipv4Addr::UNSPECIFIED, 0)).ok()?;
    s.connect((Ipv4Addr::new(8, 8, 8, 8), 53)).ok()?;
    match s.local_addr().ok()?.ip() {
        IpAddr::V4(v) if !v.is_unspecified() => Some(v),
        _ => None,
    }
}
