//! Audio in and out: 48 kHz, 20 ms frames. Mono from the mic, stereo to the speakers.

pub const RATE: usize = 48000;
/// samples per 20 ms
pub const FRAME: usize = 960;

/// A mic or output the user can pick. An empty id means the system default.
#[derive(Clone, Debug, PartialEq)]
pub struct AudioDevice {
    pub id: String,
    pub name: String,
}

pub trait MicSource: Send {
    /// Blocks until a full frame is captured.
    fn read(&mut self, frame: &mut [i16]) -> Result<(), String>;
}

pub trait SpeakerSink: Send {
    /// Takes one interleaved stereo frame; blocks to pace playback.
    fn write(&mut self, stereo: &[i16]) -> Result<(), String>;
}

pub mod fake;

#[cfg(all(target_os = "linux", not(feature = "fakeaudio")))]
mod linux;
#[cfg(all(target_os = "linux", not(feature = "fakeaudio")))]
pub use linux::{list_mics, list_speakers, open_mic, open_speaker};

#[cfg(all(windows, not(feature = "fakeaudio")))]
mod win;
#[cfg(all(windows, not(feature = "fakeaudio")))]
pub use win::{list_mics, list_speakers, open_mic, open_speaker};

#[cfg(any(feature = "fakeaudio", not(any(target_os = "linux", windows))))]
pub use fake::{list_mics, list_speakers, open_mic, open_speaker};
