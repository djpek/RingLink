//! Voice codec: Opus at 48 kHz mono.
//!
//! Full quality: 20 ms frames at ~32 kbit/s with in-band error correction, so a lost
//! packet can be rebuilt from the next one. Low quality (for people too far away to
//! hear you in big rooms, who only need it for their replays): 60 ms frames at
//! ~12 kbit/s, about a third of the upload.

use crate::audio::FRAME;
use opus::{Application, Bitrate, Channels, Decoder, Encoder};

pub const LOW_FRAMES: usize = 3;
pub const FULL_BITRATE: i32 = 32000;
pub const LOW_BITRATE: i32 = 12000;

pub struct VoiceEncoder {
    enc: Encoder,
    buf: [u8; 1500],
}

impl VoiceEncoder {
    pub fn new(bitrate: i32) -> Result<Self, String> {
        let mut enc = Encoder::new(48000, Channels::Mono, Application::Voip).map_err(|e| e.to_string())?;
        enc.set_bitrate(Bitrate::Bits(bitrate)).map_err(|e| e.to_string())?;
        let _ = enc.set_inband_fec(true);
        let _ = enc.set_packet_loss_perc(10);
        Ok(Self { enc, buf: [0; 1500] })
    }

    /// Encodes 20 ms (or 60 ms for the low-quality stream) of audio.
    pub fn encode(&mut self, pcm: &[i16]) -> Option<&[u8]> {
        let n = self.enc.encode(pcm, &mut self.buf).ok()?;
        Some(&self.buf[..n])
    }
}

pub struct VoiceDecoder {
    dec: Decoder,
}

impl VoiceDecoder {
    pub fn new() -> Result<Self, String> {
        Ok(Self { dec: Decoder::new(48000, Channels::Mono).map_err(|e| e.to_string())? })
    }

    /// Decodes one packet into 20 ms frames (one for full quality, three for low).
    pub fn decode(&mut self, pkt: &[u8]) -> Vec<Vec<i16>> {
        let mut out = vec![0i16; FRAME * LOW_FRAMES];
        match self.dec.decode(pkt, &mut out, false) {
            Ok(n) if n > 0 && n % FRAME == 0 => out[..n].chunks(FRAME).map(|c| c.to_vec()).collect(),
            _ => vec![],
        }
    }

    /// Rebuilds the 20 ms frame before `next` from the error-correction data inside it.
    pub fn recover(&mut self, next: &[u8]) -> Option<Vec<i16>> {
        let mut out = vec![0i16; FRAME];
        match self.dec.decode(next, &mut out, true) {
            Ok(n) if n == FRAME => Some(out),
            _ => None,
        }
    }

    /// Guesses a lost 20 ms frame (packet loss concealment).
    pub fn conceal(&mut self) -> Option<Vec<i16>> {
        let mut out = vec![0i16; FRAME];
        match self.dec.decode(&[], &mut out, false) {
            Ok(n) if n == FRAME => Some(out),
            _ => None,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn tone(n: usize, start: usize) -> Vec<i16> {
        (0..n).map(|i| (8000.0 * (std::f64::consts::TAU * 220.0 * (start + i) as f64 / 48000.0).sin()) as i16).collect()
    }

    fn rms(x: &[i16]) -> f64 {
        (x.iter().map(|v| (*v as f64).powi(2)).sum::<f64>() / x.len() as f64).sqrt()
    }

    #[test]
    fn full_quality_round_trip_and_loss() {
        let mut enc = VoiceEncoder::new(FULL_BITRATE).unwrap();
        let mut dec = VoiceDecoder::new().unwrap();
        let mut pkts = Vec::new();
        for i in 0..30 {
            pkts.push(enc.encode(&tone(FRAME, i * FRAME)).unwrap().to_vec());
        }
        assert!(pkts.iter().all(|p| p.len() < 200), "20 ms at 32 kbit/s is about 80 bytes");
        let mut last = vec![];
        for (i, p) in pkts.iter().enumerate() {
            if i == 20 {
                continue; // lost
            }
            if i == 21 {
                let fr = dec.recover(p).or_else(|| dec.conceal()).unwrap();
                assert_eq!(fr.len(), FRAME);
            }
            let frs = dec.decode(p);
            assert_eq!(frs.len(), 1);
            last = frs[0].clone();
        }
        let r = rms(&last);
        assert!(r > 3000.0 && r < 8000.0, "tone survives: rms {r}");
    }

    #[test]
    fn low_quality_is_three_frames() {
        let mut enc = VoiceEncoder::new(LOW_BITRATE).unwrap();
        let mut dec = VoiceDecoder::new().unwrap();
        let mut frs = vec![];
        for i in 0..5 {
            let p = enc.encode(&tone(FRAME * LOW_FRAMES, i * FRAME * LOW_FRAMES)).unwrap().to_vec();
            assert!(p.len() < 200);
            frs = dec.decode(&p);
            assert_eq!(frs.len(), LOW_FRAMES);
        }
        assert!(rms(&frs[2]) > 2000.0);
        assert!(dec.decode(&[1, 2, 3, 4, 5, 6, 7, 8, 9]).len() <= LOW_FRAMES); // garbage doesn't panic
    }
}
