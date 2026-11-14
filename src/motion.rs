//! Follows one car from its position samples: which way it's really going (in world
//! coordinates, unlike LMU's carVelocity, which can be relative to the car) and where it
//! most likely is right now. LMU only updates positions 5 times a second, so between
//! samples the position is moved forward along the car's velocity.

use std::time::{Duration, Instant};

#[derive(Clone, Copy, Debug, PartialEq, Default)]
pub struct V3 {
    pub x: f64,
    pub y: f64,
    pub z: f64,
}

impl V3 {
    pub fn new(x: f64, y: f64, z: f64) -> Self {
        Self { x, y, z }
    }
    pub fn dist(&self, o: &V3) -> f64 {
        ((self.x - o.x).powi(2) + (self.y - o.y).powi(2) + (self.z - o.z).powi(2)).sqrt()
    }
}

/// which game a position came from: distances only make sense within one game
pub const GAME_NONE: u8 = 0;
pub const GAME_LMU: u8 = 1;
pub const GAME_BEAMNG: u8 = 2;

pub fn game_name(g: u8) -> &'static str {
    match g {
        GAME_LMU => "Le Mans Ultimate",
        GAME_BEAMNG => "BeamNG",
        _ => "",
    }
}

/// The two horizontal axes of a game's world (LMU is y-up, BeamNG is z-up).
pub fn horiz(p: &V3, game: u8) -> [f64; 2] {
    if game == GAME_BEAMNG {
        [p.x, p.y]
    } else {
        [p.x, p.z]
    }
}

/// never guess further ahead than this
const MAX_LEAD: Duration = Duration::from_millis(400);

#[derive(Clone, Copy, Default)]
pub struct Motion {
    pos: Option<V3>,
    at: Option<Instant>,
    vel: V3,
    vel_ok: bool,
    game: u8,
}

impl Motion {
    pub fn reset(&mut self) {
        *self = Motion::default();
    }

    /// Update with a sample seen at `now`. Repeats of the same position (polled faster
    /// than the game updates it) are ignored so the velocity isn't dragged toward zero.
    pub fn update(&mut self, p: Option<V3>, game: u8, now: Instant) {
        let Some(p) = p else {
            self.reset();
            return;
        };
        let (Some(old), Some(at)) = (self.pos, self.at) else {
            *self = Motion { pos: Some(p), at: Some(now), game, ..Default::default() };
            return;
        };
        if game != self.game {
            *self = Motion { pos: Some(p), at: Some(now), game, ..Default::default() };
            return;
        }
        if p == old {
            return;
        }
        let dt = now.saturating_duration_since(at).as_secs_f64();
        if dt <= 0.03 {
            // burst: restart from here
            self.vel_ok = false;
        } else {
            let nv = V3::new((p.x - old.x) / dt, (p.y - old.y) / dt, (p.z - old.z) / dt);
            let sp = (nv.x * nv.x + nv.y * nv.y + nv.z * nv.z).sqrt();
            if dt > 1.5 || sp > 150.0 {
                // long gap / teleport
                self.vel_ok = false;
                self.vel = V3::default();
            } else if !self.vel_ok {
                self.vel = nv;
                self.vel_ok = true;
            } else {
                self.vel = V3::new(0.6 * nv.x + 0.4 * self.vel.x, 0.6 * nv.y + 0.4 * self.vel.y, 0.6 * nv.z + 0.4 * self.vel.z);
            }
        }
        self.pos = Some(p);
        self.at = Some(now);
    }

    /// Best guess of the position at time `t`.
    pub fn now(&self, t: Instant) -> Option<V3> {
        let p = self.pos?;
        if !self.vel_ok {
            return Some(p);
        }
        let s = t.saturating_duration_since(self.at?).min(MAX_LEAD).as_secs_f64();
        Some(V3::new(p.x + self.vel.x * s, p.y + self.vel.y * s, p.z + self.vel.z * s))
    }

    /// Speed in m/s (0 when unknown).
    pub fn speed(&self) -> f64 {
        if !self.vel_ok {
            return 0.0;
        }
        (self.vel.x * self.vel.x + self.vel.y * self.vel.y + self.vel.z * self.vel.z).sqrt()
    }

    /// Unit direction of travel in the horizontal plane, once moving.
    pub fn heading(&self) -> Option<[f64; 2]> {
        if !self.vel_ok || self.pos.is_none() {
            return None;
        }
        let h = horiz(&self.vel, self.game);
        let n = h[0].hypot(h[1]);
        if n < 2.0 {
            return None;
        }
        Some([h[0] / n, h[1] / n])
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn extrapolates_along_velocity() {
        let t0 = Instant::now();
        let mut m = Motion::default();
        m.update(Some(V3::new(0.0, 0.0, 0.0)), GAME_LMU, t0);
        m.update(Some(V3::new(10.0, 0.0, 0.0)), GAME_LMU, t0 + Duration::from_millis(200));
        // 50 m/s along x; 100 ms later we expect ~15 m
        let p = m.now(t0 + Duration::from_millis(300)).unwrap();
        assert!((p.x - 15.0).abs() < 0.01, "{p:?}");
        assert_eq!(m.heading(), Some([1.0, 0.0]));
        // never further than MAX_LEAD ahead
        let p = m.now(t0 + Duration::from_secs(5)).unwrap();
        assert!((p.x - 30.0).abs() < 0.01);
        // repeated sample doesn't slow it down
        m.update(Some(V3::new(10.0, 0.0, 0.0)), GAME_LMU, t0 + Duration::from_millis(300));
        assert!((m.speed() - 50.0).abs() < 0.01);
        // teleport resets the velocity
        m.update(Some(V3::new(5000.0, 0.0, 0.0)), GAME_LMU, t0 + Duration::from_millis(500));
        assert_eq!(m.heading(), None);
        assert_eq!(m.now(t0 + Duration::from_millis(900)).unwrap().x, 5000.0);
    }
}
