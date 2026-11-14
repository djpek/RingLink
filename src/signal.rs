//! Signaling: how members of a room find each other's network addresses.
//!
//! Each room code maps to a topic on ntfy.sh (a free public pub/sub relay). Members post
//! small encrypted "announce" messages there and listen for everyone else's. ntfy only
//! ever sees ciphertext: the key comes from the room code, which never leaves the app.
//! After that, voice goes directly between the members' PCs over UDP.

use std::collections::HashMap;
use std::io::{BufRead, BufReader};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{channel, Sender};
use std::sync::{Arc, Mutex};
use std::time::Duration;

pub type LogFn = Arc<dyn Fn(String) + Send + Sync>;

pub trait Signaler: Send + Sync {
    fn publish(&self, msg: &[u8]) -> Result<(), String>;
    /// Calls `f` for every message on the topic, including recent ones from before we
    /// joined, until `stop` is set.
    fn subscribe(&self, stop: Arc<AtomicBool>, f: &mut dyn FnMut(&[u8]));
}

pub const NTFY_BASE: &str = "https://ntfy.sh";

pub struct NtfySignal {
    topic: String,
    log: LogFn,
}

impl NtfySignal {
    pub fn new(topic: String, log: LogFn) -> Self {
        Self { topic, log }
    }
}

fn sleep_unless(stop: &AtomicBool, d: Duration) {
    let step = Duration::from_millis(100);
    let mut left = d;
    while !left.is_zero() && !stop.load(Ordering::Relaxed) {
        let s = left.min(step);
        std::thread::sleep(s);
        left -= s;
    }
}

impl Signaler for NtfySignal {
    fn publish(&self, msg: &[u8]) -> Result<(), String> {
        let agent = ureq::AgentBuilder::new().timeout(Duration::from_secs(15)).build();
        let mut err = String::new();
        for attempt in 0..3u64 {
            match agent.post(&format!("{NTFY_BASE}/{}", self.topic)).set("X-Title", "ringlink").send_bytes(msg) {
                Ok(r) if r.status() == 200 => return Ok(()),
                Ok(r) => err = format!("relay answered {}", r.status()),
                Err(e) => err = e.to_string(),
            }
            std::thread::sleep(Duration::from_secs(2 + 3 * attempt));
        }
        Err(err)
    }

    fn subscribe(&self, stop: Arc<AtomicBool>, f: &mut dyn FnMut(&[u8])) {
        // long-lived stream; ntfy sends a keepalive every ~45 s, so a 90 s read timeout
        // only fires when the connection is really dead (and lets us notice `stop`)
        let agent = ureq::AgentBuilder::new().timeout_connect(Duration::from_secs(15)).timeout_read(Duration::from_secs(90)).build();
        let mut since = "15m".to_string();
        let mut backoff = Duration::from_secs(2);
        while !stop.load(Ordering::Relaxed) {
            let url = format!("{NTFY_BASE}/{}/json?since={since}", self.topic);
            let err = match agent.get(&url).call() {
                Ok(r) if r.status() == 200 => {
                    backoff = Duration::from_secs(2);
                    let rd = BufReader::new(r.into_reader());
                    let mut e = None;
                    for line in rd.lines() {
                        if stop.load(Ordering::Relaxed) {
                            return;
                        }
                        let line = match line {
                            Ok(l) => l,
                            Err(x) => {
                                e = Some(x.to_string());
                                break;
                            }
                        };
                        let Ok(v) = serde_json::from_str::<serde_json::Value>(&line) else { continue };
                        if v["event"] == "message" {
                            if let Some(id) = v["id"].as_str() {
                                since = id.to_string();
                            }
                            if let Some(m) = v["message"].as_str() {
                                f(m.as_bytes());
                            }
                        }
                    }
                    e
                }
                Ok(r) => Some(format!("relay answered {}", r.status())),
                Err(e) => Some(e.to_string()),
            };
            if stop.load(Ordering::Relaxed) {
                return;
            }
            if let Some(e) = err {
                (self.log)(format!("lost the room relay ({e}) - reconnecting"));
            }
            sleep_unless(&stop, backoff);
            if backoff < Duration::from_secs(30) {
                backoff *= 2;
            }
        }
    }
}

/// In-process relay used by tests and the built-in demo.
#[derive(Default)]
pub struct MemHub {
    inner: Mutex<MemHubInner>,
}

#[derive(Default)]
struct MemHubInner {
    msgs: HashMap<String, Vec<Vec<u8>>>,
    subs: HashMap<String, Vec<Sender<Vec<u8>>>>,
}

pub struct MemSignal {
    pub hub: Arc<MemHub>,
    pub topic: String,
}

impl Signaler for MemSignal {
    fn publish(&self, msg: &[u8]) -> Result<(), String> {
        let mut h = self.hub.inner.lock().unwrap();
        h.msgs.entry(self.topic.clone()).or_default().push(msg.to_vec());
        if let Some(subs) = h.subs.get_mut(&self.topic) {
            subs.retain(|s| s.send(msg.to_vec()).is_ok());
        }
        Ok(())
    }

    fn subscribe(&self, stop: Arc<AtomicBool>, f: &mut dyn FnMut(&[u8])) {
        let (tx, rx) = channel();
        let old = {
            let mut h = self.hub.inner.lock().unwrap();
            h.subs.entry(self.topic.clone()).or_default().push(tx);
            h.msgs.get(&self.topic).cloned().unwrap_or_default()
        };
        for m in old {
            f(&m);
        }
        while !stop.load(Ordering::Relaxed) {
            if let Ok(m) = rx.recv_timeout(Duration::from_millis(50)) {
                f(&m);
            }
        }
    }
}
