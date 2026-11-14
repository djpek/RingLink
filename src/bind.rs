//! Key and wheel-button bindings (push-to-broadcast, push to talk, push to mute) that
//! work while the game has focus.
//!
//! Linux: wheel / joystick buttons through the joystick devices (/dev/input/js*), which
//! normal users can read, plus keyboard keys from the X server. Wayland sessions get no
//! keyboard keys; wheel buttons still work.
//! Windows: keys through GetAsyncKeyState, wheel buttons through the joystick API.

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

/// everyone hears you at full volume while held
pub const ACT_BROADCAST: &str = "broadcast";
/// push to talk
pub const ACT_PTT: &str = "ptt";
/// push to mute
pub const ACT_PTM: &str = "ptm";
/// toggle your mic mute
pub const ACT_MUTE: &str = "mute";
pub const ACTIONS: [&str; 4] = [ACT_BROADCAST, ACT_PTT, ACT_PTM, ACT_MUTE];

type ChangeCb = Box<dyn Fn(&str, bool) + Send + Sync>;
type CaptureCb = Box<dyn Fn(&str, &str) + Send + Sync>;

#[derive(Default)]
struct BState {
    /// action -> button id, e.g. "joy0:b5", "xkey:38:a", "key:66"
    binds: HashMap<String, String>,
    /// action waiting for its next button press
    cap_for: Option<String>,
    cap_base: Option<HashSet<String>>,
    cap_until: Option<Instant>,
    held: HashMap<String, bool>,
}

#[derive(Default)]
pub struct Binder {
    st: Mutex<BState>,
    on_change: Mutex<Option<ChangeCb>>,
    on_capture: Mutex<Option<CaptureCb>>,
    pub keyboard_ok: AtomicBool,
}

impl Binder {
    pub fn new() -> Arc<Self> {
        Arc::new(Self::default())
    }

    pub fn set_on_change(&self, f: impl Fn(&str, bool) + Send + Sync + 'static) {
        *self.on_change.lock().unwrap() = Some(Box::new(f));
    }

    pub fn set_on_capture(&self, f: impl Fn(&str, &str) + Send + Sync + 'static) {
        *self.on_capture.lock().unwrap() = Some(Box::new(f));
    }

    pub fn bind(&self, action: &str) -> String {
        self.st.lock().unwrap().binds.get(action).cloned().unwrap_or_default()
    }

    pub fn set_bind(&self, action: &str, id: &str) {
        let mut s = self.st.lock().unwrap();
        if id.is_empty() {
            s.binds.remove(action);
        } else {
            s.binds.insert(action.to_string(), id.to_string());
        }
    }

    /// The action waiting for a button press, if any.
    pub fn capturing_for(&self) -> Option<String> {
        let s = self.st.lock().unwrap();
        s.cap_for.clone().filter(|_| s.cap_until.map(|t| Instant::now() < t).unwrap_or(false))
    }

    /// The next button pressed (within 8 s) becomes `action`'s binding.
    pub fn capture(&self, action: &str) {
        let mut s = self.st.lock().unwrap();
        s.cap_for = Some(action.to_string());
        s.cap_base = None;
        s.cap_until = Some(Instant::now() + Duration::from_secs(8));
    }

    pub fn cancel_capture(&self) {
        self.st.lock().unwrap().cap_for = None;
    }

    /// Handles one poll of what's pressed right now.
    pub fn step(&self, down: &HashSet<String>) {
        let mut captured: Option<(String, String)> = None;
        let mut changes: Vec<(String, bool)> = Vec::new();
        {
            let mut g = self.st.lock().unwrap();
            let s = &mut *g;
            if let Some(act) = s.cap_for.clone() {
                if s.cap_until.map(|t| Instant::now() > t).unwrap_or(true) {
                    s.cap_for = None;
                } else if s.cap_base.is_none() {
                    s.cap_base = Some(down.clone()); // ignore whatever was already held (e.g. the mouse click)
                } else {
                    let base = s.cap_base.as_mut().unwrap();
                    let mut fresh: Vec<&String> = down.iter().filter(|id| !base.contains(*id)).collect();
                    base.retain(|id| down.contains(id));
                    if !fresh.is_empty() {
                        fresh.sort();
                        let id = fresh[0].clone();
                        s.binds.insert(act.clone(), id.clone());
                        s.cap_for = None;
                        s.cap_base = Some([id.clone()].into_iter().collect()); // don't fire until it's let go
                        captured = Some((act, id));
                    }
                }
            }
            for act in ACTIONS {
                let held = match s.binds.get(act) {
                    Some(id) => {
                        s.cap_for.is_none() && down.iter().any(|d| same_button(d, id)) && !s.cap_base.as_ref().map(|b| b.iter().any(|d| same_button(d, id))).unwrap_or(false)
                    }
                    None => false,
                };
                if held != s.held.get(act).copied().unwrap_or(false) {
                    s.held.insert(act.to_string(), held);
                    changes.push((act.to_string(), held));
                }
            }
            if s.cap_for.is_none() {
                if let Some(b) = s.cap_base.as_mut() {
                    b.retain(|id| down.contains(id));
                    if b.is_empty() {
                        s.cap_base = None;
                    }
                }
            }
        }
        if let Some((act, id)) = captured {
            if let Some(f) = self.on_capture.lock().unwrap().as_ref() {
                f(&act, &id);
            }
        }
        for (act, held) in changes {
            if let Some(f) = self.on_change.lock().unwrap().as_ref() {
                f(&act, held);
            }
        }
    }

    /// Polls the keyboard and controllers until `stop` is set.
    pub fn run(self: &Arc<Self>, stop: Arc<AtomicBool>) {
        let me = self.clone();
        std::thread::spawn(move || {
            let mut input = platform::Poller::new();
            me.keyboard_ok.store(input.keyboard_ok(), Ordering::Relaxed);
            let mut down = HashSet::new();
            while !stop.load(Ordering::Relaxed) {
                std::thread::sleep(Duration::from_millis(15));
                down.clear();
                input.pressed(&mut down);
                me.step(&down);
            }
        });
    }
}

/// X keys carry their name after the key code ("xkey:38:a"); bindings saved by older
/// versions don't ("xkey:38"). Both mean the same key.
fn same_button(a: &str, b: &str) -> bool {
    fn core(s: &str) -> &str {
        match s.strip_prefix("xkey:") {
            Some(rest) => &s[..5 + rest.find(':').unwrap_or(rest.len())],
            None => s,
        }
    }
    core(a) == core(b)
}

/// Turns a button id into something readable.
pub fn bind_name(id: &str) -> String {
    if id.is_empty() {
        return "Not set".into();
    }
    if let Some(rest) = id.strip_prefix("joy") {
        if let Some((d, b)) = rest.split_once(":b") {
            if let (Ok(d), Ok(b)) = (d.parse::<u32>(), b.parse::<u32>()) {
                return format!("Controller {} button {}", d + 1, b + 1);
            }
        }
    }
    if let Some(rest) = id.strip_prefix("xkey:") {
        let mut it = rest.splitn(2, ':');
        let kc = it.next().unwrap_or("");
        return match it.next() {
            Some(n) if !n.is_empty() => x_name(n),
            _ => format!("Key {kc}"),
        };
    }
    if let Some(vk) = id.strip_prefix("key:").and_then(|v| v.parse::<u32>().ok()) {
        return key_name(vk);
    }
    id.to_string()
}

pub fn key_name(vk: u32) -> String {
    match vk {
        0x41..=0x5a | 0x30..=0x39 => return format!("Key {}", char::from_u32(vk).unwrap()),
        0x70..=0x87 => return format!("F{}", vk - 0x6f),
        0x60..=0x69 => return format!("Numpad {}", vk - 0x60),
        _ => {}
    }
    let n = match vk {
        0x04 => "Middle mouse",
        0x05 => "Mouse 4",
        0x06 => "Mouse 5",
        0x08 => "Backspace",
        0x09 => "Tab",
        0x0d => "Enter",
        0x10 => "Shift",
        0x11 => "Ctrl",
        0x12 => "Alt",
        0x14 => "Caps Lock",
        0x1b => "Esc",
        0x20 => "Space",
        0x21 => "Page Up",
        0x22 => "Page Down",
        0x23 => "End",
        0x24 => "Home",
        0x25 => "Left",
        0x26 => "Up",
        0x27 => "Right",
        0x28 => "Down",
        0x2d => "Insert",
        0x2e => "Delete",
        0xa0 => "Left Shift",
        0xa1 => "Right Shift",
        0xa2 => "Left Ctrl",
        0xa3 => "Right Ctrl",
        0xa4 => "Left Alt",
        0xa5 => "Right Alt",
        0xc0 => "` key",
        0xbd => "- key",
        0xbb => "= key",
        0xdb => "[ key",
        0xdd => "] key",
        0xdc => "\\ key",
        0xba => "; key",
        0xde => "' key",
        0xbc => ", key",
        0xbe => ". key",
        0xbf => "/ key",
        0x6a => "Numpad *",
        0x6b => "Numpad +",
        0x6d => "Numpad -",
        0x6e => "Numpad .",
        0x6f => "Numpad /",
        _ => return format!("Key {vk}"),
    };
    n.to_string()
}

/// Tidies X keysym names: "a" -> "Key A", "Control_L" -> "Left Ctrl", "KP_1" -> "Numpad 1".
pub fn x_name(n: &str) -> String {
    if n.chars().count() == 1 {
        return format!("Key {}", n.to_uppercase());
    }
    let (side, n) = if let Some(b) = n.strip_suffix("_L") {
        ("Left ", b)
    } else if let Some(b) = n.strip_suffix("_R") {
        ("Right ", b)
    } else {
        ("", n)
    };
    let n = match n {
        "Control" => "Ctrl".to_string(),
        "Super" => "Windows key".to_string(),
        "Prior" => "Page Up".to_string(),
        "Next" => "Page Down".to_string(),
        "Return" => "Enter".to_string(),
        "Escape" => "Esc".to_string(),
        "grave" => "` key".to_string(),
        "space" => "Space".to_string(),
        _ => match n.strip_prefix("KP_") {
            Some(k) => format!("Numpad {k}"),
            None => n.to_string(),
        },
    };
    format!("{side}{n}")
}

#[cfg(target_os = "linux")]
mod platform {
    use std::collections::{HashMap, HashSet};
    use std::io::Read;
    use std::sync::{Arc, Mutex};
    use std::time::{Duration, Instant};

    struct X {
        lib: x11_dl::xlib::Xlib,
        dpy: *mut x11_dl::xlib::Display,
        names: HashMap<u8, String>,
    }

    pub struct Poller {
        x: Option<X>,
        js_down: Arc<Mutex<HashSet<String>>>,
        js_open: Arc<Mutex<HashSet<String>>>,
        check: Option<Instant>,
    }

    impl Poller {
        pub fn new() -> Self {
            // RINGLINK_NO_XKEYS=1 skips the keyboard (for running without a desktop)
            let x = if std::env::var("RINGLINK_NO_XKEYS").is_ok() {
                None
            } else {
                x11_dl::xlib::Xlib::open().ok().and_then(|lib| {
                    let dpy = unsafe { (lib.XOpenDisplay)(std::ptr::null()) };
                    (!dpy.is_null()).then(|| X { lib, dpy, names: HashMap::new() })
                })
            };
            Self { x, js_down: Default::default(), js_open: Default::default(), check: None }
        }

        pub fn keyboard_ok(&self) -> bool {
            self.x.is_some()
        }

        pub fn pressed(&mut self, out: &mut HashSet<String>) {
            if self.check.map(|t| t.elapsed() > Duration::from_secs(2)).unwrap_or(true) {
                self.check = Some(Instant::now());
                self.scan_joysticks();
            }
            if let Some(x) = self.x.as_mut() {
                let mut km = [0 as std::os::raw::c_char; 32];
                unsafe { (x.lib.XQueryKeymap)(x.dpy, km.as_mut_ptr()) };
                for (i, b) in km.iter().enumerate() {
                    let b = *b as u8;
                    if b == 0 {
                        continue;
                    }
                    for bit in 0..8 {
                        if b & (1 << bit) != 0 {
                            let kc = (i * 8 + bit) as u8;
                            let name = x.names.entry(kc).or_insert_with(|| unsafe {
                                let ks = (x.lib.XkbKeycodeToKeysym)(x.dpy, kc, 0, 0);
                                if ks == 0 {
                                    return String::new();
                                }
                                let p = (x.lib.XKeysymToString)(ks);
                                if p.is_null() {
                                    String::new()
                                } else {
                                    std::ffi::CStr::from_ptr(p).to_string_lossy().into_owned()
                                }
                            });
                            out.insert(format!("xkey:{kc}:{name}"));
                        }
                    }
                }
            }
            out.extend(self.js_down.lock().unwrap().iter().cloned());
        }

        fn scan_joysticks(&mut self) {
            let Ok(rd) = std::fs::read_dir("/dev/input") else { return };
            for e in rd.filter_map(|e| e.ok()) {
                let name = e.file_name().to_string_lossy().into_owned();
                let Some(n) = name.strip_prefix("js").and_then(|n| n.parse::<u32>().ok()) else { continue };
                if self.js_open.lock().unwrap().contains(&name) {
                    continue;
                }
                let Ok(mut f) = std::fs::File::open(e.path()) else { continue };
                self.js_open.lock().unwrap().insert(name.clone());
                let (down, open) = (self.js_down.clone(), self.js_open.clone());
                std::thread::spawn(move || {
                    // js_event: u32 time, s16 value, u8 type, u8 number; type 1 = button (0x80 = initial state)
                    let mut ev = [0u8; 8];
                    while f.read_exact(&mut ev).is_ok() {
                        if ev[6] & !0x80 != 1 {
                            continue;
                        }
                        let id = format!("joy{n}:b{}", ev[7]);
                        let pressed = i16::from_le_bytes([ev[4], ev[5]]) != 0;
                        let mut d = down.lock().unwrap();
                        if pressed {
                            d.insert(id);
                        } else {
                            d.remove(&id);
                        }
                    }
                    // unplugged
                    open.lock().unwrap().remove(&name);
                    let prefix = format!("joy{n}:");
                    down.lock().unwrap().retain(|k| !k.starts_with(&prefix));
                });
            }
        }
    }
}

#[cfg(windows)]
mod platform {
    use std::collections::HashSet;
    use windows_sys::Win32::Media::Multimedia::{joyGetPosEx, JOYINFOEX, JOY_RETURNBUTTONS};
    use windows_sys::Win32::UI::Input::KeyboardAndMouse::GetAsyncKeyState;

    pub struct Poller {
        tick: u32,
        present: [bool; 16],
    }

    impl Poller {
        pub fn new() -> Self {
            Self { tick: 0, present: [true; 16] }
        }

        pub fn keyboard_ok(&self) -> bool {
            true
        }

        pub fn pressed(&mut self, out: &mut HashSet<String>) {
            // keys and extra mouse buttons (not left / right click, and not the generic
            // Shift / Ctrl / Alt codes: the left / right ones are reported instead)
            for vk in 4u32..=0xfe {
                if matches!(vk, 0x10 | 0x11 | 0x12) {
                    continue;
                }
                if unsafe { GetAsyncKeyState(vk as i32) } as u16 & 0x8000 != 0 {
                    out.insert(format!("key:{vk}"));
                }
            }
            // controllers that aren't plugged in are slow to ask about: retry those every ~2 s
            self.tick = self.tick.wrapping_add(1);
            let rescan = self.tick % 128 == 0;
            for j in 0..16u32 {
                if !self.present[j as usize] && !rescan {
                    continue;
                }
                let mut info: JOYINFOEX = unsafe { std::mem::zeroed() };
                info.dwSize = std::mem::size_of::<JOYINFOEX>() as u32;
                info.dwFlags = JOY_RETURNBUTTONS as u32;
                let ok = unsafe { joyGetPosEx(j, &mut info) } == 0;
                self.present[j as usize] = ok;
                if ok {
                    for b in 0..32 {
                        if info.dwButtons & (1 << b) != 0 {
                            out.insert(format!("joy{j}:b{b}"));
                        }
                    }
                }
            }
        }
    }
}

#[cfg(not(any(target_os = "linux", windows)))]
mod platform {
    pub struct Poller;
    impl Poller {
        pub fn new() -> Self {
            Poller
        }
        pub fn keyboard_ok(&self) -> bool {
            false
        }
        pub fn pressed(&mut self, _: &mut std::collections::HashSet<String>) {}
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn set(ids: &[&str]) -> HashSet<String> {
        ids.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn capture_then_hold() {
        let b = Binder::new();
        let log: Arc<Mutex<Vec<String>>> = Default::default();
        let l = log.clone();
        b.set_on_change(move |a, h| l.lock().unwrap().push(format!("{a}={h}")));
        let l = log.clone();
        b.set_on_capture(move |a, id| l.lock().unwrap().push(format!("cap {a} {id}")));

        b.capture(ACT_BROADCAST);
        assert_eq!(b.capturing_for().as_deref(), Some(ACT_BROADCAST));
        b.step(&set(&["key:1"])); // whatever is already down is ignored
        b.step(&set(&["key:1", "joy0:b5"]));
        assert_eq!(b.bind(ACT_BROADCAST), "joy0:b5");
        assert!(b.capturing_for().is_none());
        // still held from the capture press: doesn't fire until let go
        b.step(&set(&["joy0:b5"]));
        b.step(&set(&[]));
        b.step(&set(&["joy0:b5"]));
        b.step(&set(&[]));
        assert_eq!(*log.lock().unwrap(), vec!["cap broadcast joy0:b5", "broadcast=true", "broadcast=false"]);
        // a binding saved by the old app (no key name) still matches
        b.set_bind(ACT_PTT, "xkey:38");
        b.step(&set(&["xkey:38:a"]));
        assert_eq!(log.lock().unwrap().last().unwrap(), "ptt=true");
        b.step(&set(&["xkey:380:a"]));
        assert_eq!(log.lock().unwrap().last().unwrap(), "ptt=false");
        log.lock().unwrap().truncate(3);
        b.set_bind(ACT_BROADCAST, "");
        b.step(&set(&["joy0:b5"]));
        assert_eq!(log.lock().unwrap().len(), 3);
    }

    #[test]
    fn readable_names() {
        assert_eq!(bind_name(""), "Not set");
        assert_eq!(bind_name("joy0:b18"), "Controller 1 button 19");
        assert_eq!(bind_name("xkey:38:a"), "Key A");
        assert_eq!(bind_name("xkey:37:Control_L"), "Left Ctrl");
        assert_eq!(bind_name("xkey:87:KP_1"), "Numpad 1");
        assert_eq!(bind_name("xkey:99:"), "Key 99");
        assert_eq!(bind_name("key:66"), "Key B");
        assert_eq!(bind_name("key:113"), "F2");
        assert_eq!(bind_name("key:162"), "Left Ctrl");
    }
}
