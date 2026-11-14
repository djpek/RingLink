//! `--demo`: a room on this PC with a few pretend drivers circling a track, for trying
//! the app (and taking screenshots) without a game, a microphone or other people.

use crate::app::{App, AppOptions};
use crate::motion::{V3, GAME_LMU};
use crate::signal::MemHub;
use std::sync::Arc;
use std::time::{Duration, Instant};

pub struct Demo {
    pub hub: Arc<MemHub>,
    bots: Vec<Arc<App>>,
}

/// A point on a 400 m x 240 m oval, `lap_s` seconds a lap.
fn oval(t0: Instant, lap_s: f64, phase: f64, lane: f64) -> V3 {
    let a = (t0.elapsed().as_secs_f64() / lap_s + phase) * std::f64::consts::TAU;
    V3::new((200.0 + lane) * a.cos(), 0.0, (120.0 + lane) * a.sin())
}

impl Demo {
    pub fn options(dir: std::path::PathBuf) -> (Arc<MemHub>, AppOptions) {
        std::env::set_var("RINGLINK_FAKE_AUDIO", "1");
        let hub = Arc::new(MemHub::default());
        (hub.clone(), AppOptions { hub: Some(hub), no_stun: true, config_dir: Some(dir) })
    }

    /// Opens a room as `me` and brings the pretend drivers in.
    pub fn start(hub: Arc<MemHub>, me: &Arc<App>) -> Demo {
        let t0 = Instant::now();
        me.voice.set_pos_source(Some(Arc::new(move || Some((oval(t0, 40.0, 0.0, 0.0), GAME_LMU)))));
        let _ = me.create_room();
        let code = me.room().map(|r| r.code.clone()).unwrap_or_default();
        let mut bots = Vec::new();
        for (i, (name, lap, phase, lane)) in [("Sam", 40.0, 0.02, 4.0), ("Jordan", 43.0, 0.10, -3.0), ("Riley", 38.0, 0.55, 2.0)].into_iter().enumerate() {
            let dir = std::env::temp_dir().join(format!("ringlink-demo-{}-{i}", std::process::id()));
            let bot = App::new(Arc::new(|| {}), AppOptions { hub: Some(hub.clone()), no_stun: true, config_dir: Some(dir) });
            {
                let mut s = bot.settings.lock().unwrap();
                s.display_name = name.to_string();
                s.replay_voice = false;
            }
            bot.voice.replay.set_enabled(false);
            bot.voice.set_pos_source(Some(Arc::new(move || Some((oval(t0, lap, phase, lane), GAME_LMU)))));
            let (b, c) = (bot.clone(), code.clone());
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(600 + 500 * i as u64));
                let _ = b.join_room(&c);
            });
            bots.push(bot);
        }
        Demo { hub, bots }
    }

    pub fn stop(&self) {
        for b in &self.bots {
            b.shutdown();
        }
    }
}
