//! Where your car is, from the games.
//!
//! Le Mans Ultimate: the game's local REST API (http://127.0.0.1:6397/rest/watch/standings,
//! the entry with player=true). Nothing to set up.
//!
//! BeamNG.drive: the game's "Motion Simulator" UDP output (Options > Other > MotionSim,
//! IP 127.0.0.1, port 4444). Packets are "BNG1" then little-endian float32s, the first
//! three being the vehicle's world position. In BeamMP everyone on the same map shares
//! one coordinate system, so distances between players are real distances.

use crate::motion::V3;
use serde::Deserialize;
use std::net::UdpSocket;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

pub const BEAMNG_PORT: u16 = 4444;
pub const LMU_REST: &str = "http://127.0.0.1:6397";

#[derive(Deserialize)]
struct LmuVec {
    x: f64,
    y: f64,
    z: f64,
}

#[derive(Deserialize)]
struct LmuStanding {
    #[serde(default)]
    player: bool,
    #[serde(default, rename = "hasFocus")]
    has_focus: bool,
    #[serde(default, rename = "inGarageStall")]
    in_garage_stall: bool,
    #[serde(default, rename = "carPosition")]
    car_position: Option<LmuVec>,
    #[serde(default, rename = "driverName")]
    driver_name: Option<String>,
}

/// What one look at LMU's standings tells us.
#[derive(Default, Debug, Clone)]
pub struct LmuState {
    pub ok: bool,
    /// your car
    pub pos: Option<V3>,
    pub in_garage: bool,
    /// the car the camera follows (the replay's car while watching one)
    pub focus: Option<V3>,
    /// everyone in the session (tells sessions at the same track apart)
    pub drivers: Vec<String>,
}

pub fn parse_lmu_standings(json: &str) -> LmuState {
    let mut out = LmuState::default();
    let Ok(st) = serde_json::from_str::<Vec<LmuStanding>>(json) else { return out };
    out.ok = true;
    let conv = |p: &Option<LmuVec>| p.as_ref().filter(|p| !(p.x == 0.0 && p.y == 0.0 && p.z == 0.0)).map(|p| V3::new(p.x, p.y, p.z));
    for s in &st {
        if let Some(n) = s.driver_name.as_ref().map(|n| n.trim()).filter(|n| !n.is_empty()) {
            out.drivers.push(n.to_string());
        }
        if s.player {
            out.pos = conv(&s.car_position);
            out.in_garage = s.in_garage_stall;
        }
        if s.has_focus {
            out.focus = conv(&s.car_position);
        }
    }
    out
}

pub fn lmu_agent() -> ureq::Agent {
    ureq::AgentBuilder::new().timeout(Duration::from_millis(400)).build()
}

pub fn read_lmu(agent: &ureq::Agent) -> LmuState {
    match agent.get(&format!("{LMU_REST}/rest/watch/standings")).call().ok().and_then(|r| r.into_string().ok()) {
        Some(s) => parse_lmu_standings(&s),
        None => LmuState::default(),
    }
}

pub fn parse_beamng(pkt: &[u8]) -> Option<V3> {
    if pkt.len() < 16 || &pkt[..4] != b"BNG1" {
        return None;
    }
    let f = |o: usize| f32::from_le_bytes(pkt[o..o + 4].try_into().unwrap()) as f64;
    let p = V3::new(f(4), f(8), f(12));
    if p.x.is_nan() || p.y.is_nan() || p.z.is_nan() {
        return None;
    }
    Some(p)
}

#[derive(Default)]
pub struct BeamNgListener {
    last: Mutex<Option<(V3, Instant)>>,
}

impl BeamNgListener {
    /// Listens until `stop` is set. If the port is taken (another telemetry app),
    /// BeamNG proximity just stays off and `Err` says why.
    pub fn start(stop: Arc<AtomicBool>) -> Result<Arc<BeamNgListener>, String> {
        let sock = UdpSocket::bind(("127.0.0.1", BEAMNG_PORT)).map_err(|e| e.to_string())?;
        sock.set_read_timeout(Some(Duration::from_millis(300))).ok();
        let l = Arc::new(BeamNgListener::default());
        let l2 = l.clone();
        std::thread::spawn(move || {
            let mut buf = [0u8; 256];
            while !stop.load(Ordering::Relaxed) {
                if let Ok((n, _)) = sock.recv_from(&mut buf) {
                    if let Some(p) = parse_beamng(&buf[..n]) {
                        *l2.last.lock().unwrap() = Some((p, Instant::now()));
                    }
                }
            }
        });
        Ok(l)
    }

    pub fn recent(&self, d: Duration) -> Option<V3> {
        self.last.lock().unwrap().filter(|(_, t)| t.elapsed() <= d).map(|(p, _)| p)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn beamng_packet() {
        let mut p = b"BNG1".to_vec();
        for v in [12.5f32, -3.0, 100.25] {
            p.extend_from_slice(&v.to_le_bytes());
        }
        p.resize(88, 0);
        assert_eq!(parse_beamng(&p), Some(V3::new(12.5, -3.0, 100.25)));
        assert_eq!(parse_beamng(b"XXXX000000000000"), None);
        assert_eq!(parse_beamng(b"BNG1"), None);
    }

    #[test]
    fn lmu_standings() {
        let js = r#"[
          {"player":false,"hasFocus":true,"carPosition":{"x":1,"y":2,"z":3},"driverName":"Other"},
          {"player":true,"inGarageStall":true,"carPosition":{"x":10.5,"y":0,"z":-4},"carVelocity":{"x":0,"y":0,"z":0},"driverName":"Me","extra":7}
        ]"#;
        let s = parse_lmu_standings(js);
        assert!(s.ok && s.in_garage);
        assert_eq!(s.pos, Some(V3::new(10.5, 0.0, -4.0)));
        assert_eq!(s.focus, Some(V3::new(1.0, 2.0, 3.0)));
        assert_eq!(s.drivers, vec!["Other".to_string(), "Me".to_string()]);
        // all-zero position = not on track
        let s = parse_lmu_standings(r#"[{"player":true,"carPosition":{"x":0,"y":0,"z":0}}]"#);
        assert!(s.ok && s.pos.is_none());
        assert!(!parse_lmu_standings("not json").ok);
    }
}
