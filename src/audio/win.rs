//! Windows audio through WASAPI (cpal). The device's own sample rate and channel count
//! are used and converted to/from RingLink's 48 kHz here.

use super::*;
use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use std::collections::VecDeque;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::sync_channel;
use std::sync::{Arc, Condvar, Mutex};
use std::time::Duration;

struct Shared {
    q: Mutex<VecDeque<f32>>,
    cv: Condvar,
    stop: AtomicBool,
    err: Mutex<Option<String>>,
}

impl Shared {
    fn new() -> Arc<Self> {
        Arc::new(Self { q: Mutex::new(VecDeque::new()), cv: Condvar::new(), stop: AtomicBool::new(false), err: Mutex::new(None) })
    }
}

/// Linear resampler for a mono stream.
struct Resampler {
    step: f64,
    pos: f64,
    last: f32,
}

impl Resampler {
    fn new(from: f64, to: f64) -> Self {
        Self { step: from / to, pos: 0.0, last: 0.0 }
    }
    fn push(&mut self, input: &[f32], out: &mut impl FnMut(f32)) {
        // positions are relative to `last` (index -1) followed by input[0..]
        let at = |i: isize, last: f32| if i < 0 { last } else { input[i as usize] };
        let n = input.len() as f64;
        while self.pos < n {
            let k = self.pos.floor();
            let t = (self.pos - k) as f32;
            let a = at(k as isize - 1, self.last);
            let b = at(k as isize, self.last);
            out(a + (b - a) * t);
            self.pos += self.step;
        }
        self.pos -= n;
        if let Some(l) = input.last() {
            self.last = *l;
        }
    }
}

pub struct WinMic {
    sh: Arc<Shared>,
}

pub struct WinSpeaker {
    sh: Arc<Shared>,
    /// device frames (per channel) we keep queued at most
    max_queue: usize,
    rs_l: Resampler,
    rs_r: Resampler,
    tmp_l: Vec<f32>,
    tmp_r: Vec<f32>,
}

impl Drop for WinMic {
    fn drop(&mut self) {
        self.sh.stop.store(true, Ordering::SeqCst);
        self.sh.cv.notify_all();
    }
}

impl Drop for WinSpeaker {
    fn drop(&mut self) {
        self.sh.stop.store(true, Ordering::SeqCst);
        self.sh.cv.notify_all();
    }
}

fn find_device(input: bool, id: &str) -> Result<cpal::Device, String> {
    let host = cpal::default_host();
    if !id.is_empty() {
        let devs = if input { host.input_devices() } else { host.output_devices() };
        if let Ok(mut devs) = devs {
            if let Some(d) = devs.find(|d| d.name().map(|n| n == id).unwrap_or(false)) {
                return Ok(d);
            }
        }
    }
    let d = if input { host.default_input_device() } else { host.default_output_device() };
    d.ok_or_else(|| if input { "no microphone found".to_string() } else { "no sound output found".to_string() })
}

fn to_f32(fmt: cpal::SampleFormat) -> bool {
    matches!(fmt, cpal::SampleFormat::F32 | cpal::SampleFormat::I16 | cpal::SampleFormat::U16)
}

pub fn open_mic(device: &str) -> Result<(Box<dyn MicSource>, String), String> {
    let sh = Shared::new();
    let sh2 = sh.clone();
    let device = device.to_string();
    let (tx, rx) = sync_channel::<Result<String, String>>(1);
    // cpal streams can't move between threads: the stream lives on its own thread
    std::thread::spawn(move || {
        let res = (|| -> Result<(cpal::Stream, String), String> {
            let dev = find_device(true, &device)?;
            let name = dev.name().unwrap_or_else(|_| "microphone".into());
            let cfg = dev.default_input_config().map_err(|e| e.to_string())?;
            if !to_f32(cfg.sample_format()) {
                return Err(format!("unsupported sample format {:?}", cfg.sample_format()));
            }
            let ch = cfg.channels() as usize;
            let rate = cfg.sample_rate().0 as f64;
            let mut rs = Resampler::new(rate, RATE as f64);
            let mut mono: Vec<f32> = Vec::new();
            let sh = sh2.clone();
            let mut feed = move |mono: &[f32]| {
                let mut q = sh.q.lock().unwrap();
                rs.push(mono, &mut |s| q.push_back(s));
                // keep at most ~200 ms so we always send the newest sound
                let extra = q.len().saturating_sub(RATE / 5);
                q.drain(..extra);
                drop(q);
                sh.cv.notify_all();
            };
            let she = sh2.clone();
            let on_err = move |e: cpal::StreamError| {
                *she.err.lock().unwrap() = Some(e.to_string());
                she.cv.notify_all();
            };
            let sc: cpal::StreamConfig = cfg.clone().into();
            let stream = match cfg.sample_format() {
                cpal::SampleFormat::F32 => dev.build_input_stream(
                    &sc,
                    move |d: &[f32], _: &_| {
                        mono.clear();
                        mono.extend(d.chunks(ch).map(|c| c.iter().sum::<f32>() / ch as f32));
                        feed(&mono);
                    },
                    on_err,
                    None,
                ),
                cpal::SampleFormat::I16 => dev.build_input_stream(
                    &sc,
                    move |d: &[i16], _: &_| {
                        mono.clear();
                        mono.extend(d.chunks(ch).map(|c| c.iter().map(|x| *x as f32 / 32768.0).sum::<f32>() / ch as f32));
                        feed(&mono);
                    },
                    on_err,
                    None,
                ),
                _ => dev.build_input_stream(
                    &sc,
                    move |d: &[u16], _: &_| {
                        mono.clear();
                        mono.extend(d.chunks(ch).map(|c| c.iter().map(|x| (*x as f32 - 32768.0) / 32768.0).sum::<f32>() / ch as f32));
                        feed(&mono);
                    },
                    on_err,
                    None,
                ),
            }
            .map_err(|e| e.to_string())?;
            stream.play().map_err(|e| e.to_string())?;
            Ok((stream, name))
        })();
        match res {
            Ok((stream, name)) => {
                let _ = tx.send(Ok(name));
                while !sh2.stop.load(Ordering::Relaxed) {
                    std::thread::sleep(Duration::from_millis(100));
                }
                drop(stream);
            }
            Err(e) => {
                let _ = tx.send(Err(e));
            }
        }
    });
    let name = rx.recv_timeout(Duration::from_secs(10)).map_err(|_| "the microphone didn't answer".to_string())??;
    Ok((Box::new(WinMic { sh }), name))
}

impl MicSource for WinMic {
    fn read(&mut self, frame: &mut [i16]) -> Result<(), String> {
        let mut q = self.sh.q.lock().unwrap();
        loop {
            if let Some(e) = self.sh.err.lock().unwrap().take() {
                return Err(e);
            }
            if q.len() >= frame.len() {
                break;
            }
            let (g, to) = self.sh.cv.wait_timeout(q, Duration::from_secs(2)).unwrap();
            q = g;
            if to.timed_out() && q.len() < frame.len() {
                return Err("no sound is coming from the microphone".into());
            }
        }
        for s in frame.iter_mut() {
            *s = (q.pop_front().unwrap().clamp(-1.0, 1.0) * 32767.0) as i16;
        }
        Ok(())
    }
}

pub fn open_speaker(device: &str) -> Result<(Box<dyn SpeakerSink>, String), String> {
    let sh = Shared::new();
    let sh2 = sh.clone();
    let device = device.to_string();
    let (tx, rx) = sync_channel::<Result<(String, f64), String>>(1);
    std::thread::spawn(move || {
        let res = (|| -> Result<(cpal::Stream, String, f64), String> {
            let dev = find_device(false, &device)?;
            let name = dev.name().unwrap_or_else(|_| "speakers".into());
            let cfg = dev.default_output_config().map_err(|e| e.to_string())?;
            if !to_f32(cfg.sample_format()) {
                return Err(format!("unsupported sample format {:?}", cfg.sample_format()));
            }
            let ch = cfg.channels() as usize;
            let rate = cfg.sample_rate().0 as f64;
            let sh = sh2.clone();
            // the queue holds interleaved L,R at the device rate
            let pull = move |n: usize, put: &mut dyn FnMut(usize, f32, f32)| {
                let mut q = sh.q.lock().unwrap();
                for i in 0..n {
                    let (l, r) = if q.len() >= 2 { (q.pop_front().unwrap(), q.pop_front().unwrap()) } else { (0.0, 0.0) };
                    put(i, l, r);
                }
                drop(q);
                sh.cv.notify_all();
            };
            let she = sh2.clone();
            let on_err = move |e: cpal::StreamError| {
                *she.err.lock().unwrap() = Some(e.to_string());
                she.cv.notify_all();
            };
            let sc: cpal::StreamConfig = cfg.clone().into();
            let stream = match cfg.sample_format() {
                cpal::SampleFormat::F32 => dev.build_output_stream(
                    &sc,
                    move |d: &mut [f32], _: &_| {
                        let n = d.len() / ch;
                        pull(n, &mut |i, l, r| {
                            for c in 0..ch {
                                d[i * ch + c] = if ch == 1 { (l + r) * 0.5 } else if c == 0 { l } else if c == 1 { r } else { 0.0 };
                            }
                        });
                    },
                    on_err,
                    None,
                ),
                cpal::SampleFormat::I16 => dev.build_output_stream(
                    &sc,
                    move |d: &mut [i16], _: &_| {
                        let n = d.len() / ch;
                        pull(n, &mut |i, l, r| {
                            for c in 0..ch {
                                let v = if ch == 1 { (l + r) * 0.5 } else if c == 0 { l } else if c == 1 { r } else { 0.0 };
                                d[i * ch + c] = (v.clamp(-1.0, 1.0) * 32767.0) as i16;
                            }
                        });
                    },
                    on_err,
                    None,
                ),
                _ => dev.build_output_stream(
                    &sc,
                    move |d: &mut [u16], _: &_| {
                        let n = d.len() / ch;
                        pull(n, &mut |i, l, r| {
                            for c in 0..ch {
                                let v = if ch == 1 { (l + r) * 0.5 } else if c == 0 { l } else if c == 1 { r } else { 0.0 };
                                d[i * ch + c] = ((v.clamp(-1.0, 1.0) * 32767.0) + 32768.0) as u16;
                            }
                        });
                    },
                    on_err,
                    None,
                ),
            }
            .map_err(|e| e.to_string())?;
            stream.play().map_err(|e| e.to_string())?;
            Ok((stream, name, rate))
        })();
        match res {
            Ok((stream, name, rate)) => {
                let _ = tx.send(Ok((name, rate)));
                while !sh2.stop.load(Ordering::Relaxed) {
                    std::thread::sleep(Duration::from_millis(100));
                }
                drop(stream);
            }
            Err(e) => {
                let _ = tx.send(Err(e));
            }
        }
    });
    let (name, rate) = rx.recv_timeout(Duration::from_secs(10)).map_err(|_| "the sound output didn't answer".to_string())??;
    Ok((
        Box::new(WinSpeaker {
            sh,
            max_queue: (rate * 0.10) as usize,
            rs_l: Resampler::new(RATE as f64, rate),
            rs_r: Resampler::new(RATE as f64, rate),
            tmp_l: vec![],
            tmp_r: vec![],
        }),
        name,
    ))
}

impl SpeakerSink for WinSpeaker {
    fn write(&mut self, stereo: &[i16]) -> Result<(), String> {
        let l: Vec<f32> = stereo.chunks(2).map(|c| c[0] as f32 / 32768.0).collect();
        let r: Vec<f32> = stereo.chunks(2).map(|c| c[1] as f32 / 32768.0).collect();
        self.tmp_l.clear();
        self.tmp_r.clear();
        let (tl, tr) = (&mut self.tmp_l, &mut self.tmp_r);
        self.rs_l.push(&l, &mut |s| tl.push(s));
        self.rs_r.push(&r, &mut |s| tr.push(s));
        let mut q = self.sh.q.lock().unwrap();
        // pace: wait while more than ~100 ms is already queued
        let mut waited = 0;
        while q.len() / 2 > self.max_queue {
            if let Some(e) = self.sh.err.lock().unwrap().take() {
                return Err(e);
            }
            let (g, to) = self.sh.cv.wait_timeout(q, Duration::from_millis(500)).unwrap();
            q = g;
            if to.timed_out() {
                waited += 1;
                if waited >= 4 {
                    return Err("the sound output stopped taking audio".into());
                }
            }
        }
        for i in 0..self.tmp_l.len().min(self.tmp_r.len()) {
            q.push_back(self.tmp_l[i]);
            q.push_back(self.tmp_r[i]);
        }
        Ok(())
    }
}

fn list(input: bool) -> Vec<AudioDevice> {
    let host = cpal::default_host();
    let devs = if input { host.input_devices() } else { host.output_devices() };
    let mut out = Vec::new();
    if let Ok(devs) = devs {
        for d in devs {
            if let Ok(n) = d.name() {
                out.push(AudioDevice { id: n.clone(), name: n });
            }
        }
    }
    out
}

pub fn list_mics() -> Vec<AudioDevice> {
    list(true)
}

pub fn list_speakers() -> Vec<AudioDevice> {
    list(false)
}
