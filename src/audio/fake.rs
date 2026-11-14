//! Fake audio devices for UI screenshots and tests (`--features fakeaudio`, or
//! RINGLINK_FAKE_AUDIO=1 at run time): the "mic" says something every few seconds, the
//! "speakers" discard audio.

use super::*;
use std::time::Duration;

pub struct FakeMic {
    n: usize,
}

impl MicSource for FakeMic {
    fn read(&mut self, f: &mut [i16]) -> Result<(), String> {
        std::thread::sleep(Duration::from_millis(20));
        let t = self.n as f64 / RATE as f64;
        let on = t % 4.0 < 1.6; // talk 1.6 s out of every 4 s
        for (i, s) in f.iter_mut().enumerate() {
            let tt = (self.n + i) as f64 / RATE as f64;
            *s = if on { (6000.0 * (std::f64::consts::TAU * 180.0 * tt).sin() * (0.6 + 0.4 * (std::f64::consts::TAU * 3.0 * tt).sin())) as i16 } else { 0 };
        }
        self.n += f.len();
        Ok(())
    }
}

pub struct FakeSpeaker;

impl SpeakerSink for FakeSpeaker {
    fn write(&mut self, _: &[i16]) -> Result<(), String> {
        std::thread::sleep(Duration::from_millis(20));
        Ok(())
    }
}

pub fn open_mic(_: &str) -> Result<(Box<dyn MicSource>, String), String> {
    Ok((Box::new(FakeMic { n: 0 }), "test mic".into()))
}

pub fn open_speaker(_: &str) -> Result<(Box<dyn SpeakerSink>, String), String> {
    Ok((Box::new(FakeSpeaker), "test output".into()))
}

pub fn list_mics() -> Vec<AudioDevice> {
    vec![AudioDevice { id: "usb-mic".into(), name: "USB Microphone".into() }, AudioDevice { id: "headset-mic".into(), name: "Headset Microphone".into() }]
}

pub fn list_speakers() -> Vec<AudioDevice> {
    vec![AudioDevice { id: "speakers".into(), name: "Speakers".into() }, AudioDevice { id: "headset".into(), name: "Headset".into() }]
}
