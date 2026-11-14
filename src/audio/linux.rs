//! Linux audio through the desktop's sound server command-line tools, so no C libraries
//! are needed: PipeWire/PulseAudio (parec/pacat, pw-record/pw-play) or plain ALSA
//! (arecord/aplay). Device lists come from `pactl`.

use super::*;
use std::io::{Read, Write};
use std::os::fd::AsRawFd;
use std::process::{Child, ChildStdin, ChildStdout, Command, Stdio};
use std::time::{Duration, Instant};

/// headroom so a busy CPU (the game) doesn't make it crackle
const PLAY_AHEAD: Duration = Duration::from_millis(120);

struct ProcMic {
    child: Child,
    out: ChildStdout,
    buf: Vec<u8>,
}

struct ProcSpeaker {
    child: Child,
    inp: ChildStdin,
    buf: Vec<u8>,
    // playback pacing: never let more than PLAY_AHEAD of audio queue up in the pipe and
    // the player, otherwise the pipe fills and every voice lags behind
    start: Option<Instant>,
    written: u64,
}

impl Drop for ProcMic {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl Drop for ProcSpeaker {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn pipe_backlog(fd: i32) -> usize {
    let mut n: libc::c_int = 0;
    if unsafe { libc::ioctl(fd, libc::FIONREAD, &mut n) } == 0 && n > 0 {
        n as usize
    } else {
        0
    }
}

fn shrink_pipe(fd: i32) {
    unsafe {
        libc::fcntl(fd, libc::F_SETPIPE_SZ, 4096);
    }
}

impl MicSource for ProcMic {
    fn read(&mut self, frame: &mut [i16]) -> Result<(), String> {
        self.buf.resize(frame.len() * 2, 0);
        // drop mic audio that piled up (e.g. after a stall) so we always send the newest sound
        let n = pipe_backlog(self.out.as_raw_fd());
        if n > 3 * self.buf.len() {
            let mut drop = (n - self.buf.len()) / self.buf.len() * self.buf.len();
            let mut sink = [0u8; 4096];
            while drop > 0 {
                let k = drop.min(sink.len());
                self.out.read_exact(&mut sink[..k]).map_err(|e| e.to_string())?;
                drop -= k;
            }
        }
        self.out.read_exact(&mut self.buf).map_err(|e| e.to_string())?;
        for (i, s) in frame.iter_mut().enumerate() {
            *s = i16::from_le_bytes([self.buf[2 * i], self.buf[2 * i + 1]]);
        }
        Ok(())
    }
}

impl SpeakerSink for ProcSpeaker {
    fn write(&mut self, stereo: &[i16]) -> Result<(), String> {
        self.buf.clear();
        for s in stereo {
            self.buf.extend_from_slice(&s.to_le_bytes());
        }
        let now = Instant::now();
        let start = *self.start.get_or_insert(now);
        let played = Duration::from_micros(self.written * 1_000_000 / RATE as u64);
        let elapsed = now - start;
        if elapsed > played + Duration::from_millis(200) {
            // we fell behind (stall): restart the clock instead of bursting
            self.start = Some(now);
            self.written = 0;
        } else if played > elapsed + PLAY_AHEAD {
            std::thread::sleep(played - elapsed - PLAY_AHEAD);
        }
        self.written += (stereo.len() / 2) as u64;
        self.inp.write_all(&self.buf).map_err(|e| e.to_string())
    }
}

fn mic_cmds() -> Vec<Vec<String>> {
    let r = RATE.to_string();
    vec![
        vec!["parec".into(), "--raw".into(), "--format=s16le".into(), format!("--rate={r}"), "--channels=1".into(), "--latency-msec=20".into(), "--client-name=RingLink".into(), "--stream-name=Voice chat mic".into()],
        vec!["pw-record".into(), "--rate".into(), r.clone(), "--channels".into(), "1".into(), "--format".into(), "s16".into(), "--latency".into(), "20ms".into(), "-".into()],
        vec!["arecord".into(), "-q".into(), "-t".into(), "raw".into(), "-f".into(), "S16_LE".into(), "-r".into(), r, "-c".into(), "1".into(), "--buffer-time=40000".into()],
    ]
}

fn spk_cmds() -> Vec<Vec<String>> {
    let r = RATE.to_string();
    vec![
        vec!["pacat".into(), "--playback".into(), "--raw".into(), "--format=s16le".into(), format!("--rate={r}"), "--channels=2".into(), "--latency-msec=80".into(), "--client-name=RingLink".into(), "--stream-name=Voice chat".into()],
        vec!["pw-play".into(), "--rate".into(), r.clone(), "--channels".into(), "2".into(), "--format".into(), "s16".into(), "--latency".into(), "80ms".into(), "-".into()],
        vec!["aplay".into(), "-q".into(), "-t".into(), "raw".into(), "-f".into(), "S16_LE".into(), "-r".into(), r, "-c".into(), "2".into(), "--buffer-time=80000".into()],
    ]
}

fn start_proc(cands: Vec<Vec<String>>, capture: bool, device: &str) -> Result<(Child, String), String> {
    let mut last = "no audio tool found (install pulseaudio-utils, pipewire or alsa-utils)".to_string();
    for c in cands {
        let mut args: Vec<String> = c[1..].to_vec();
        if !device.is_empty() {
            match c[0].as_str() {
                "parec" | "pacat" => args.push(format!("--device={device}")),
                "pw-record" | "pw-play" => {
                    args.insert(0, device.to_string());
                    args.insert(0, "--target".into());
                }
                _ => {}
            }
        }
        let mut cmd = Command::new(&c[0]);
        cmd.args(&args).stderr(Stdio::piped());
        if capture {
            cmd.stdin(Stdio::null()).stdout(Stdio::piped());
        } else {
            cmd.stdin(Stdio::piped()).stdout(Stdio::null());
        }
        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => {
                if e.kind() != std::io::ErrorKind::NotFound {
                    last = format!("{}: {e}", c[0]);
                }
                continue;
            }
        };
        // make sure the tool actually works (it exits at once when there's no sound server)
        std::thread::sleep(Duration::from_millis(400));
        if let Ok(Some(_)) = child.try_wait() {
            let mut err = String::new();
            if let Some(mut e) = child.stderr.take() {
                let _ = e.read_to_string(&mut err);
            }
            last = format!("{}: {}", c[0], err.trim());
            continue;
        }
        // nobody reads stderr from here on: don't let a chatty tool block on it
        drop(child.stderr.take());
        return Ok((child, c[0].clone()));
    }
    Err(last)
}

pub fn open_mic(device: &str) -> Result<(Box<dyn MicSource>, String), String> {
    let (mut child, name) = start_proc(mic_cmds(), true, device)?;
    let out = child.stdout.take().ok_or("no pipe")?;
    shrink_pipe(out.as_raw_fd()); // small pipe = little audio can hide in it
    Ok((Box::new(ProcMic { child, out, buf: vec![] }), name))
}

pub fn open_speaker(device: &str) -> Result<(Box<dyn SpeakerSink>, String), String> {
    let (mut child, name) = start_proc(spk_cmds(), false, device)?;
    let inp = child.stdin.take().ok_or("no pipe")?;
    shrink_pipe(inp.as_raw_fd());
    Ok((Box::new(ProcSpeaker { child, inp, buf: vec![], start: None, written: 0 }), name))
}

pub fn list_mics() -> Vec<AudioDevice> {
    pactl_list("sources", true)
}

pub fn list_speakers() -> Vec<AudioDevice> {
    pactl_list("sinks", false)
}

fn pactl_list(kind: &str, skip_monitors: bool) -> Vec<AudioDevice> {
    match Command::new("pactl").args(["list", kind]).env("LC_ALL", "C").output() {
        Ok(o) if o.status.success() => parse_pactl(&String::from_utf8_lossy(&o.stdout), skip_monitors),
        _ => vec![],
    }
}

pub(crate) fn parse_pactl(out: &str, skip_monitors: bool) -> Vec<AudioDevice> {
    let mut devs = Vec::new();
    let mut cur: Option<(String, String)> = None;
    let mut flush = |cur: &mut Option<(String, String)>| {
        if let Some((id, name)) = cur.take() {
            if !id.is_empty() && !(skip_monitors && id.ends_with(".monitor")) {
                devs.push(AudioDevice { name: if name.is_empty() { id.clone() } else { name }, id });
            }
        }
    };
    for line in out.lines() {
        let t = line.trim();
        if line.starts_with("Source #") || line.starts_with("Sink #") {
            flush(&mut cur);
            cur = Some((String::new(), String::new()));
        } else if let Some(c) = cur.as_mut() {
            if let Some(v) = t.strip_prefix("Name: ") {
                if c.0.is_empty() {
                    c.0 = v.to_string();
                }
            } else if let Some(v) = t.strip_prefix("Description: ") {
                if c.1.is_empty() {
                    c.1 = v.to_string();
                }
            }
        }
    }
    flush(&mut cur);
    devs
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pactl_parsing() {
        let out = "Sink #52\n\tState: RUNNING\n\tName: alsa_output.usb-headset\n\tDescription: Headset\n\tProperties:\n\t\tName: nope\nSink #60\n\tName: bare\n";
        let d = parse_pactl(out, false);
        assert_eq!(d, vec![AudioDevice { id: "alsa_output.usb-headset".into(), name: "Headset".into() }, AudioDevice { id: "bare".into(), name: "bare".into() }]);
        let out = "Source #1\n\tName: x.monitor\n\tDescription: Monitor of X\nSource #2\n\tName: mic\n\tDescription: Mic\n";
        assert_eq!(parse_pactl(out, true).len(), 1);
    }
}
