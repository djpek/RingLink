//! Whole-app tests: several RingLinks in one process, talking over real UDP sockets on
//! this PC, with an in-process relay and fake audio devices.

use crate::app::{App, AppOptions};
use crate::motion::{V3, GAME_LMU};
use crate::roles::RoomRules;
use crate::signal::MemHub;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::{Duration, Instant};

fn mk(hub: &Arc<MemHub>, name: &str) -> Arc<App> {
    std::env::set_var("RINGLINK_FAKE_AUDIO", "1");
    std::env::set_var("RINGLINK_NO_XKEYS", "1");
    let dir = std::env::temp_dir().join(format!("rl-e2e-{}-{}-{name}", std::process::id(), rand::random::<u32>()));
    let a = App::new(Arc::new(|| {}), AppOptions { hub: Some(hub.clone()), no_stun: true, config_dir: Some(dir) });
    a.settings.lock().unwrap().display_name = name.to_string();
    a.settings.lock().unwrap().max_people = 3;
    a
}

fn wait(what: &str, secs: u64, mut f: impl FnMut() -> bool) {
    let end = Instant::now() + Duration::from_secs(secs);
    while Instant::now() < end {
        if f() {
            return;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    panic!("timed out waiting for: {what}");
}

fn connected(a: &Arc<App>, n: usize) -> bool {
    a.room().map(|r| r.members().iter().filter(|m| m.connected).count() == n).unwrap_or(false)
}

#[test]
fn three_people_roles_relay_and_voice() {
    let hub = Arc::new(MemHub::default());
    let (a, b, c) = (mk(&hub, "Alex"), mk(&hub, "Sam"), mk(&hub, "Jordan"));
    a.create_room().unwrap();
    let code = a.room().unwrap().code.clone();
    assert!(b.join_room("K7QM-29X!").is_err());
    b.join_room(&code.to_lowercase()).unwrap();
    c.join_room(&code).unwrap();
    wait("everyone connected", 20, || connected(&a, 3) && connected(&b, 3) && connected(&c, 3));
    let (ra, rb, rc) = (a.room().unwrap(), b.room().unwrap(), c.room().unwrap());
    assert!(ra.am_host() && !rb.am_host());
    wait("host known to all", 5, || rb.host_id() == ra.id && rc.host_id() == ra.id);
    assert_eq!(rb.state().1, 3, "joiners take the room size from the creator");

    // a fourth person finds the room full
    let d = mk(&hub, "Late");
    d.join_room(&code).unwrap();
    wait("room full", 10, || d.room().map(|r| r.state().0.contains("full")).unwrap_or(false));
    d.shutdown();

    // roles: guests can't moderate; the host makes Sam a mod; Sam mutes Jordan
    assert!(rb.set_muted(rc.id, true).is_err());
    assert!(rb.set_mod(rb.id, true).is_err());
    assert!(ra.set_muted(ra.id, true).is_err(), "the host can't be muted");
    ra.set_mod(rb.id, true).unwrap();
    wait("Sam is a mod everywhere", 5, || rb.can_moderate() && rc.members().iter().any(|m| m.id == rb.id && m.moderator));
    rb.set_muted(rc.id, true).unwrap();
    wait("Jordan is muted", 5, || c.voice.host_mute.load(Ordering::Relaxed) && ra.muted_by_host(rc.id));
    rb.set_muted(rc.id, false).unwrap();
    wait("Jordan is unmuted", 5, || !c.voice.host_mute.load(Ordering::Relaxed));
    // room rules reach everyone, and a removed sub-room is left
    c.voice.set_sub("Blue");
    ra.set_rules(RoomRules { pit_radio: false, isolate: rb.id, subs: vec!["Blue".into()] }).unwrap();
    wait("rules everywhere", 5, || rc.rules().isolate == rb.id && !rc.rules().pit_radio && rb.rules().subs == vec!["Blue".to_string()]);
    ra.set_rules(RoomRules::default()).unwrap();
    wait("sub-room removed", 5, || c.voice.sub().is_empty());

    // voice: the fake mics talk 1.6 s of every 4 s, so everyone hears everyone
    wait("voice running", 10, || a.voice.running() && b.voice.running() && c.voice.running());
    wait("Alex hears both", 10, || {
        let st = a.voice.status();
        st.peers.get(&rb.id).map(|p| p.talking).unwrap_or(false) && st.peers.get(&rc.id).map(|p| p.talking).unwrap_or(false)
    });

    // proximity: Sam is 30 m ahead of Alex, Jordan 140 m behind
    a.voice.set_pos_source(Some(Arc::new(|| Some((V3::new(0.0, 0.0, 0.0), GAME_LMU)))));
    b.voice.set_pos_source(Some(Arc::new(|| Some((V3::new(0.0, 0.0, 30.0), GAME_LMU)))));
    c.voice.set_pos_source(Some(Arc::new(|| Some((V3::new(0.0, 0.0, -140.0), GAME_LMU)))));
    wait("distances", 5, || {
        let st = a.voice.status();
        let (s, j) = (st.peers.get(&rb.id), st.peers.get(&rc.id));
        matches!((s, j), (Some(s), Some(j)) if (s.dist - 30.0).abs() < 1.0 && (j.dist - 140.0).abs() < 1.0 && s.volume > 60 && j.volume < 5)
    });
    // broadcasting overrides distance
    c.voice.bcast_btn.store(true, Ordering::Relaxed);
    c.voice.kick_pos();
    wait("broadcast heard at full volume", 5, || a.voice.status().peers.get(&rc.id).map(|p| p.volume == 100 && p.broadcasting).unwrap_or(false));
    c.voice.bcast_btn.store(false, Ordering::Relaxed);

    // relay: Alex and Jordan can't reach each other directly any more; Sam passes it on
    ra.blocked_ports.lock().unwrap().push(rc.local_port());
    rc.blocked_ports.lock().unwrap().push(ra.local_port());
    wait("routed through Sam", 25, || ra.members().iter().any(|m| m.id == rc.id && m.connected && m.via == "Sam"));
    let before = a.voice.status().peers.get(&rc.id).map(|p| p.talking).unwrap_or(false);
    let _ = before;
    wait("still hears Jordan through Sam", 12, || {
        std::thread::sleep(Duration::from_millis(450));
        a.voice.status().peers.get(&rc.id).map(|p| p.talking).unwrap_or(false)
    });

    // leaving is noticed
    c.shutdown();
    wait("Jordan gone", 10, || connected(&a, 2) && connected(&b, 2));
    let (rx, tx) = ra.traffic();
    assert!(rx > 10_000 && tx > 10_000);
    assert!(a.logs().iter().any(|l| l.contains("Jordan left")));
    a.shutdown();
    b.shutdown();
}

#[test]
fn settings_survive_a_restart() {
    let hub = Arc::new(MemHub::default());
    let a = mk(&hub, "Alex");
    let dir = std::env::temp_dir().join(format!("rl-e2e-settings-{}", rand::random::<u32>()));
    let a2 = App::new(Arc::new(|| {}), AppOptions { hub: Some(hub.clone()), no_stun: true, config_dir: Some(dir.clone()) });
    a2.voice.range_m.store(300, Ordering::Relaxed);
    a2.voice.denoise.store(false, Ordering::Relaxed);
    a2.set_peer_volume(1, "Sam", 140);
    a2.settings.lock().unwrap().display_name = "djvryt-desktop".into();
    a2.save();
    a2.shutdown();
    // a settings file from RingLink 4 loads too (unknown / missing fields are fine)
    let b = App::new(Arc::new(|| {}), AppOptions { hub: Some(hub.clone()), no_stun: true, config_dir: Some(dir.clone()) });
    assert_eq!(b.voice.range_m.load(Ordering::Relaxed), 300);
    assert!(!b.voice.denoise.load(Ordering::Relaxed));
    assert_eq!(b.settings.lock().unwrap().peer_volume["Sam"], 140);
    assert_eq!(b.settings.lock().unwrap().display_name, "djvryt-desktop");
    b.shutdown();
    std::fs::write(dir.join("settings.json"), r#"{"displayName":"Old","maxPeople":6,"game":1,"voiceVol":180,"broadcastBind":"joy0:b18","peerVolume":{"Sam":50},"someOldField":1}"#).unwrap();
    let c = App::new(Arc::new(|| {}), AppOptions { hub: Some(hub), no_stun: true, config_dir: Some(dir) });
    assert_eq!(c.voice.vol_pct.load(Ordering::Relaxed), 180);
    assert_eq!(c.bind.bind("broadcast"), "joy0:b18");
    assert!(c.voice.denoise.load(Ordering::Relaxed), "new settings get their defaults");
    assert_eq!(c.settings.lock().unwrap().max_people, 6);
    c.shutdown();
    a.shutdown();
}
