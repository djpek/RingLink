//! RingLink: proximity voice chat for Le Mans Ultimate and BeamNG.drive.

pub mod app;
pub mod audio;
pub mod bind;
pub mod codec;
pub mod codes;
pub mod demo;
pub mod games;
pub mod motion;
pub mod replay;
pub mod roles;
pub mod room;
pub mod signal;
pub mod stun;
pub mod ui;
pub mod voice;

#[cfg(test)]
mod e2e_tests;
